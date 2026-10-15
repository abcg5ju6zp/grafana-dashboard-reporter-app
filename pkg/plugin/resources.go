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

	"github.com/grafana/grafana-plugin-sdk-go/config"
	"github.com/mahendrapaipuri/authlib/authz"
	plugin_config "github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/config"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/dashboard"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/helpers"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/report"
)

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

// updateConfigFromValues updates the frozen per-request config from query
// parameters. The instance config is never mutated.
func (app *App) updateConfigFromValues(values url.Values, conf *plugin_config.Config) {
	if values.Has("theme") {
		conf.Theme = values.Get("theme")
	}

	if values.Has("layout") {
		conf.Layout = values.Get("layout")
	}

	if values.Has("orientation") {
		conf.Orientation = values.Get("orientation")
	}

	if values.Has("dashboardMode") {
		conf.DashboardMode = values.Get("dashboardMode")
	}

	if values.Has("format") {
		conf.ReportFormat = values.Get("format")
	}

	if values.Has("timeZone") {
		conf.TimeZone = values.Get("timeZone")
	}

	// Starting from Grafana v11.3.0, Grafana sets timezone query parameter.
	// We should give priority to that over the plugin's config value.
	// We will still support plugin's config parameter for backwards compatibility
	if values.Has("timezone") {
		timeZone := values.Get("timezone")
		if !slices.Contains([]string{"browser", "default"}, timeZone) {
			if timeZone == "utc" {
				timeZone = "Etc/UTC"
			}

			conf.TimeZone = timeZone
		}
	}

	if values.Has("timeFormat") {
		conf.TimeFormat = values.Get("timeFormat")
	}

	if values.Has("includePanelID") {
		conf.IncludePanelIDs = app.convertPanelIDs(values["includePanelID"])
	}

	if values.Has("excludePanelID") {
		conf.ExcludePanelIDs = app.convertPanelIDs(values["excludePanelID"])
	}

	if values.Has("includePanelDataID") {
		conf.IncludePanelDataIDs = app.convertPanelIDs(values["includePanelDataID"])
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

// grafanaAPIGet performs a GET against a Grafana API using exclusively the
// snapshot's frozen URL and authentication header. Non 2xx responses are
// returned as apiStatusError; 401/403 additionally wrap errPermissionDenied so
// callers can fail consistently without retrying under another identity.
func (app *App) grafanaAPIGet(ctx context.Context, requestURL string, authHeader http.Header) ([]byte, error) {
	// Create a new GET request
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("error creating request for %s: %w", requestURL, err)
	}

	// Forward the frozen auth headers. A clone ensures the snapshot header can
	// never be mutated through the request.
	for name, values := range authHeader.Clone() {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}

	// Make request
	resp, err := app.httpClient.Do(req) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("error executing request for %s: %w", requestURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response body from %s: %w", requestURL, err)
	}

	if resp.StatusCode != http.StatusOK {
		statusErr := &apiStatusError{
			url:        requestURL,
			statusCode: resp.StatusCode,
			status:     resp.Status,
			body:       string(body),
		}

		// Authentication/authorization failures must surface as a consistent
		// failure. The caller must not switch token and retry.
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("%w: %w", errPermissionDenied, statusErr)
		}

		return nil, statusErr
	}

	return body, nil
}

