package provider

import (
	"context"
	"errors"
	"sync"

	"github.com/vincentsch/maildo/internal/errs"
)

// errBoundedOrderedCoordinatorCanceled marks the cancel that RunBoundedOrdered
// sends to the other workers after one fails, so their own "canceled" errors
// are not reported as the failure.
var errBoundedOrderedCoordinatorCanceled = errors.New("bounded_ordered_coordinator_canceled")

type boundedOrderedFailure struct {
	ordinal int
	err     error
	panic   any
}

// RunBoundedOrdered runs work on every input with at most concurrency
// goroutines and returns the outputs in input order. After the first error
// it stops handing out new inputs and returns the error of the earliest
// failed input. A panic in work is re-raised in the caller.
func RunBoundedOrdered[I, O any](ctx context.Context, concurrency int, inputs []I, work func(context.Context, int, I) (O, error)) ([]O, error) {
	if ctx == nil || concurrency <= 0 || work == nil {
		return nil, errs.NewSharedProviderContractInvalid()
	}
	outputs := make([]O, len(inputs))
	if len(inputs) == 0 {
		return outputs, nil
	}
	if concurrency > len(inputs) {
		concurrency = len(inputs)
	}

	child, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	jobs := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr *boundedOrderedFailure
	var firstPanic *boundedOrderedFailure

	recordErr := func(ordinal int, err error) {
		if err == nil {
			return
		}
		mu.Lock()
		if firstErr == nil || ordinal < firstErr.ordinal {
			firstErr = &boundedOrderedFailure{ordinal: ordinal, err: err}
		}
		mu.Unlock()
		cancel(errBoundedOrderedCoordinatorCanceled)
	}
	recordPanic := func(ordinal int, value any) {
		mu.Lock()
		if firstPanic == nil || ordinal < firstPanic.ordinal {
			firstPanic = &boundedOrderedFailure{ordinal: ordinal, panic: value}
		}
		mu.Unlock()
		cancel(errBoundedOrderedCoordinatorCanceled)
	}

	for worker := 0; worker < concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ordinal := range jobs {
				func() {
					defer func() {
						if p := recover(); p != nil {
							recordPanic(ordinal, p)
						}
					}()
					out, err := work(child, ordinal, inputs[ordinal])
					if err != nil {
						if causedByBoundedOrderedCoordinator(child, err) {
							return
						}
						recordErr(ordinal, err)
						return
					}
					outputs[ordinal] = out
				}()
			}
		}()
	}

dispatch:
	for ordinal := range inputs {
		select {
		case <-child.Done():
			break dispatch
		case jobs <- ordinal:
		}
	}
	close(jobs)
	wg.Wait()

	mu.Lock()
	panicFailure := firstPanic
	errFailure := firstErr
	mu.Unlock()

	if panicFailure != nil {
		panic(panicFailure.panic)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if errFailure != nil {
		return nil, errFailure.err
	}
	return outputs, nil
}

func causedByBoundedOrderedCoordinator(ctx context.Context, err error) bool {
	if !errors.Is(context.Cause(ctx), errBoundedOrderedCoordinatorCanceled) {
		return false
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, errBoundedOrderedCoordinatorCanceled)
}
