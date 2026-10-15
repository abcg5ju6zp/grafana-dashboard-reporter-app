package plugin

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/httpclient"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/chrome"
	plugin_config "github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/config"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/worker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// closeWaitTimeout is how long tests wait for a generation to close.
const closeWaitTimeout = time.Second

// fakeChrome is a chrome.Instance double for generation manager tests.
type fakeChrome struct {
	name     string
	readyErr error

	mu      sync.Mutex
	closedV bool
	closeCh chan struct{}
}

func newFakeChrome(name string, readyErr error) *fakeChrome {
	return &fakeChrome{
		name:     name,
		readyErr: readyErr,
		closeCh:  make(chan struct{}),
	}
}

func (f *fakeChrome) NewTab(_ log.Logger, _ *plugin_config.Config) *chrome.Tab { return nil }

func (f *fakeChrome) Ready(_ context.Context, _ log.Logger) error { return f.readyErr }

func (f *fakeChrome) Name() string { return f.name }

func (f *fakeChrome) Close(_ log.Logger) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closedV {
		return
	}

	f.closedV = true
	close(f.closeCh)
}

func (f *fakeChrome) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.closedV
}

func (f *fakeChrome) requireClosed(t *testing.T) {
	t.Helper()

	select {
	case <-f.closeCh:
	case <-time.After(closeWaitTimeout):
		t.Fatalf("chrome instance %q was not closed within %s", f.name, closeWaitTimeout)
	}
}

func (f *fakeChrome) requireOpen(t *testing.T, duration time.Duration) {
	t.Helper()

	select {
	case <-f.closeCh:
		t.Fatalf("chrome instance %q was closed while it was supposed to stay open", f.name)
	case <-time.After(duration):
	}
}

// countingTransport records CloseIdleConnections calls.
type countingTransport struct {
	idleClosed atomic.Int32
}

func (c *countingTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	return nil, errors.New("fake transport has no network")
}

func (c *countingTransport) CloseIdleConnections() {
	c.idleClosed.Add(1)
}

// stubFactories builds observable fake resources and records every created one.
type stubFactories struct {
	httpErr   error
	chromeErr error
	readyErr  error
	stopPools bool

	mu      sync.Mutex
	chromes []*fakeChrome
	clients []*countingTransport
	pools   []*worker.Pool
}

func (s *stubFactories) factories() resourceFactories {
	return resourceFactories{
		newHTTPClient: func(_ httpclient.Options) (*http.Client, error) {
			if s.httpErr != nil {
				return nil, s.httpErr
			}

			transport := &countingTransport{}

			s.mu.Lock()
			s.clients = append(s.clients, transport)
			s.mu.Unlock()

			return &http.Client{Transport: transport}, nil
		},
		newChrome: func(_ log.Logger, _ plugin_config.Config) (chrome.Instance, error) {
			if s.chromeErr != nil {
				return nil, s.chromeErr
			}

			browser := newFakeChrome("local", s.readyErr)

			s.mu.Lock()
			s.chromes = append(s.chromes, browser)
			s.mu.Unlock()

			return browser, nil
		},
		newPool: func(maxWorkers int) *worker.Pool {
			pool := worker.New(context.Background(), maxWorkers)

			s.mu.Lock()
			s.pools = append(s.pools, pool)
			s.mu.Unlock()

			if s.stopPools {
				// Stop and wait for every worker to exit before any readiness
				// task is enqueued, so Ping deterministically times out.
				pool.Done()
				pool.WaitStopped()
			}

			return pool
		},
	}
}

func newStubManager(t *testing.T, factories *stubFactories) *Manager {
	t.Helper()

	manager := newManagerWithFactories(log.NewNullLogger(), factories.factories())

	// Close generations created during the test (worker pools, HTTP clients
	// and fake browsers) once the test finishes.
	t.Cleanup(func() {
		manager.Shutdown(context.Background())
	})

	return manager
}

