package worker

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
)

// ErrPoolStopped is returned by Do when the pool has been stopped during
// plugin instance disposal.
var ErrPoolStopped = errors.New("worker pool stopped")

type Pool struct {
	ctx           context.Context
	ctxCancelFunc context.CancelFunc
	queue         chan func()
	wg            sync.WaitGroup
	// stopped is set only after every worker has exited. It prevents sends
	// into the buffered queue once nobody drains it anymore.
	stopped atomic.Bool
}

type Pools map[string]*Pool

const (
	Browser  = "browser"
	Renderer = "renderer"
)

func New(ctx context.Context, maxWorker int) *Pool {
	if maxWorker <= 0 {
		maxWorker = runtime.NumCPU()
	}

	queue := make(chan func(), maxWorker)
	ctx, cancel := context.WithCancel(ctx)

	w := &Pool{
		ctx:           ctx,
		ctxCancelFunc: cancel,
		queue:         queue,
	}

	for range maxWorker {
		w.wg.Go(func() {
			for {
				select {
				case f := <-queue:
					f()
				case <-ctx.Done():
					// Drain and execute work already accepted into the queue.
					// The tasks run with their own (now canceled) request
					// contexts and therefore fail fast, while callers' wait
					// groups are always released, so nothing deadlocks.
					for {
						select {
						case f := <-queue:
							f()
						default:
							return
						}
					}
				}
			}
		})
	}

	return w
}

// Do enqueues f for execution. It fails if the request context is canceled
// (for instance during instance disposal) or if the pool has been stopped, so
// callers can fail consistently instead of blocking forever.
func (w *Pool) Do(ctx context.Context, f func()) error {
	if w.stopped.Load() {
		return ErrPoolStopped
	}

	select {
	case w.queue <- f:
	case <-ctx.Done():
		return ctx.Err()
	case <-w.ctx.Done():
		return ErrPoolStopped
	}

	// The send may have raced the final shutdown drain. If all workers have
	// exited, the task can no longer run.
	if w.stopped.Load() {
		return ErrPoolStopped
	}

	return nil
}

// Done stops accepting new work, lets workers finish or drain already queued
// work, and waits until every worker has exited.
func (w *Pool) Done() {
	w.ctxCancelFunc()
	w.wg.Wait()
	w.stopped.Store(true)
}
