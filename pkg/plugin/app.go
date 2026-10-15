package plugin

import (
	"context"
	"net/http"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/backend/resource/httpadapter"
	"github.com/grafana/grafana-plugin-sdk-go/backend/useragent"
)

const Name = "mahendrapaipuri-dashboardreporter-app"

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
//
// App itself is a thin handle created by the Grafana SDK whenever the plugin
// settings are updated. All heavyweight resources (config, HTTP client, local
// or remote Chromium, worker pools) are owned by configuration generations in
// the process-wide manager. Each report request acquires a lease on the
// active generation at entry, so the disposal of an old App by the SDK can
// never interrupt an in-flight report.
type App struct {
	backend.CallResourceHandler

	manager *Manager

	// generation is the generation that was active when this App handle was
	// created. Requests nonetheless always lease the manager's current active
	// generation at entry.
	generation *Generation

	grafanaSemVer string
	ctxLogger     log.Logger
}

// NewDashboardReporterApp creates a new *App instance. The Grafana SDK calls
// this whenever the plugin is started or the admin updates the plugin
// settings. The new generation is built and validated completely (config,
// HTTP client, browser and worker capacity) before it starts serving reports;
// if the build fails the previous generation continues to serve and this
// returns an error.
func NewDashboardReporterApp(ctx context.Context, settings backend.AppInstanceSettings) (instancemgmt.Instance, error) {
	// Get context logger for debugging
	ctxLogger := log.DefaultLogger.FromContext(ctx)
	ctxLogger.Info("new instance of plugin app created")

	// Build and activate the full resource generation. Nothing is switched
	// over until all resources are ready; on failure any partially created
	// resources are closed and the current generation keeps serving.
	generation, err := pluginManager.Activate(ctx, settings)
	if err != nil {
		ctxLogger.Error("error activating plugin configuration generation", "err", err)

		return nil, err
	}

	ctxLogger.Info("starting plugin with initial config: " + generation.conf.String())

	// Get current Grafana version
	grafanaSemVer := "v" + useragent.FromContext(ctx).GrafanaVersion()

	if grafanaSemVer == "v0.0.0" && generation.conf.AppVersion != "0.0.0" {
		grafanaSemVer = "v" + generation.conf.AppVersion

		ctxLogger.Debug("got grafana version from plugin settings", "version", grafanaSemVer)
	} else {
		ctxLogger.Debug("got grafana version from backend user agent", "version", grafanaSemVer)
	}

	app := &App{
		manager:       pluginManager,
		generation:    generation,
		grafanaSemVer: grafanaSemVer,
		ctxLogger:     ctxLogger,
	}

	// Use a httpadapter (provided by the SDK) for resource calls. This allows us
	// to use a *http.ServeMux for resource calls, so we can map multiple routes
	// to CallResource without having to implement extra logic.
	mux := http.NewServeMux()
	app.registerRoutes(mux)
	app.CallResourceHandler = httpadapter.New(mux)

	return app, nil
}

// Dispose is called by the Grafana SDK a few seconds after the App has been
// replaced because settings were updated. It must not close anything here:
//
//   - the generation that was active when this App was created may still be
//     serving reports that entered through it, and is closed by the manager
//     only after all its leases have been returned;
//   - a newer generation may already be active and owned by the manager.
//
// Resource cleanup (worker pools, HTTP connections and local/remote Chromium)
// happens in the generation drain path or, bounded, at process shutdown.
func (app *App) Dispose() {
	app.ctxLogger.Debug("plugin app instance disposed by Grafana SDK; generation resources are managed independently")
}

// CheckHealth handles health checks sent from Grafana to the plugin.
func (app *App) CheckHealth(_ context.Context, _ *backend.CheckHealthRequest) (*backend.CheckHealthResult, error) {
	return &backend.CheckHealthResult{
		Status:  backend.HealthStatusOk,
		Message: "ok",
	}, nil
}