// 1. A generation serves reports only after config, HTTP client, browser and
// worker capacity are all ready; concurrent reports without updates all use the
// same generation (compatibility).
func TestManagerActivateAndConcurrentLeases(t *testing.T) {
	t.Parallel()

	stubs := &stubFactories{}
	manager := newStubManager(t, stubs)

	// No generation yet.
	_, err := manager.Lease()
	require.ErrorIs(t, err, ErrServiceUnavailable)

	settings := backend.AppInstanceSettings{
		JSONData: []byte(`{"theme":"dark"}`),
	}

	gen1, err := manager.Activate(t.Context(), settings)
	require.NoError(t, err)
	require.NotZero(t, gen1.id)
	require.NotNil(t, gen1.httpClient)
	require.NotNil(t, gen1.chromeInstance)
	require.Contains(t, gen1.workerPools, worker.Browser)
	require.Contains(t, gen1.workerPools, worker.Renderer)
	// Config from settings is honored.
	assert.Equal(t, "dark", gen1.conf.Theme)

	// Concurrent reports all lease the same, still-open generation.
	const n = 50

	var (
		wg     sync.WaitGroup
		errMu  sync.Mutex
		errs   []error
		leases = make([]*Lease, n)
	)

	for i := range n {
		wg.Go(func() {
			lease, leaseErr := manager.Lease()
			if leaseErr != nil {
				errMu.Lock()

				errs = append(errs, leaseErr)
				errMu.Unlock()

				return
			}

			if lease.Generation() != gen1 {
				errMu.Lock()

				errs = append(errs, errors.New("report leased an unexpected generation"))
				errMu.Unlock()
			}

			leases[i] = lease
		})
	}

	wg.Wait()
	require.Empty(t, errs)

	for _, lease := range leases {
		lease.Release()
	}

	assert.False(t, stubs.chromes[0].isClosed())
}

// 2. Any initialization failure closes the resources created for that attempt
// and the previous generation keeps serving; there is no switch to a broken
// generation.
func TestManagerActivateFailureKeepsCurrentGeneration(t *testing.T) {
	t.Parallel()

	stubs := &stubFactories{}
	manager := newStubManager(t, stubs)

	gen1, err := manager.Activate(t.Context(), backend.AppInstanceSettings{})
	require.NoError(t, err)

	lease1, err := manager.Lease()
	require.NoError(t, err)

	defer lease1.Release()

	// Failure while creating the browser: the HTTP client of the attempt must
	// be closed, the active generation must be untouched.
	stubs.chromeErr = errors.New("cannot start browser")

	_, err = manager.Activate(t.Context(), backend.AppInstanceSettings{})
	require.Error(t, err)

	stubs.chromeErr = nil

	require.Len(t, stubs.clients, 2)
	assert.Equal(t, int32(1), stubs.clients[1].idleClosed.Load(), "failed attempt's HTTP client must be closed")
	assert.Equal(t, int32(0), stubs.clients[0].idleClosed.Load(), "active generation's HTTP client must stay open")
	assert.False(t, stubs.chromes[0].isClosed())

	// Browser constructs but fails the readiness check: the browser of the
	// attempt is closed, active generation still serves.
	stubs.readyErr = errors.New("browser not reachable")

	_, err = manager.Activate(t.Context(), backend.AppInstanceSettings{})
	require.Error(t, err)
	require.ErrorContains(t, err, "browser is not ready")

	stubs.readyErr = nil

	require.Len(t, stubs.chromes, 2)
	stubs.chromes[1].requireClosed(t)
	assert.Equal(t, int32(1), stubs.clients[2].idleClosed.Load(), "failed attempt's HTTP client must be closed")
	assert.False(t, stubs.chromes[0].isClosed())

	// New reports still land on the old (active) generation.
	leaseAgain, err := manager.Lease()
	require.NoError(t, err)
	assert.Same(t, gen1, leaseAgain.Generation())
	leaseAgain.Release()
}

// 3 & 4. After promotion the old generation receives no new leases and is kept
// open until reports holding leases finish.
func TestManagerDrainWaitsForLeases(t *testing.T) {
	t.Parallel()

	stubs := &stubFactories{}
	manager := newStubManager(t, stubs)

	gen1, err := manager.Activate(t.Context(), backend.AppInstanceSettings{})
	require.NoError(t, err)

	lease1, err := manager.Lease()
	require.NoError(t, err)

	gen2, err := manager.Activate(t.Context(), backend.AppInstanceSettings{})
	require.NoError(t, err)

	// New requests go to gen2; gen1 is draining but pinned open by lease1.
	lease2, err := manager.Lease()
	require.NoError(t, err)
	assert.Same(t, gen2, lease2.Generation())
	lease2.Release()

	assert.Same(t, gen1, lease1.Generation(), "a report keeps using the generation it entered on")
	stubs.chromes[0].requireOpen(t, 100*time.Millisecond)
	assert.False(t, stubs.chromes[1].isClosed())

	// Once the in-flight report returns its lease, gen1 closes; gen2 remains.
	lease1.Release()
	stubs.chromes[0].requireClosed(t)
	assert.False(t, stubs.chromes[1].isClosed())
}

