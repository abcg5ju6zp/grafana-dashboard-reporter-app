package dashboard

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/chrome"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/config"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/helpers"
)

// Embed the entire directory.
//
//go:embed js
var jsFS embed.FS

// New creates a new instance of the Dashboard struct. The dashboard is bound
// to an immutable snapshot that carries the exact model version and the
// absolute time window the whole report must be built from.
func New(logger log.Logger, conf *config.Config, httpClient *http.Client, chromeInstance chrome.Instance,
	appURL, appVersion string, snapshot *Snapshot, authHeader http.Header,
) (*Dashboard, error) {
	// Parse app URL
	u, err := url.Parse(appURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse app URL: %w", errors.Unwrap(err))
	}

	// Read JS from embedded file
	js, err := jsFS.ReadFile("js/panels.js")
	if err != nil {
		return nil, fmt.Errorf("failed to load JS: %w", err)
	}

	return &Dashboard{
		logger,
		conf,
		httpClient,
		chromeInstance,
		u,
		appVersion,
		string(js),
		snapshot,
		authHeader,
		nil,
	}, nil
}

// SetVersionFetcher injects the function used to read the dashboard version
// currently stored in Grafana. When configured, CheckVersion can detect that
// the dashboard was saved after the snapshot was taken.
func (d *Dashboard) SetVersionFetcher(fetcher func(ctx context.Context) (int, error)) {
	d.versionFetcher = fetcher
}

// ReplaceSnapshot rebinds the dashboard to a freshly built snapshot. It is used
// by the report rebuild loop after a dashboard version change has been
// detected; the previous in-flight candidate is discarded by the caller.
func (d *Dashboard) ReplaceSnapshot(snapshot *Snapshot) {
	d.snapshot = snapshot
}

// CheckVersion verifies that the dashboard version currently stored in Grafana
// still matches the version recorded by the snapshot. It returns
// ErrDashboardVersionChanged when they differ. When no version fetcher has
// been configured (for instance in unit tests), the check is a no-op.
func (d *Dashboard) CheckVersion(ctx context.Context) error {
	if d.versionFetcher == nil {
		return nil
	}

	currentVersion, err := d.versionFetcher(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch current dashboard version: %w", err)
	}

	if currentVersion != d.snapshot.Version() {
		d.logger.Warn(
			"dashboard version changed since snapshot; discarding in-flight report candidate",
			"snapshot_version", d.snapshot.Version(),
			"current_version", currentVersion,
		)

		return fmt.Errorf(
			"snapshot version %d, current dashboard version %d: %w",
			d.snapshot.Version(),
			currentVersion,
			ErrDashboardVersionChanged,
		)
	}

	return nil
}

// GetData fetches dashboard related data.
func (d *Dashboard) GetData(ctx context.Context) (*Data, error) {
	defer helpers.TimeTrack(time.Now(), "dashboard data", d.logger)

	// Make panels from loading the dashboard in a browser instance
	panels, err := d.panels(ctx)
	if err != nil {
		d.logger.Error("error collecting panels from browser", "error", err)

		return nil, fmt.Errorf("error collecting panels from browser: %w", err)
	}

	return &Data{
		Title:       d.snapshot.model.Dashboard.Title,
		TimeRange:   d.snapshot.TimeRange(),
		Variables:   variablesValues(d.snapshot.model.Dashboard.Variables),
		Panels:      panels,
		GeneratedAt: d.snapshot.FetchedAt,
	}, err
}

// variablesValues returns current dashboard template variables and their values as
// a string.
func variablesValues(queryParams url.Values) string {
	values := []string{}

	for k, v := range queryParams {
		if strings.HasPrefix(k, "var-") {
			n := strings.Split(k, "var-")[1]
			values = append(values, fmt.Sprintf("%s=%s", n, strings.Join(v, ",")))
		}
	}

	return strings.Join(values, "; ")
}
