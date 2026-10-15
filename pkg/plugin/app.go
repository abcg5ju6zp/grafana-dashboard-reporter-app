package plugin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/httpclient"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/backend/resource/httpadapter"
	"github.com/grafana/grafana-plugin-sdk-go/backend/useragent"
	"github.com/mahendrapaipuri/authlib/authz"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/chrome"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/config"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/worker"
)

const Name = "mahendrapaipuri-dashboardreporter-app"

// inFlightDrainTimeout is the maximum time Dispose waits for reports started
// on the old instance generation to finish. Past this deadline the remaining
// snapshots are explicitly canceled.
var inFlightDrainTimeout = 30 * time.Second

// errInstanceDisposed cancels in-flight snapshots when their instance
// generation is torn down.
var errInstanceDisposed = errors.New("plugin instance disposed")

// instanceGeneration monotonically identifies plugin instance generations in
// the process. Every settings update creates a new generation.
var instanceGeneration atomic.Uint64

// Make sure App implements required interfaces. This is important to do
// since otherwise we will only get a not implemented error response from plugin in
// runtime. Plugin should not implement all these interfaces - only those which are
// required for a particular task.
var (
	_ backend.CallResourceHandler   = (*App)(nil)
	_ instancemgmt.InstanceDisposer = (*App)(nil)
	_ backend.CheckHealthHandler    = (*App)(nil)
)

// App is the backend plugin which can respond to api queries.
type App struct {
	backend.CallResourceHandler

	// generation identifies the plugin instance generation the app belongs
	// to. Snapshots freeze it at request entry.
	generation uint64

	grafanaSemVer string
	httpClient    *http.Client

	// authzClients caches enforcement clients keyed by the frozen
	// (Grafana URL, token) pair. A request always gets the client matching its
	// snapshot token, never a client swapped in by another request after a
	// token rotation.
	authzClients map[string]authz.EnforcementClient
	mx           sync.Mutex

	conf config.Config

	workerPools    worker.Pools
	chromeInstance chrome.Instance
	ctxLogger      log.Logger

	// instanceCtx is the root context of all snapshots accepted by this
	// instance generation. cancelInstance is called on disposal to explicitly
	// cancel reports that did not finish during the drain period.
	instanceCtx    context.Context
	cancelInstance context.CancelCauseFunc

	// inFlight tracks reports running on this instance generation so disposal
	// can let them complete or cancel them explicitly.
	inFlight sync.WaitGroup
	// disposing rejects new requests once disposal has started.
	disposing atomic.Bool
}

// NewDashboardReporterApp creates a new example *App instance.
func NewDashboardReporterApp(ctx context.Context, settings backend.AppInstanceSettings) (instancemgmt.Instance, error) {
	var (
		app App
		err error
	)

	// Identify this instance generation
	app.generation = instanceGeneration.Add(1)

	// Root context for every snapshot accepted by this generation
	app.instanceCtx, app.cancelInstance = context.WithCancelCause(context.Background())
	app.authzClients = make(map[string]authz.EnforcementClient)

	// Get context logger for debugging
	app.ctxLogger = log.DefaultLogger.FromContext(ctx).With("generation", app.generation)
	app.ctxLogger.Info("new instance of plugin app created")

	// Use a httpadapter (provided by the SDK) for resource calls. This allows us
	// to use a *http.ServeMux for resource calls, so we can map multiple routes
	// to CallResource without having to implement extra logic.
	mux := http.NewServeMux()
	app.registerRoutes(mux)
	app.CallResourceHandler = httpadapter.New(mux)

	app.conf, err = config.Load(ctx, settings)
	if err != nil {
		app.ctxLogger.Error("error loading config", "err", err)

		return nil, fmt.Errorf("error loading config: %w", err)
	}

	app.ctxLogger.Info("starting plugin with initial config: " + app.conf.String())

	// Get current Grafana version
	app.grafanaSemVer = "v" + useragent.FromContext(ctx).GrafanaVersion()

	if app.grafanaSemVer == "v0.0.0" && app.conf.AppVersion != "0.0.0" {
		app.grafanaSemVer = "v" + app.conf.AppVersion

		app.ctxLogger.Debug("got grafana version from plugin settings", "version", app.grafanaSemVer)
	} else {
		app.ctxLogger.Debug("got grafana version from backend user agent", "version", app.grafanaSemVer)
	}

	// Make a new HTTP client
	app.httpClient, err = httpclient.New(app.conf.HTTPClientOptions)
	if err != nil {
		return nil, fmt.Errorf("error in httpclient new: %w", err)
	}

	// Create a new browser instance
	var chromeInstance chrome.Instance

	switch app.conf.RemoteChromeURL {
	case "":
		chromeInstance, err = chrome.NewLocalBrowserInstance(
			context.Background(),
			app.ctxLogger,
			app.conf.HTTPClientOptions.TLS.InsecureSkipVerify,
		)
	default:
		chromeInstance, err = chrome.NewRemoteBrowserInstance(
			context.Background(),
			app.ctxLogger,
			app.conf.RemoteChromeURL,
		)
	}

	if err != nil {
		app.ctxLogger.Error("failed to start browser", "err", err)

		return nil, fmt.Errorf("failed to start browser: %w", err)
	}

	// Use the same browser instance for all API requests
	app.chromeInstance = chromeInstance

	// Span Worker Pool across multiple instances
	// Seems like context passed by App instance is closing channel at the end of
	// request which I dont understand.
	// So, discard context from the App. Always use background context and as we are
	// safely disposing both workers and chrome instances in dispose() method, we are
	// sure that there wont be any leaks.
	app.workerPools = worker.Pools{
		worker.Browser:  worker.New(context.Background(), app.conf.MaxBrowserWorkers),
		worker.Renderer: worker.New(context.Background(), app.conf.MaxRenderWorkers),
	}

	return &app, nil
}

