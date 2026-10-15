package plugin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/httpclient"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/mahendrapaipuri/authlib/authz"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/chrome"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/config"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/worker"
)

const (
	// workerReadyTimeout bounds how long a generation build waits for each
	// freshly created worker pool to prove that it is processing tasks.
	workerReadyTimeout = 2 * time.Second
)

var (
	// ErrServiceUnavailable is returned when no generation can accept new
	// reports, for instance while the plugin process is shutting down.
	ErrServiceUnavailable = errors.New("report service is temporarily unavailable")

	// errManagerShuttingDown is returned by Activate when a new generation
	// cannot be promoted because the manager is already shutting down.
	errManagerShuttingDown = errors.New("plugin is shutting down")
)

// Generation is one complete, immutable group of resources derived from a
// single plugin configuration: the config itself, the HTTP client, the
// (local or remote) Chromium instance and the browser/renderer worker pools.
//
// A generation never changes after it has been promoted. Every report acquires
// a lease on the active generation at entry and uses exactly that generation's
// resources until the response has been written. A generation is closed only
// after it has been retired and all leases on it have been returned (or after
// the bounded wait at process shutdown).
type Generation struct {
	id     uint64
	logger log.Logger

	conf           config.Config
	httpClient     *http.Client
	chromeInstance chrome.Instance
	workerPools    worker.Pools

	// Cached authorization enforcement client for this generation. It is
	// (re)created when the service account token used by a request changes.
	authzMx     sync.Mutex
	authzClient authz.EnforcementClient
	saToken     string

	// leases tracks reports currently using the generation. It is mutated only
	// while the manager lock is held, so it cannot race with Wait during
	// retirement or shutdown.
	leases    sync.WaitGroup
	closeOnce sync.Once
}

// close releases every resource owned by the generation. It is idempotent.
// Resources are closed in reverse order of their dependency: worker pools are
// stopped first so that no new tab/HTTP work can be scheduled, then idle HTTP
// connections are dropped and finally the local or remote Chromium instance is
// shut down.
func (g *Generation) close() {
	g.closeOnce.Do(func() {
		g.logger.Info("closing plugin configuration generation", "generation", g.id)

		for name, pool := range g.workerPools {
			if pool != nil {
				g.logger.Debug("stopping worker pool", "generation", g.id, "pool", name)
				pool.Done()
			}
		}

		if g.httpClient != nil {
			g.httpClient.CloseIdleConnections()
		}

		if g.chromeInstance != nil {
			g.logger.Info("disposing chromium from plugin configuration generation", "generation", g.id)
			g.chromeInstance.Close(g.logger)
		}
	})
}

// Lease represents a report's claim on a single generation. The report must
// call Release once the response has been written; until then the generation
// (and therefore its config, HTTP client, worker pools and browser) is kept
// alive.
type Lease struct {
	gen  *Generation
	once sync.Once
}

// Generation returns the generation the lease was acquired on.
func (l *Lease) Generation() *Generation {
	return l.gen
}

// Release returns the lease. It is safe to call multiple times.
func (l *Lease) Release() {
	l.once.Do(func() {
		l.gen.leases.Done()
	})
}

// resourceFactories contains the constructors used while building a generation.
// They are overridable in tests.
type resourceFactories struct {
	newHTTPClient func(opts httpclient.Options) (*http.Client, error)
	newChrome     func(logger log.Logger, conf config.Config) (chrome.Instance, error)
	newPool       func(maxWorkers int) *worker.Pool
}

func defaultFactories() resourceFactories {
	return resourceFactories{
		newHTTPClient: func(opts httpclient.Options) (*http.Client, error) {
			return httpclient.New(opts)
		},
		newChrome: func(logger log.Logger, conf config.Config) (chrome.Instance, error) {
			// Use a background context: the context passed by the SDK when
			// creating an App instance is canceled when the triggering request
			// ends, which would immediately kill the browser and workers.
			// Resources are safely released by the generation close path.
			if conf.RemoteChromeURL == "" {
				return chrome.NewLocalBrowserInstance(
					context.Background(),
					logger,
					conf.HTTPClientOptions.TLS.InsecureSkipVerify,
				)
			}

			return chrome.NewRemoteBrowserInstance(
				context.Background(),
				logger,
				conf.RemoteChromeURL,
			)
		},
		newPool: func(maxWorkers int) *worker.Pool {
			return worker.New(context.Background(), maxWorkers)
		},
	}
}

