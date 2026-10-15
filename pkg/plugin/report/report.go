package report

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/sprig/v3"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/chrome"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/config"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/dashboard"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/helpers"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/worker"
)

// Embed the entire directory.
//
//go:embed templates
var templateFS embed.FS

// Base64 content signatures.
var popularSignatures = map[string]string{
	"JVBERi0":     "application/pdf",
	"R0lGODdh":    "image/gif",
	"R0lGODlh":    "image/gif",
	"iVBORw0KGgo": "image/png",
	"/9j/":        "image/jpg",
	"Qk02U":       "image/bmp",
}

// ErrReportConflict is returned when the dashboard keeps changing while a
// report is being generated and the request deadline is reached. No stale
// candidate is ever published; the client must retry the report.
var ErrReportConflict = errors.New("dashboard modified during report generation; please retry")

func New(logger log.Logger, conf *config.Config, httpClient *http.Client, chromeInstance chrome.Instance,
	pools worker.Pools, dashboard dashboardReport,
) *Report {
	return &Report{
		logger,
		conf,
		httpClient,
		chromeInstance,
		pools,
		dashboard,
		nil,
	}
}

// Generate builds the report and writes it to the response writer.
//
// A complete candidate (panel metadata, PNGs and CSV data) is assembled
// against a single immutable snapshot: one dashboard model version and one
// absolute time window. If the dashboard is saved at any point while a
// candidate is being assembled, the whole candidate is discarded and a new
// candidate is rebuilt from scratch against the latest version. Panels from
// different versions are therefore never mixed into one report. If the
// dashboard keeps changing until the request deadline, ErrReportConflict is
// returned and nothing is written to the response.
func (r *Report) Generate(ctx context.Context, writer http.ResponseWriter) error {
	defer helpers.TimeTrack(time.Now(), "report generation", r.logger)

	var (
		htmlReport    HTML
		dashboardData *dashboard.Data
	)

	// attempts counts discarded candidates, i.e. how many times the dashboard
	// version changed during generation.
	attempts := 0

	for {
		// Build a complete candidate bound to the current snapshot.
		data, err := r.buildCandidate(ctx)
		if err == nil {
			dashboardData = data

			htmlReport, err = r.generateHTMLFile(data)
			if err != nil {
				return fmt.Errorf("failed to generate HTML file: %w", err)
			}

			// The candidate is fully assembled in memory and still consistent.
			// From this point on Grafana is not queried anymore, so it is safe
			// to publish.
			break
		}

		if !errors.Is(err, dashboard.ErrDashboardVersionChanged) {
			// If the request expired while chasing a continuously modified
			// dashboard, surface a conflict instead of any stale candidate.
			if attempts > 0 && ctx.Err() != nil {
				return ErrReportConflict
			}

			return err
		}

		// Dashboard version changed: discard this candidate, rotate the
		// snapshot to the latest version and rebuild a complete candidate.
		err = r.rebuildSnapshot(ctx)
		if err != nil {
			return err
		}

		attempts++
	}

	if r.conf.ReportFormat == "html" {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(writer, htmlReport.Body)
	} else {
		// Sanitize title to escape non ASCII characters
		// Ref: https://stackoverflow.com/questions/62705546/unicode-characters-in-attachment-name
		// Ref: https://medium.com/@JeremyLaine/non-ascii-content-disposition-header-in-django-3a20acc05f0d
		filename := url.PathEscape(dashboardData.Title)
		header := fmt.Sprintf(`inline; filename*=UTF-8''%s.pdf`, filename)
		writer.Header().Add("Content-Disposition", header)

		err := r.renderPDF(htmlReport, writer)
		if err != nil {
			return fmt.Errorf("failed to render PDF: %w", err)
		}
	}

	return nil
}