// dashboardFolder fetches current dashboard folder from Grafana API using the
// frozen snapshot. If the folder tree changes while the report is being
// generated (folder moved/deleted), the upstream error is returned as-is and
// the report fails consistently.
func (app *App) dashboardFolder(snap *reportSnapshot, folderUID string) ([]string, error) {
	folderURL := fmt.Sprintf("%s/api/folders/%s", snap.grafanaAppURL, folderUID)

	body, err := app.grafanaAPIGet(snap.ctx, folderURL, snap.authHeader)
	if err != nil {
		return nil, err
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

// dashboardModel fetches dashboard JSON model from Grafana API using the
// frozen snapshot. Both the model and the parent folder UIDs are read with
// the same identity, so a mid-flight config or token change cannot produce a
// mixed view.
func (app *App) dashboardModel(snap *reportSnapshot) (*dashboard.Model, error) {
	dashURL := fmt.Sprintf("%s/api/dashboards/uid/%s", snap.grafanaAppURL, snap.dashboardUID)

	body, err := app.grafanaAPIGet(snap.ctx, dashURL, snap.authHeader)
	if err != nil {
		return nil, err
	}

	var model dashboard.Model

	// Read data into dashboard.Model
	err = json.Unmarshal(body, &model) //nolint:musttag
	if err != nil {
		return nil, fmt.Errorf("error reading response body into dashboard model: %w", err)
	}

	// Add template variables to model from the frozen query parameters
	model.Dashboard.Variables = snap.query

	// If dashboard is in folder, fetch all parents. Because if permissions is set on
	// parent folder, they are inherited in the nested dashboards and so, we need to
	// provide the top level one where permissions are set
	if model.Meta.FolderUID != "" {
		folderUIDs, err := app.dashboardFolder(snap, model.Meta.FolderUID)
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

	// Freeze the requesting user, Grafana URL, auth header, report options,
	// feature toggles and instance generation at entry. Token resolution
	// failures abort before any downstream work is started.
	snap, status, err := app.newReportSnapshot(req)
	if err != nil {
		http.Error(w, errText(status), status)

		return
	}
	defer snap.close()

	// Attach the report to its instance generation. Once disposal of this
	// generation has started, new requests are refused (Grafana routes them to
	// the new instance) instead of racing teardown.
	if !app.beginRequest() {
		snap.logger.Warn("rejecting report request: plugin instance is disposing")
		http.Error(w, "plugin instance is reloading, please retry", http.StatusServiceUnavailable)

		return
	}
	defer app.inFlight.Done()

	ctxLogger := snap.logger

	// Get dashboard JSON model from API. Both the model and the parent folders
	// are read exclusively with the frozen identity and URL.
	model, err := app.dashboardModel(snap)
	if err != nil {
		ctxLogger.Error("failed to get dashboard JSON model", "err", err, "generation", snap.generation)

		if errors.Is(err, errPermissionDenied) {
			http.Error(w, "permission denied", http.StatusForbidden)
		} else {
			http.Error(w, "error generating report", http.StatusInternalServerError)
		}

		return
	}

	// If dashboard is in a folder, check if user has permissions on either the dashboard
	// or the folder.
	resources := []authz.Resource{
		{
			Kind: "dashboards",
			Attr: uidAttr,
			ID:   snap.dashboardUID,
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

	// If the required feature flags are enabled, check if user has access to the resource
	// using authz client. The check uses only the frozen id token and the client built
	// for the frozen service account token.
	// Here we check if user has permissions to do an action "dashboards:read" on
	// dashboards resource of a given dashboard UID
	if snap.authzEnabled {
		hasAccess, err := snap.hasAccess("dashboards:read", resources...)
		if err != nil || !hasAccess {
			if err != nil {
				ctxLogger.Error("failed to check permissions", "err", err)
			} else {
				ctxLogger.Error("user does not have necessary permissions to view dashboard")
			}

			http.Error(w, "permission denied", http.StatusForbidden)

			return
		}
	}

	grafanaDashboard, err := dashboard.New(
		ctxLogger,
		&snap.conf,
		app.httpClient,
		app.chromeInstance,
		snap.grafanaAppURL,
		app.grafanaSemVer,
		model,
		snap.authHeader,
	)
	if err != nil {
		ctxLogger.Error("failed to create a new dashboard", "err", err)
		http.Error(w, "error generating report", http.StatusInternalServerError)

		return
	}

	ctxLogger.Info(fmt.Sprintf("generate report using %s chrome", app.chromeInstance.Name()))

	// Make app new Report to put all PNGs into app HTML template and print it into app PDF
	pdfReport := report.New(
		ctxLogger,
		&snap.conf,
		app.httpClient,
		app.chromeInstance,
		app.workerPools,
		grafanaDashboard,
	)

	// Generate report. Every downstream operation is bound to the snapshot
	// context and uses only snapshot configuration, identity and instance
	// generation resources.
	err = pdfReport.Generate(snap.ctx, w)
	if err != nil {
		ctxLogger.Error("error generating report", "err", err)

		if errors.Is(context.Cause(snap.ctx), errInstanceDisposed) {
			// The instance generation was torn down mid report. Report an
			// explicit, consistent failure instead of a mixed data view.
			http.Error(w, "report generation canceled because plugin instance reloaded", http.StatusServiceUnavailable)
		} else {
			http.Error(w, "error generating report", http.StatusInternalServerError)
		}

		return
	}

	ctxLogger.Info("report generated")
}

// errText returns the response body for a given status code.
func errText(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid request"
	case http.StatusForbidden:
		return "permission denied"
	default:
		return "error generating report"
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
