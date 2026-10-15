package worker_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/worker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPool(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	pool := worker.New(ctx, 1)

	resultCh := make(chan int, 10)

	for i := range 10 {
		err := pool.Do(ctx, func() {
			resultCh <- i
		})
		require.NoError(t, err)
	}

	for i := range 10 {
		assert.Equal(t, i, <-resultCh)
	}
}

// When the request context is canceled, Do must unblock and report the
// cancellation instead of queuing work that never runs (which would deadlock
// the caller's wait group).
func TestPoolDoCanceledContext(t *testing.T) {
	t.Parallel()

	pool := worker.New(t.Context(), 1)

	// Occupy the single worker with a long running task...
	started := make(chan struct{})

	release := make(chan struct{})

	require.NoError(t, pool.Do(t.Context(), func() {
		close(started)
		<-release
	}))

	<-started

	// ...and fill the one-slot buffered queue, so the next Do blocks.
	require.NoError(t, pool.Do(t.Context(), func() {}))

	ctx, cancel := context.WithCancel(t.Context())

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := pool.Do(ctx, func() {})
	require.ErrorIs(t, err, context.Canceled)

	// Let the running task finish and drain/stop the pool
	close(release)
	pool.Done()
}

// Done must execute already queued work so callers' wait groups are always
// released and no goroutine deadlocks.
func TestPoolDoneDrainsQueuedWork(t *testing.T) {
	t.Parallel()

	pool := worker.New(t.Context(), 1)

	started := make(chan struct{})

	release := make(chan struct{})

	require.NoError(t, pool.Do(t.Context(), func() {
		close(started)
		<-release
	}))

	<-started

	var wg sync.WaitGroup

	wg.Add(1)

	require.NoError(t, pool.Do(t.Context(), func() {
		defer wg.Done()
	}))

	done := make(chan struct{})

	go func() {
		close(release)
		pool.Done()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Done did not return")
	}

	wg.Wait()

	// After shutdown Do rejects new work
	err := pool.Do(t.Context(), func() {})
	assert.ErrorIs(t, err, worker.ErrPoolStopped)
}