// buildCandidate assembles one complete report candidate from the current
// snapshot. Panel metadata, PNG and CSV downloads all come from the same
// model version and absolute time window. The dashboard version is checked at
// every stage boundary; on mismatch the caller discards the candidate and
// rebuilds against a fresh snapshot.
func (r *Report) buildCandidate(ctx context.Context) (*dashboard.Data, error) {
	// Get panel data from dashboard
	dashboardData, err := r.dashboard.GetData(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get dashboard data: %w", err)
	}

	// The browser may have rendered a dashboard saved after the snapshot was
	// read. Never mix the snapshot model with a newer layout.
	err = r.dashboard.CheckVersion(ctx)
	if err != nil {
		return nil, err
	}

	// Populate panels with PNG and tabular data
	populateErr := r.populatePanels(ctx, dashboardData)
	if populateErr != nil {
		return nil, fmt.Errorf("failed to populate panels: %w", populateErr)
	}

	// PNG/CSV downloads reopen the dashboard many times and can take long.
	// Before any result is published, verify that the dashboard has not been
	// saved in the meantime. If it has, all panels of this attempt are
	// invalidated and the candidate is discarded.
	err = r.dashboard.CheckVersion(ctx)
	if err != nil {
		return nil, err
	}

	return dashboardData, nil
}

// rebuildSnapshot replaces the current snapshot with a freshly read one after
// a dashboard version change has been detected. Permission denials while
// rebuilding fail the report directly, and a request that expires while the
// dashboard keeps changing results in ErrReportConflict rather than in an old
// report.
func (r *Report) rebuildSnapshot(ctx context.Context) error {
	// Do not rebuild past the request deadline.
	ctxErr := ctx.Err()
	if ctxErr != nil {
		r.logger.Warn("request deadline reached while dashboard keeps changing; returning conflict")

		return ErrReportConflict
	}

	// Without a snapshot builder the report cannot be rebuilt safely, so never
	// fall back to stale data.
	if r.newSnapshot == nil {
		return ErrReportConflict
	}

	snapshot, err := r.newSnapshot(ctx)
	if err != nil {
		if ctx.Err() != nil {
			r.logger.Warn("request deadline reached while rebuilding snapshot; returning conflict")

			return ErrReportConflict
		}

		return fmt.Errorf("failed to rebuild report snapshot: %w", err)
	}

	r.dashboard.ReplaceSnapshot(snapshot)

	r.logger.Info("rebuilt report candidate from new dashboard snapshot", "dashboard_version", snapshot.Version())

	return nil
}

// populatePanels populates the panels with PNG and tabular data.
func (r *Report) populatePanels(ctx context.Context, dashboardData *dashboard.Data) error {
	defer helpers.TimeTrack(time.Now(), "panel PNGs and/or data generation", r.logger)

	// Get the indexes of PNG panels that need to be included in the report
	pngPanels := selectPanels(dashboardData.Panels, r.conf.IncludePanelIDs, r.conf.ExcludePanelIDs, true)

	// Get the indexes of table panels that need to be included in the report
	tablePanels := selectPanels(dashboardData.Panels, r.conf.IncludePanelDataIDs, nil, false)

	errorCh := make(chan error, len(pngPanels)+len(tablePanels))

	wg := sync.WaitGroup{}

	for idx, panel := range dashboardData.Panels {
		if slices.Contains(pngPanels, idx) {
			wg.Add(1)

			r.pools[worker.Renderer].Do(func() {
				defer wg.Done()

				panelPNG, err := r.dashboard.PanelPNG(ctx, panel)
				if err != nil {
					errorCh <- fmt.Errorf("failed to fetch PNG data for panel %s: %w", panel.ID, err)
				}

				dashboardData.Panels[idx].EncodedImage = panelPNG
			})
		}

		if slices.Contains(tablePanels, idx) {
			wg.Add(1)

			r.pools[worker.Browser].Do(func() {
				defer wg.Done()

				panelData, err := r.dashboard.PanelCSV(ctx, panel)
				if err != nil && !errors.Is(err, dashboard.ErrEmptyPanelElement) {
					errorCh <- fmt.Errorf("failed to fetch CSV data for panel %s: %w", panel.ID, err)
				}

				dashboardData.Panels[idx].CSVData = panelData
			})
		}
	}

	wg.Wait()
	close(errorCh)

	errs := make([]error, 0, len(pngPanels)+len(tablePanels))

	for err := range errorCh {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to generate report: %w", errors.Join(errs...))
	}

	return nil
}

