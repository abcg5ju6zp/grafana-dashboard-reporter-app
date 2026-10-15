package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/config"
	"github.com/mahendrapaipuri/authlib/authz"
	plugin_config "github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/config"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/dashboard"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/helpers"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/report"
)

// Sentinel errors used to map report generation failures onto HTTP statuses.
var (
	// errPermissionDenied is returned when the user is not allowed to read the
	// dashboard (or any of its parent folders) of a snapshot.
	errPermissionDenied = errors.New("permission denied")
)

// statusError is an error carrying the HTTP status returned by Grafana API.
type statusError struct {
	status  int
	apiURL  string
	message string
}

func (e *statusError) Error() string {
	return fmt.Sprintf(
		"grafana API request failed: URL: %s. Status: %d, message: %s",
		e.apiURL,
		e.status,
		e.message,
	)
}

// GrafanaUserSignInTokenHeaderName the header name used for forwarding
// the SignIn token of a Grafana User.
// Requires idForwarded feature toggle enabled.
const GrafanaUserSignInTokenHeaderName = "X-Grafana-Id" //nolint:gosec

// Required feature flags.
const (
	accessControlFeatureFlag = "accessControlOnCall" // added in Grafana 10.4.0
	idForwardingFlag         = "idForwarding"        // added in Grafana 10.3.0
)

const (
	uidAttr = "uid"
)

// convertPanelIDs returns panel IDs based on Grafana version.
func (app *App) convertPanelIDs(ids []string) []string {
	// For Grafana < 11.3.0, we can use the IDs as such
	if helpers.SemverCompare(app.grafanaSemVer, "v11.3.0") == -1 {
		return ids
	}

	panelIDs := make([]string, len(ids))

	for i, id := range ids {
		if !strings.HasPrefix(id, "panel") {
			panelIDs[i] = "panel-" + id
		} else {
			panelIDs[i] = id
		}
	}

	return panelIDs
}

// updateConfig updates the default config from query parameters.
func (app *App) updateConfig(req *http.Request, conf *plugin_config.Config) {
	if req.URL.Query().Has("theme") {
		conf.Theme = req.URL.Query().Get("theme")
	}

	if req.URL.Query().Has("layout") {
		conf.Layout = req.URL.Query().Get("layout")
	}

	if req.URL.Query().Has("orientation") {
		conf.Orientation = req.URL.Query().Get("orientation")
	}

	if req.URL.Query().Has("dashboardMode") {
		conf.DashboardMode = req.URL.Query().Get("dashboardMode")
	}

	if req.URL.Query().Has("format") {
		conf.ReportFormat = req.URL.Query().Get("format")
	}

	if req.URL.Query().Has("timeZone") {
		conf.TimeZone = req.URL.Query().Get("timeZone")
	}

	// Starting from Grafana v11.3.0, Grafana sets timezone query parameter.
	// We should give priority to that over the plugin's config value.
	// We will still support plugin's config parameter for backwards compatibility
	if req.URL.Query().Has("timezone") {
		timeZone := req.URL.Query().Get("timezone")
		if !slices.Contains([]string{"browser", "default"}, timeZone) {
			if timeZone == "utc" {
				timeZone = "Etc/UTC"
			}

			conf.TimeZone = timeZone
		}
	}

	if req.URL.Query().Has("timeFormat") {
		conf.TimeFormat = req.URL.Query().Get("timeFormat")
	}

	if req.URL.Query().Has("includePanelID") {
		conf.IncludePanelIDs = app.convertPanelIDs(req.URL.Query()["includePanelID"])
	}

	if req.URL.Query().Has("excludePanelID") {
		conf.ExcludePanelIDs = app.convertPanelIDs(req.URL.Query()["excludePanelID"])
	}

	if req.URL.Query().Has("includePanelDataID") {
		conf.IncludePanelDataIDs = app.convertPanelIDs(req.URL.Query()["includePanelDataID"])
	}
}