// A single lease sees one immutable set of resources for its whole lifetime,
// even when a newer generation is promoted mid-report.
func TestLeaseResourcesAreStableAcrossPromotion(t *testing.T) {
	t.Parallel()

	stubs := &stubFactories{}
	manager := newStubManager(t, stubs)

	gen1, err := manager.Activate(t.Context(), backend.AppInstanceSettings{})
	require.NoError(t, err)

	lease, err := manager.Lease()
	require.NoError(t, err)

	gen2, err := manager.Activate(t.Context(), backend.AppInstanceSettings{})
	require.NoError(t, err)
	require.NotSame(t, gen1, gen2)

	used := lease.Generation()
	assert.Same(t, gen1.httpClient, used.httpClient)
	assert.Same(t, gen1.chromeInstance, used.chromeInstance)
	assert.Same(t, gen1.workerPools[worker.Browser], used.workerPools[worker.Browser])
	assert.Same(t, gen1.workerPools[worker.Renderer], used.workerPools[worker.Renderer])
	assert.Equal(t, gen1.conf, used.conf)

	lease.Release()
	stubs.chromes[0].requireClosed(t)
}

// 6. Updates while old generations are draining only affect the new candidate:
// neither the active generation nor generations used by in-flight reports are
// closed.
func TestManagerRepeatedUpdatesDuringDrain(t *testing.T) {
	t.Parallel()

	stubs := &stubFactories{}
	manager := newStubManager(t, stubs)

	gen1, err := manager.Activate(t.Context(), backend.AppInstanceSettings{})
	require.NoError(t, err)

	lease1, err := manager.Lease()
	require.NoError(t, err)

	gen2, err := manager.Activate(t.Context(), backend.AppInstanceSettings{})
	require.NoError(t, err)

	lease2, err := manager.Lease()
	require.NoError(t, err)
	assert.Same(t, gen2, lease2.Generation())

	gen3, err := manager.Activate(t.Context(), backend.AppInstanceSettings{})
	require.NoError(t, err)
	assert.NotSame(t, gen1, gen3)
	assert.NotSame(t, gen2, gen3)

	lease3, err := manager.Lease()
	require.NoError(t, err)
	assert.Same(t, gen3, lease3.Generation())
	lease3.Release()

	// Both previous generations are draining but pinned by held leases.
	assert.False(t, stubs.chromes[0].isClosed())
	assert.False(t, stubs.chromes[1].isClosed())
	assert.False(t, stubs.chromes[2].isClosed())

	// A failed update closes only its own candidate resources.
	stubs.readyErr = errors.New("candidate broken")

	_, err = manager.Activate(t.Context(), backend.AppInstanceSettings{})
	require.Error(t, err)

	stubs.readyErr = nil

	require.Len(t, stubs.chromes, 4)
	stubs.chromes[3].requireClosed(t)
	assert.False(t, stubs.chromes[0].isClosed(), "old generation used by a report must not be closed")
	assert.False(t, stubs.chromes[1].isClosed(), "active-ish generation used by a report must not be closed")
	assert.False(t, stubs.chromes[2].isClosed(), "active generation must not be closed")

	// Leases return in reverse order: each generation closes independently.
	lease2.Release()
	stubs.chromes[1].requireClosed(t)
	assert.False(t, stubs.chromes[0].isClosed())

	lease1.Release()
	stubs.chromes[0].requireClosed(t)
	assert.False(t, stubs.chromes[2].isClosed())
}

// Worker capacity that never becomes ready aborts the build and closes the
// browser and HTTP client already created for that generation.
func TestManagerWorkerNotReadyAbortsAndCleansUp(t *testing.T) {
	t.Parallel()

	stubs := &stubFactories{}
	manager := newStubManager(t, stubs)

	_, err := manager.Activate(t.Context(), backend.AppInstanceSettings{})
	require.NoError(t, err)

	stubs.mu.Lock()
	activeChrome := stubs.chromes[0]
	activeClient := stubs.clients[0]
	stubs.mu.Unlock()

	// Subsequent builds get worker pools that never process tasks.
	stubs.stopPools = true

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	_, err = manager.Activate(ctx, backend.AppInstanceSettings{})
	require.Error(t, err)
	require.ErrorContains(t, err, "worker pool")

	stubs.mu.Lock()
	require.Len(t, stubs.chromes, 2)
	require.Len(t, stubs.clients, 2)
	failedChrome := stubs.chromes[1]
	failedClient := stubs.clients[1]
	stubs.mu.Unlock()

	failedChrome.requireClosed(t)
	assert.Equal(t, int32(1), failedClient.idleClosed.Load())

	// Active generation survives.
	assert.False(t, activeChrome.isClosed())
	assert.Equal(t, int32(0), activeClient.idleClosed.Load())

	lease, err := manager.Lease()
	require.NoError(t, err)
	lease.Release()
}