// Manager owns the configuration generations of the plugin process. It survives
// the App instances created and disposed by the Grafana SDK: when settings are
// updated, the SDK builds a new App (and disposes the old one a few seconds
// later), while the manager coordinates atomic, lease-aware generation
// handover.
type Manager struct {
	mu sync.Mutex

	// active is the only generation that receives new report requests.
	active *Generation

	// draining contains retired generations that no longer receive new reports
	// but may still be used by reports holding leases.
	draining map[uint64]*Generation

	closed bool
	nextID uint64

	factories resourceFactories
	logger    log.Logger
}

// newManager creates a generation manager using the production resource
// factories.
func newManager(logger log.Logger) *Manager {
	return newManagerWithFactories(logger, defaultFactories())
}

func newManagerWithFactories(logger log.Logger, factories resourceFactories) *Manager {
	return &Manager{
		draining:  make(map[uint64]*Generation),
		factories: factories,
		nextID:    1,
		logger:    logger,
	}
}

// Active returns the generation currently receiving new reports. It is used by
// health and diagnostics paths that do not need a lease.
func (m *Manager) Active() *Generation {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.active
}

// Lease acquires a lease on the active generation for a new report. It must be
// called at the report entry and the returned lease released once the response
// has been written. After a generation has been retired, Lease can no longer
// hand it out, so an old generation never receives new requests.
func (m *Manager) Lease() (*Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed || m.active == nil {
		return nil, ErrServiceUnavailable
	}

	gen := m.active
	gen.leases.Add(1)

	return &Lease{gen: gen}, nil
}

// Activate builds a complete generation from settings and promotes it only
// after the config, HTTP client, browser and worker capacity are all ready.
//
// If any step of the build fails, every resource already created for the
// attempt is closed and the current generation keeps serving: there is no
// observable switch to a broken generation.
func (m *Manager) Activate(ctx context.Context, settings backend.AppInstanceSettings) (*Generation, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()

		return nil, errManagerShuttingDown
	}

	genID := m.nextID
	m.nextID++
	// The generation that was active when this update started. A newer update
	// completing while we are still building means we are an obsolete
	// candidate and must not be promoted.
	expectedActive := m.active
	m.mu.Unlock()

	gen, err := m.buildGeneration(ctx, genID, settings)
	if err != nil {
		m.logger.Error(
			"failed to build new configuration generation; current generation continues to serve reports",
			"generation", genID, "err", err,
		)

		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		// Plugin is shutting down; do not leave a stray browser behind.
		gen.close()

		return nil, errManagerShuttingDown
	}

	// While we were building (a slow local/remote Chromium start, for
	// instance), a newer update may already have promoted another generation.
	// We never served traffic, so we are just a stale candidate: close our own
	// resources and adopt the current generation. Active and draining
	// generations are never closed from here.
	if m.active != expectedActive {
		m.logger.Info(
			"discarding configuration generation superseded while being built",
			"generation", gen.id,
		)
		gen.close()

		return m.active, nil
	}

	previous := m.active
	m.active = gen

	m.logger.Info("activating new plugin configuration generation", "generation", gen.id)

	if previous != nil {
		m.draining[previous.id] = previous
		go m.drain(previous)
	}

	return gen, nil
}