// featureTogglesEnabled checks if the necessary feature toogles are enabled on Grafana server.
func (app *App) featureTogglesEnabled(ctx context.Context) bool {
	// If Grafana <= 10.4.3, we use cookies to make request. Moreover feature toggles are
	// not available for these Grafana versions.
	if helpers.SemverCompare(app.grafanaSemVer, "v10.4.3") <= 0 {
		return false
	}

	// From Grafana 11.6.0 and above accessControl and idForwaring feature toggles are always enabled.
	// So we can skip checking for them.
	if helpers.SemverCompare(app.grafanaSemVer, "v11.6.0") >= 0 {
		return true
	}

	// Get Grafana config from context
	cfg := config.GrafanaConfigFromContext(ctx)

	// For grafana >= 10.4.4 and < 11.2.0 check for feature toggles
	if helpers.SemverCompare(app.grafanaSemVer, "v11.2.0") == -1 {
		if cfg.FeatureToggles().IsEnabled(accessControlFeatureFlag) && cfg.FeatureToggles().IsEnabled(idForwardingFlag) {
			return true
		}
	} else {
		// In Grafana v11.2.0, idForwarding has been removed. So, check for only accessControl
		// for versions >= 11.2.0 and < 11.6.0
		if cfg.FeatureToggles().IsEnabled(accessControlFeatureFlag) {
			return true
		}
	}

	return false
}

// grafanaAppURL returns the Grafana's App URL. User configured URL has higher
// precedence than the App URL in the request's context.
func (app *App) grafanaAppURL(grafanaConfig *config.GrafanaCfg) (string, error) {
	var grafanaAppURL string

	var err error

	if app.conf.AppURL != "" {
		grafanaAppURL = app.conf.AppURL
	} else {
		grafanaAppURL, err = grafanaConfig.AppURL()
		if err != nil {
			return "", err
		}
	}

	return strings.TrimSuffix(grafanaAppURL, "/"), nil
}

// dashboardFolder fetches current dashboard folder from Grafana API.
func (app *App) dashboardFolder(ctx context.Context, appURL, folderUID string, authHeader http.Header) ([]string, error) {
	dashURL := fmt.Sprintf("%s/api/folders/%s", appURL, folderUID)

	// Create a new GET request
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dashURL, nil)
	if err != nil {
		return nil, fmt.Errorf("error creating request for %s: %w", dashURL, err)
	}

	// Forward auth headers
	for name, values := range authHeader {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}

	// Make request
	resp, err := app.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error executing request for %s: %w", dashURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response body from %s: %w", dashURL, err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, &statusError{
			status:  resp.StatusCode,
			apiURL:  dashURL,
			message: string(body),
		}
	}

	var folder dashboard.Folder

	// Read data into dashboard.Model
	err = json.Unmarshal(body, &folder) //nolint:musttag
	if err != nil {
		return nil, fmt.Errorf("error reading response body into dashboard folder: %w", err)
	}

	// Get folder parents
	var parentUIDs []string
	for _, parent := range folder.Parents {
		parentUIDs = append(parentUIDs, parent.UID)
	}

	return parentUIDs, nil
}

// dashboardModel fetches dashboard JSON model from Grafana API.
func (app *App) dashboardModel(ctx context.Context, appURL, dashUID string, authHeader http.Header, values url.Values) (*dashboard.Model, error) {
	dashURL := fmt.Sprintf("%s/api/dashboards/uid/%s", appURL, dashUID)

	// Create a new GET request
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dashURL, nil) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("error creating request for %s: %w", dashURL, err)
	}

	// Forward auth headers
	for name, values := range authHeader {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}

	// Make request
	resp, err := app.httpClient.Do(req) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("error executing request for %s: %w", dashURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response body from %s: %w", dashURL, err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, &statusError{
			status:  resp.StatusCode,
			apiURL:  dashURL,
			message: string(body),
		}
	}

	var model dashboard.Model

	// Read data into dashboard.Model
	err = json.Unmarshal(body, &model) //nolint:musttag
	if err != nil {
		return nil, fmt.Errorf("error reading response body into dashboard model: %w", err)
	}

	// Add template variables to model
	model.Dashboard.Variables = values

	// If dashboard is in folder, fetch all parents. Because if permissions is set on
	// parent folder, they are inherited in the nested dashboards and so, we need to
	// provide the top level one where permissions are set
	if model.Meta.FolderUID != "" {
		folderUIDs, err := app.dashboardFolder(ctx, appURL, model.Meta.FolderUID, authHeader)
		if err != nil {
			return nil, fmt.Errorf("error fetching dashboard folder: %w", err)
		}

		model.Meta.ParentFolderUIDs = folderUIDs
	}

	return &model, nil
}