// Dispose here tells plugin SDK that plugin wants to clean up resources when a new instance
// created.
//
// The SDK swaps in the new instance immediately and calls Dispose on the old
// generation a few seconds later in its own goroutine. We first let reports
// that froze their snapshot on this generation complete. If they do not
// finish within inFlightDrainTimeout their snapshots are explicitly canceled,
// worker pools are stopped (queued work is drained and fails fast from the
// canceled context) and the browser is closed. No report mixes old and new
// instance configuration.
func (app *App) Dispose() {
	if !app.disposing.CompareAndSwap(false, true) {
		return
	}

	app.ctxLogger.Info("disposing plugin app instance, draining in-flight reports")

	done := make(chan struct{})

	go func() {
		app.inFlight.Wait()
		close(done)
	}()

	select {
	case <-done:
		app.ctxLogger.Info("all in-flight reports completed before disposal")
	case <-time.After(inFlightDrainTimeout):
		app.ctxLogger.Warn("timed out waiting for in-flight reports, explicitly canceling snapshots")
	}

	// Explicitly cancel snapshots that are still running. Downstream work
	// (HTTP requests, Chrome tabs) is tied to the snapshot context and fails
	// fast instead of reading any updated configuration.
	app.cancelInstance(errInstanceDisposed)

	if app.workerPools != nil {
		for name, pool := range app.workerPools {
			app.ctxLogger.Debug("stopping worker pool", "pool", name)
			pool.Done()
		}
	}

	if app.chromeInstance != nil {
		// cleanup old chromium instances
		app.ctxLogger.Info("disposing chromium from old plugin app instance")
		app.chromeInstance.Close(app.ctxLogger)
	}

	// Clean up idle connections
	app.httpClient.CloseIdleConnections()
}

// CheckHealth handles health checks sent from Grafana to the plugin.
func (app *App) CheckHealth(_ context.Context, _ *backend.CheckHealthRequest) (*backend.CheckHealthResult, error) {
	return &backend.CheckHealthResult{
		Status:  backend.HealthStatusOk,
		Message: "ok",
	}, nil
}

// beginRequest registers an in-flight report on the instance generation. It
// returns false once disposal has started, in which case the caller must not
// touch any instance resource. New requests are routed by Grafana to the
// freshly created instance anyway.
func (app *App) beginRequest() bool {
	if app.disposing.Load() {
		return false
	}

	app.inFlight.Add(1)

	// Re-check under the Add to close the race with Dispose
	if app.disposing.Load() {
		app.inFlight.Done()

		return false
	}

	return true
}
