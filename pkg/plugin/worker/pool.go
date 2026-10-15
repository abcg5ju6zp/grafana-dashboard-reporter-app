package worker

import (
	"context"
	"runtime"
	"sync"
)

type Pool struct {
	ctx           context.Context
	ctxCancelFunc context.CancelFunc
	queue         chan func()
	workers       sync.WaitGroup
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

	pool := &Pool{
		ctx:           ctx,
		ctxCancelFunc: cancel,
		queue:         queue,
	}

	pool.workers.Add(maxWorker)

	for range maxWorker {
		go func() {
			defer pool.workers.Done()

			for {
				select {
				case f := <-queue:
					f()
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	return pool
}

func (w *Pool) Do(f func()) {
	w.queue <- f
}

// Ping enqueues a no-op task and waits until a worker picks it up. It is used
// during a configuration generation build to verify the worker capacity is
// actually available before the generation is allowed to serve reports.
func (w *Pool) Ping(ctx context.Context) error {
	done := make(chan struct{})

	w.queue <- func() {
		close(done)
	}

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Done signals the worker goroutines to stop. It does not wait for a task
// already running to finish, so that closing a browser that the task is
// blocked on is not deadlocked.
func (w *Pool) Done() {
	w.ctxCancelFunc()
}

// WaitStopped blocks until every worker goroutine has exited after Done (or
// after the pool context was canceled). It is intended for orderly test
// shutdown; production generation shutdown uses Done alone and bounds waiting
// at the manager level.
func (w *Pool) WaitStopped() {
	w.workers.Wait()
}

// Stopped reports whether the pool has been shut down with Done.
func (w *Pool) Stopped() bool {
	return w.ctx.Err() != nil
}