// Shutdown stops accepting new reports, waits up to the context deadline for
// in-flight reports across active and draining generations to finish, and then
// force-closes every remaining generation, stopping its worker pools and
// cleaning up its local or remote Chromium and HTTP connections.
// It is idempotent.
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()

		return
	}

	m.closed = true

	gens := make([]*Generation, 0, len(m.draining)+1)

	for _, gen := range m.draining {
		gens = append(gens, gen)
	}

	if m.active != nil {
		gens = append(gens, m.active)
	}

	m.active = nil
	m.draining = make(map[uint64]*Generation)
	m.mu.Unlock()

	if len(gens) == 0 {
		return
	}

	// Bounded wait for leases to be returned.
	allReturned := make(chan struct{})

	go func() {
		for _, gen := range gens {
			gen.leases.Wait()
		}

		close(allReturned)
	}()

	select {
	case <-allReturned:
	case <-ctx.Done():
		m.logger.Warn(
			"timed out waiting for in-flight reports to finish; terminating remaining leases",
			"err", ctx.Err(),
		)
	}

	// Idempotent: generations that drained naturally are already closed. The
	// remaining ones are closed now, interrupting any outstanding reports.
	for _, gen := range gens {
		gen.close()
	}
}

// buildGeneration constructs and validates a full resource group. On any
// failure it closes whatever has already been created for this attempt.
func (m *Manager) buildGeneration(ctx context.Context, genID uint64, settings backend.AppInstanceSettings) (*Generation, error) {
	gen := &Generation{
		id:     genID,
		logger: m.logger,
	}

	built := false

	defer func() {
		if !built {
			gen.close()
		}
	}()

	// 1. Configuration.
	conf, err := config.Load(ctx, settings)
	if err != nil {
		return nil, fmt.Errorf("error loading config: %w", err)
	}

	gen.conf = conf

	// 2. HTTP client.
	gen.httpClient, err = m.factories.newHTTPClient(gen.conf.HTTPClientOptions)
	if err != nil {
		return nil, fmt.Errorf("error in httpclient new: %w", err)
	}

	// 3. Local or remote Chromium.
	gen.chromeInstance, err = m.factories.newChrome(m.logger, gen.conf)
	if err != nil {
		m.logger.Error("failed to start browser", "err", err)

		return nil, fmt.Errorf("failed to start browser: %w", err)
	}

	// 4. Verify the browser is actually usable. A remote allocator connects
	// lazily, so without this an unreachable remote Chromium would be promoted
	// and the report entry would break for every request.
	err = gen.chromeInstance.Ready(ctx, m.logger)
	if err != nil {
		m.logger.Error("new browser instance is not ready", "err", err)

		return nil, fmt.Errorf("browser is not ready: %w", err)
	}

	// 5. Worker capacity.
	gen.workerPools = worker.Pools{
		worker.Browser:  m.factories.newPool(gen.conf.MaxBrowserWorkers),
		worker.Renderer: m.factories.newPool(gen.conf.MaxRenderWorkers),
	}

	// Verify workers are processing tasks before promoting.
	pingCtx, cancel := context.WithTimeout(ctx, workerReadyTimeout)
	defer cancel()

	for poolName, pool := range gen.workerPools {
		err = pool.Ping(pingCtx)
		if err != nil {
			return nil, fmt.Errorf("worker pool %s is not ready: %w", poolName, err)
		}
	}

	built = true

	return gen, nil
}

// drain waits until all reports using a retired generation have returned their
// leases and then closes it. A generation parked here is not affected by later
// Activate calls that promote new generations.
func (m *Manager) drain(gen *Generation) {
	gen.leases.Wait()
	gen.close()

	m.mu.Lock()
	delete(m.draining, gen.id)
	m.mu.Unlock()
}

// pluginManager is the process-wide generation manager. It is shared across
// the App instances created/disposed by the Grafana SDK when admins update the
// plugin settings.
var pluginManager = newManager(log.DefaultLogger)

// Shutdown gracefully shuts down the plugin's generations. It waits up to the
// context deadline for in-flight reports to finish before force-closing the
// remaining resources (worker pools, HTTP clients and local/remote Chromium).
func Shutdown(ctx context.Context) {
	pluginManager.Shutdown(ctx)
}