// handleReport handles creating a PDF report from a given dashboard UID
// GET /api/plugins/mahendrapaipuri-dashboardreporter-app/resources/report.
func (app *App) handleReport(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	var err error

	// Always start with an instance of current app's config
	conf := app.conf

	// Get context logger which we will use everywhere
	ctxLogger := log.DefaultLogger.FromContext(req.Context())

	// Get config from context
	pluginConfig := backend.PluginConfigFromContext(req.Context())
	currentUser := pluginConfig.User.Login

	// Get Dashboard ID
	dashboardUID := req.URL.Query().Get("dashUid")
	if dashboardUID == "" {
		ctxLogger.Debug("Query parameter dashUid not found")
		http.Error(w, "missing dashUid query parameter", http.StatusBadRequest)

		return
	}

	// Add dash uid and user to logger
	ctxLogger = ctxLogger.With("user", currentUser, "dash_uid", dashboardUID)

	grafanaConfig := config.GrafanaConfigFromContext(req.Context())

	// Get Grafana App URL by looking both at passed config and user defined config
	grafanaAppURL, err := app.grafanaAppURL(grafanaConfig)
	if err != nil {
		ctxLogger.Error("failed to get app URL", "err", err)
		http.Error(w, "error generating report", http.StatusInternalServerError)

		return
	}

	// Update plugin's config from query params
	app.updateConfig(req, &conf)

	// Validate new updated config
	err = conf.RTValidate()
	if err != nil {
		ctxLogger.Debug("invalid config: "+conf.String(), "err", err)
		http.Error(w, "invalid query parameters found", http.StatusBadRequest)

		return
	}

	ctxLogger.Info("generate report using config: " + conf.String())

	// authHeader is header name value pair that will be used in API requests
	authHeader := http.Header{}

	switch {
	// This case is irrelevant starting from Grafana 10.4.4.
	// This commit https://github.com/grafana/grafana/commit/56a4af87d706087ea42780a79f8043df1b5bc3ea
	// made changes to not forward the cookies to app plugins.
	// So we will not be able to use cookies to make requests to Grafana to fetch
	// dashboards.
	case req.Header.Get(backend.CookiesHeaderName) != "":
		ctxLogger.Debug("using user cookie")

		authHeader.Add(backend.CookiesHeaderName, req.Header.Get(backend.CookiesHeaderName))
	case conf.Token != "":
		ctxLogger.Debug("using user configured token")

		authHeader.Add(backend.OAuthIdentityTokenHeaderName, "Bearer "+conf.Token)
	default:
		ctxLogger.Debug("using service account token")

		saToken, err := grafanaConfig.PluginAppClientSecret()
		if err != nil {
			ctxLogger.Error("failed to get plugin app client secret", "err", err)
			http.Error(w, "error generating report", http.StatusInternalServerError)

			return
		}

		if saToken == "" {
			ctxLogger.Error("failed to get plugin app client secret", "err", "empty client secret")
			http.Error(w, "error generating report", http.StatusInternalServerError)

			return
		}

		authHeader.Add(backend.OAuthIdentityTokenHeaderName, "Bearer "+saToken)
	}

	// buildSnapshot reads the dashboard model and its folder permission
	// resources as they are stored right now, enforces that the current user
	// can read them and binds that model version together with one absolute
	// time window (resolved once from the request's possibly relative from/to)
	// into an immutable snapshot.
	//
	// The same closure is used for the initial snapshot and for rebuilding a
	// complete candidate after the dashboard has been saved during generation.
	buildSnapshot := func(ctx context.Context) (*dashboard.Snapshot, error) {
		// Get dashboard JSON model from API
		model, err := app.dashboardModel(ctx, grafanaAppURL, dashboardUID, authHeader, req.URL.Query())
		if err != nil {
			return nil, fmt.Errorf("failed to get dashboard JSON model: %w", err)
		}

		// Build the permission resources from THIS model version: the dashboard
		// itself, its folder and all parent folders.
		resources := dashboardResources(dashboardUID, model)

		// If the required feature flags are enabled, check if user has access to the resource
		// using authz client.
		// Here we check if user has permissions to do an action "dashboards:read" on
		// dashboards resource of a given dashboard UID.
		//
		// This runs on every snapshot (including rebuilds after a version change),
		// so a permission change always fails the current generation directly,
		// without reusing previous permission results.
		if app.featureTogglesEnabled(ctx) {
			hasAccess, err := app.HasAccess(req, "dashboards:read", resources...)
			if err != nil {
				return nil, fmt.Errorf("failed to check permissions: %w", err)
			}

			if !hasAccess {
				return nil, errPermissionDenied
			}
		}

		// Pin relative from/to to a single absolute window for this snapshot.
		snapshot, err := dashboard.NewSnapshot(model, time.Now())
		if err != nil {
			return nil, &statusError{
				status:  http.StatusBadRequest,
				apiURL:  req.URL.String(),
				message: err.Error(),
			}
		}

		return snapshot, nil
	}

	// Build the initial report snapshot at the request entry.
	snapshot, err := buildSnapshot(req.Context())
	if err != nil {
		ctxLogger.Error("failed to build dashboard snapshot", "err", err)
		respondReportError(w, err)

		return
	}

	ctxLogger.Info("report snapshot bound", "dashboard_version", snapshot.Version(),
		"from", snapshot.From, "to", snapshot.To)

	grafanaDashboard, err := dashboard.New(
		ctxLogger,
		&conf,
		app.httpClient,
		app.chromeInstance,
		grafanaAppURL,
		app.grafanaSemVer,
		snapshot,
		authHeader,
	)
	if err != nil {
		ctxLogger.Error("failed to create a new dashboard", "err", err)
		http.Error(w, "error generating report", http.StatusInternalServerError)

		return
	}

	// Version fetcher used at every stage boundary to detect dashboard saves
	// happening while the report is being assembled.
	grafanaDashboard.SetVersionFetcher(func(ctx context.Context) (int, error) {
		freshModel, err := app.dashboardModel(ctx, grafanaAppURL, dashboardUID, authHeader, req.URL.Query())
		if err != nil {
			return 0, fmt.Errorf("failed to refresh dashboard version: %w", err)
		}

		return freshModel.Dashboard.Version, nil
	})

	ctxLogger.Info(fmt.Sprintf("generate report using %s chrome", app.chromeInstance.Name()))

	// Make app new Report to put all PNGs into app HTML template and print it into app PDF
	pdfReport := report.New(
		ctxLogger,
		&conf,
		app.httpClient,
		app.chromeInstance,
		app.workerPools,
		grafanaDashboard,
	)

	// Allow the report to rebuild a complete candidate from a fresh snapshot
	// when the dashboard changes during generation.
	pdfReport.SetSnapshotBuilder(buildSnapshot)

	// Generate report
	err = pdfReport.Generate(req.Context(), w)
	if err != nil {
		ctxLogger.Error("error generating report", "err", err)
		respondReportError(w, err)

		return
	}

	ctxLogger.Info("report generated")
}