// generateHTMLFile generates HTML files for PDF.
func (r *Report) generateHTMLFile(dashboardData *dashboard.Data) (HTML, error) {
	var tmpl *template.Template

	var html HTML

	var err error

	// Template functions
	localFuncMap := template.FuncMap{
		// The name "inc" is what the function will be called in the template text.
		"inc": func(i int) int {
			return i + 1
		},

		"add": func(i float64) float64 {
			return i + 1
		},

		"mult": func(i int) int {
			return i*30 + 5
		},

		"embed": func(base64Content string) template.URL {
			for signature, mimeType := range popularSignatures {
				if strings.HasPrefix(base64Content, signature) {
					return template.URL(template.HTMLEscapeString(fmt.Sprintf("data:%s;base64,%s", mimeType, base64Content))) //nolint:gosec
				}
			}

			return template.URL(template.HTMLEscapeString(base64Content)) //nolint:gosec
		},

		"url": func(url string) template.URL {
			return template.URL(template.HTMLEscapeString(url)) //nolint:gosec
		},
	}

	// Include functions from sprig
	funcMap := sprig.FuncMap()

	// Add local funcs to map
	maps.Copy(funcMap, localFuncMap)

	// Make a new template for Body of the PDF
	tmpl, err = template.New("report").Funcs(funcMap).ParseFS(templateFS, "templates/report.gohtml")
	if err != nil {
		return HTML{}, fmt.Errorf("error parsing PDF template: %w", err)
	}

	// Use the snapshot reference instant for the header so that the displayed
	// generation time comes from the same observation moment as the rest of the
	// report. Fall back to the current time only when no snapshot timestamp is
	// available (for instance in unit tests).
	generatedAt := dashboardData.GeneratedAt
	if generatedAt.IsZero() {
		generatedAt = time.Now()
	}

	// Template data
	data := templateData{
		generatedAt.Local().In(r.conf.Location).Format(r.conf.TimeFormat),
		dashboardData,
		r.conf,
	}

	// Render the template for Body of the PDF
	bufBody := &bytes.Buffer{}

	err = tmpl.ExecuteTemplate(bufBody, "report.gohtml", data)
	if err != nil {
		return HTML{}, fmt.Errorf("error executing PDF template: %w", err)
	}

	html.Body = bufBody.String()

	// Make a new template for Header of the PDF
	if r.conf.HeaderTemplate != "" {
		tmpl, err = template.New("header").Funcs(funcMap).Parse(fmt.Sprintf(`{{define "header.gohtml"}}%s{{end}}`, r.conf.HeaderTemplate))
	} else {
		tmpl, err = template.New("header").Funcs(funcMap).ParseFS(templateFS, "templates/header.gohtml")
	}

	if err != nil {
		return HTML{}, fmt.Errorf("error parsing Header template: %w", err)
	}

	// Render the template for Header of the PDF
	bufHeader := &bytes.Buffer{}

	err = tmpl.ExecuteTemplate(bufHeader, "header.gohtml", data)
	if err != nil {
		return HTML{}, fmt.Errorf("error executing Header template: %w", err)
	}

	html.Header = bufHeader.String()

	// Make a new template for Footer of the PDF
	if r.conf.FooterTemplate != "" {
		tmpl, err = template.New("footer").Funcs(funcMap).Parse(fmt.Sprintf(`{{define "footer.gohtml"}}%s{{end}}`, r.conf.FooterTemplate))
	} else {
		tmpl, err = template.New("footer").Funcs(funcMap).ParseFS(templateFS, "templates/footer.gohtml")
	}

	if err != nil {
		return HTML{}, fmt.Errorf("error parsing Footer template: %w", err)
	}

	// Render the template for Footer of the PDF
	bufFooter := &bytes.Buffer{}

	err = tmpl.ExecuteTemplate(bufFooter, "footer.gohtml", data)
	if err != nil {
		return HTML{}, fmt.Errorf("error executing Footer template: %w", err)
	}

	html.Footer = bufFooter.String()

	return html, nil
}

// renderPDF renders HTML page into PDF using Chromium.
func (r *Report) renderPDF(htmlReport HTML, writer io.Writer) error {
	defer helpers.TimeTrack(time.Now(), "pdf rendering", r.logger)

	// Create a new tab
	tab := r.chromeInstance.NewTab(r.logger, r.conf)
	defer tab.Close(r.logger)

	err := tab.PrintToPDF(chrome.PDFOptions{
		Header:      htmlReport.Header,
		Body:        htmlReport.Body,
		Footer:      htmlReport.Footer,
		Orientation: r.conf.Orientation,
	}, writer)
	if err != nil {
		return fmt.Errorf("error rendering PDF: %w", err)
	}

	return nil
}