// 7. Shutdown stops accepting new reports, bounds the wait for outstanding
// leases and then closes every generation's resources, including Chromium that
// was still in use by reports.
func TestManagerShutdownBoundedAndClosesAll(t *testing.T) {
	t.Parallel()

	stubs := &stubFactories{}
	manager := newStubManager(t, stubs)

	_, err := manager.Activate(t.Context(), backend.AppInstanceSettings{})
	require.NoError(t, err)

	// Report that never finishes within the grace period.
	stuckLease, err := manager.Lease()
	require.NoError(t, err)

	_, err = manager.Activate(t.Context(), backend.AppInstanceSettings{})
	require.NoError(t, err)

	// Shutdown waits bounded and force-closes the generation with the stuck
	// lease as well as the active one.
	start := time.Now()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	manager.Shutdown(shutdownCtx)
	cancel()

	assert.Less(t, time.Since(start), 2*time.Second, "shutdown must be bounded")

	for _, browser := range stubs.chromes {
		browser.requireClosed(t)
	}

	// No new reports or updates after shutdown.
	_, err = manager.Lease()
	require.ErrorIs(t, err, ErrServiceUnavailable)

	_, err = manager.Activate(t.Context(), backend.AppInstanceSettings{})
	require.ErrorIs(t, err, errManagerShuttingDown)

	// Idempotent and safe when the stuck report finally "finishes".
	manager.Shutdown(context.Background())
	stuckLease.Release()
}

// 6b. If a second settings update completes while the first update is still
// building its resources (for example a slow Chromium start), the first build
// is a stale candidate: it is discarded and closed, while the active
// generation and generations still used by reports are untouched.
func TestManagerOverlappingUpdatesDiscardStaleCandidate(t *testing.T) {
	t.Parallel()

	stubs := &stubFactories{}
	base := stubs.factories()

	build2Entered := make(chan struct{})
	allowBuild2 := make(chan struct{})

	var builds atomic.Int32

	base.newChrome = func(logger log.Logger, conf plugin_config.Config) (chrome.Instance, error) {
		if builds.Add(1) == 2 {
			close(build2Entered)
			<-allowBuild2
		}

		return stubs.factories().newChrome(logger, conf)
	}

	manager := newManagerWithFactories(log.NewNullLogger(), base)

	t.Cleanup(func() {
		manager.Shutdown(context.Background())
	})

	gen1, err := manager.Activate(t.Context(), backend.AppInstanceSettings{})
	require.NoError(t, err)

	// A report entered before any update pins gen1 open.
	lease1, err := manager.Lease()
	require.NoError(t, err)

	type activateResult struct {
		gen *Generation
		err error
	}

	resultCh := make(chan activateResult, 1)

	go func() {
		g, activateErr := manager.Activate(t.Context(), backend.AppInstanceSettings{})
		resultCh <- activateResult{g, activateErr}
	}()

	<-build2Entered

	// Newer update completes while the first build is still blocked.
	gen3, err := manager.Activate(t.Context(), backend.AppInstanceSettings{})
	require.NoError(t, err)

	close(allowBuild2)

	stale := <-resultCh
	require.NoError(t, stale.err)
	assert.Same(t, gen3, stale.gen, "stale candidate adopts the already promoted generation")

	require.Len(t, stubs.chromes, 3)

	// The superseded build is the only chrome that belongs to neither gen1
	// (draining, pinned by lease1) nor gen3 (active): it never served and must
	// be closed.
	gen1Chrome, ok := gen1.chromeInstance.(*fakeChrome)
	require.True(t, ok)

	gen3Chrome, ok := gen3.chromeInstance.(*fakeChrome)
	require.True(t, ok)

	var staleChrome *fakeChrome

	for _, browser := range stubs.chromes {
		if browser != gen1Chrome && browser != gen3Chrome {
			staleChrome = browser
		}
	}

	require.NotNil(t, staleChrome)
	staleChrome.requireClosed(t)
	// Active gen3 and draining gen1 (pinned by lease1) stay open.
	assert.False(t, gen1Chrome.isClosed())
	assert.False(t, gen3Chrome.isClosed())
	assert.Same(t, gen3, manager.Active())

	lease, err := manager.Lease()
	require.NoError(t, err)
	assert.Same(t, gen3, lease.Generation())
	lease.Release()

	lease1.Release()
	gen1Chrome.requireClosed(t)
	assert.NotSame(t, gen1, gen3)
}

// Shutdown with no in-flight reports closes generations immediately, and
// generations that drained naturally are not closed twice.
func TestManagerShutdownAfterNaturalDrain(t *testing.T) {
	t.Parallel()

	stubs := &stubFactories{}
	manager := newStubManager(t, stubs)

	_, err := manager.Activate(t.Context(), backend.AppInstanceSettings{})
	require.NoError(t, err)

	lease, err := manager.Lease()
	require.NoError(t, err)

	_, err = manager.Activate(t.Context(), backend.AppInstanceSettings{})
	require.NoError(t, err)
	lease.Release()

	stubs.chromes[0].requireClosed(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	manager.Shutdown(ctx)

	stubs.chromes[1].requireClosed(t)
}
