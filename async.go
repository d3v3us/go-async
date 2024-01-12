package async

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	ErrPanicOccurred = errors.New("panic occurred")
	ErrIncomplete    = errors.New("incomplete")
	ErrCanceled      = errors.New("operation canceled")
)

type Func[In, Out any] func(context.Context, In) (Out, error)

type ChainablePromise[Out any] struct {
	val        Out
	err        error
	done       chan struct{}
	cancelFunc context.CancelFunc
}

func (cp *ChainablePromise[Out]) Get() (Out, error) {
	<-cp.done
	return cp.val, cp.err
}

func (cp *ChainablePromise[Out]) GetNow() (Out, error) {
	select {
	case <-cp.done:
		return cp.val, cp.err
	default:
		var zero Out
		return zero, ErrIncomplete
	}
}

func RunPromise[Out any](ctx context.Context, f Func[context.Context, Out]) *ChainablePromise[Out] {
	done := make(chan struct{})
	cp := &ChainablePromise[Out]{
		done: done,
	}
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				cp.err = ErrPanicOccurred
			}
		}()
		select {
		case <-ctx.Done():
			cp.err = ErrCanceled
		default:
			cp.val, cp.err = f(ctx, nil) // Pass nil as input for RunPromise
		}
	}()
	return cp
}
func ContinueWith[Out any](ctx context.Context, cp *ChainablePromise[Out], f Func[Out, Out]) *ChainablePromise[Out] {
	done := make(chan struct{})
	out := &ChainablePromise[Out]{
		done: done,
	}

	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			out.err = ErrCanceled
		default:
			val, _ := cp.GetNow() // Ignore the error from the previous Promise
			val2, err := f(ctx, val)
			out.val = val2
			out.err = err
		}
	}()

	return out
}

func (cp *ChainablePromise[Out]) Filter(predicate func(Out) bool) *ChainablePromise[Out] {
	return ContinueWith(context.Background(), cp, func(ctx context.Context, val Out) (Out, error) {
		if predicate(val) {
			return val, nil
		}
		return val, errors.New("filter condition not met")
	})
}

func (cp *ChainablePromise[Out]) Catch(fallback Func[Out, Out]) *ChainablePromise[Out] {
	done := make(chan struct{})
	out := &ChainablePromise[Out]{
		done: done,
	}

	go func() {
		defer close(done)
		select {
		case <-cp.done:
			// Do nothing; already closed
		default:
			val, err := cp.GetNow()
			if err != nil {
				val, _ = fallback(context.Background(), val)
			}
			out.val = val
			out.err = err
		}
	}()

	return out
}

func (cp *ChainablePromise[Out]) Finalize(finalizer func(Out, error)) *ChainablePromise[Out] {
	done := make(chan struct{})
	out := &ChainablePromise[Out]{
		done: done,
	}

	go func() {
		defer close(done)
		val, err := cp.GetNow()
		finalizer(val, err)
		out.val = val
		out.err = err
	}()

	return out
}

// Retry mechanism
func (cp *ChainablePromise[Out]) Retry(ctx context.Context, attempts int, delay time.Duration) *ChainablePromise[Out] {
	retryFunc := func(ctx context.Context, t Out) (Out, error) {
		for i := 0; i < attempts; i++ {
			val, err := cp.val, cp.err
			if err == nil {
				return val, nil
			}
			select {
			case <-ctx.Done():
				return cp.val, ErrCanceled
			case <-time.After(delay):
				// Retry logic here if needed
			}
		}
		return cp.val, cp.err
	}

	return ContinueWith(ctx, cp, retryFunc)
}

// Timeout operation
func (cp *ChainablePromise[Out]) Timeout(ctx context.Context, timeout time.Duration) *ChainablePromise[Out] {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return ContinueWith(ctx, cp, func(ctx context.Context, t Out) (Out, error) {
		return cp.val, cp.err
	})
}

// Any resolves when any of the promises in the slice is resolved.
func Any[Out any](ctx context.Context, promises ...*ChainablePromise[Out]) *ChainablePromise[Out] {
	var wg sync.WaitGroup
	out := make(chan struct{})

	for _, p := range promises {
		wg.Add(1)
		go func(cp *ChainablePromise[Out]) {
			defer wg.Done()
			select {
			case <-ctx.Done():
				close(out)
			case <-cp.done:
				close(out)
			}
		}(p)
	}

	go func() {
		wg.Wait()
		close(out)
	}()

	return &ChainablePromise[Out]{done: out}
}

// All resolves when all of the promises in the slice are resolved.
func All[Out any](ctx context.Context, promises ...*ChainablePromise[Out]) *ChainablePromise[Out] {
	var wg sync.WaitGroup
	out := make(chan struct{})

	for _, p := range promises {
		wg.Add(1)
		go func(cp *ChainablePromise[Out]) {
			defer wg.Done()
			select {
			case <-ctx.Done():
			case <-cp.done:
			}
		}(p)
	}

	go func() {
		wg.Wait()
		close(out)
	}()

	return &ChainablePromise[Out]{done: out}
}