// dashboardResources builds the authz resources (dashboard, its folder and all
// parent folders) associated with a given dashboard model version.
func dashboardResources(dashboardUID string, model *dashboard.Model) []authz.Resource {
	resources := []authz.Resource{
		{
			Kind: "dashboards",
			Attr: uidAttr,
			ID:   dashboardUID,
		},
	}

	if model.Meta.FolderUID != "" {
		resources = append(resources, authz.Resource{
			Kind: "folders",
			Attr: uidAttr,
			ID:   model.Meta.FolderUID,
		})

		// If there are parents, add them too
		for _, uid := range model.Meta.ParentFolderUIDs {
			resources = append(resources, authz.Resource{
				Kind: "folders",
				Attr: uidAttr,
				ID:   uid,
			})
		}
	}

	return resources
}

// respondReportError maps report generation errors onto the appropriate HTTP
// status. A continuously modified dashboard yields 409 Conflict, permission
// failures yield 403 Forbidden and upstream API statuses are forwarded.
func respondReportError(w http.ResponseWriter, err error) {
	var apiErr *statusError

	switch {
	case errors.Is(err, report.ErrReportConflict):
		http.Error(w, "dashboard has been modified while generating the report, please retry", http.StatusConflict)
	case errors.Is(err, errPermissionDenied):
		http.Error(w, "permission denied", http.StatusForbidden)
	case errors.As(err, &apiErr):
		switch apiErr.status {
		case http.StatusForbidden, http.StatusUnauthorized:
			http.Error(w, "permission denied", http.StatusForbidden)
		case http.StatusBadRequest:
			http.Error(w, "invalid query parameters found", http.StatusBadRequest)
		default:
			http.Error(w, "error generating report", http.StatusInternalServerError)
		}
	default:
		http.Error(w, "error generating report", http.StatusInternalServerError)
	}
}

// handleHealth is an example HTTP GET resource that returns an OK response.
func (app *App) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Add("Content-Type", "text/plan")

	_, err := w.Write([]byte("OK"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	w.WriteHeader(http.StatusOK)
}

// registerRoutes takes a *http.ServeMux and registers some HTTP handlers.
func (app *App) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/report", app.handleReport)
	mux.HandleFunc("/healthz", app.handleHealth)
}