// Race resolves when the first promise in the slice is resolved.
func Race[Out any](ctx context.Context, promises ...*ChainablePromise[Out]) *ChainablePromise[Out] {
	out := make(chan struct{})

	for _, p := range promises {
		go func(cp *ChainablePromise[Out]) {
			select {
			case <-ctx.Done():
			case <-cp.done:
				close(out)
			}
		}(p)
	}

	return &ChainablePromise[Out]{done: out}
}

func (cp *ChainablePromise[Out]) Continue(onSuccess func(Out) Out, onFailure func(error) Out) *ChainablePromise[Out] {
	done := make(chan struct{})
	out := &ChainablePromise[Out]{
		done: done,
	}

	go func() {
		defer close(done)
		val, err := cp.GetNow()
		if err != nil {
			out.val = onFailure(err)
		} else {
			out.val = onSuccess(val)
		}
	}()

	return out
}

func (cp *ChainablePromise[Out]) OnSuccess(onSuccess func(Out)) *ChainablePromise[Out] {
	return cp.Continue(func(val Out) Out {
		onSuccess(val)
		return val
	}, func(err error) Out {
		return cp.val
	})
}

func (cp *ChainablePromise[Out]) OnFailure(onFailure func(error)) *ChainablePromise[Out] {
	return cp.Continue(func(val Out) Out {
		return cp.val
	}, func(err error) Out {
		onFailure(err)
		return cp.val
	})
}

// Delay introduces a delay before resolving the promise.
func (cp *ChainablePromise[Out]) Delay(ctx context.Context, delay time.Duration) *ChainablePromise[Out] {
	out := &ChainablePromise[Out]{
		done: make(chan struct{}),
	}

	go func() {
		defer close(out.done)
		select {
		case <-time.After(delay):
			val, _ := cp.GetNow()
			out.val = val
		case <-ctx.Done():
			val, _ := cp.GetNow()
			out.val = val
			out.err = ErrCanceled
		case <-cp.done:
			val, _ := cp.GetNow()
			out.val = val
			out.err = ErrIncomplete
		}
	}()

	return out
}

// RetryWithDelay retries the promise with a specified delay between attempts.
func (cp *ChainablePromise[Out]) RetryWithDelay(ctx context.Context, attempts int, delay time.Duration) *ChainablePromise[Out] {
	retryFunc := func(ctx context.Context, t Out) (Out, error) {
		for i := 0; i < attempts; i++ {
			val, err := cp.val, cp.err
			if err == nil {
				return val, nil
			}
			select {
			case <-ctx.Done():
				return cp.val, ErrCanceled
			case <-time.After(delay):
				// Retry logic here if needed
			}
		}
		return cp.val, cp.err
	}

	return ContinueWith(ctx, cp, retryFunc)
}

// TimeoutWithFallback applies a timeout to the promise and provides a fallback value or function if the timeout is reached.
func (cp *ChainablePromise[Out]) TimeoutWithFallback(ctx context.Context, timeout time.Duration, fallbackValue Out) *ChainablePromise[Out] {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return ContinueWith(ctx, cp, func(ctx context.Context, t Out) (Out, error) {
		select {
		case <-ctx.Done():
			return fallbackValue, ErrCanceled
		case <-cp.done:
			return cp.val, cp.err
		}
	})
}

type BackoffFunc func(retryAttempt int) time.Duration

// ExponentialBackoff generates an exponential backoff duration based on the retry attempt.
func ExponentialBackoff(retryAttempt int) time.Duration {
	return time.Duration(1<<uint(retryAttempt)) * time.Millisecond
}

// LinearBackoff generates a linear backoff duration based on the retry attempt.
func LinearBackoff(retryAttempt int) time.Duration {
	return time.Duration(retryAttempt) * time.Millisecond
}

// Backoff applies a backoff strategy to the promise's retry attempts.
func (cp *ChainablePromise[Out]) Backoff(ctx context.Context, backoffFunc BackoffFunc) *ChainablePromise[Out] {
	backoffRetryFunc := func(ctx context.Context, t Out) (Out, error) {
		retryAttempt := 0
		for {
			val, err := cp.val, cp.err
			if err == nil {
				return val, nil
			}

			select {
			case <-ctx.Done():
				return cp.val, ErrCanceled
			case <-time.After(backoffFunc(retryAttempt)):
				// Retry logic here if needed
			}

			retryAttempt++
		}
	}

	return ContinueWith(ctx, cp, backoffRetryFunc)
}

func WithCancellation[Out any](ctx context.Context) *ChainablePromise[Out] {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	return &ChainablePromise[Out]{
		done:       done,
		cancelFunc: cancel,
	}
}

// Cancel cancels the ChainablePromise and releases associated resources.
func (cp *ChainablePromise[Out]) Cancel() {
	cp.cancelFunc()
	close(cp.done)
}
