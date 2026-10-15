package plugin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	sdkconfig "github.com/grafana/grafana-plugin-sdk-go/config"
	"github.com/mahendrapaipuri/authlib/authz"
	plugin_config "github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/config"
)

// Sentinel errors used to map upstream failures onto consistent HTTP responses.
var (
	errInvalidRequest   = errors.New("invalid report request")
	errPermissionDenied = errors.New("permission denied")
)

// apiStatusError is returned when an upstream Grafana API responds with a
// non 2xx status. It carries the status code so the handler can answer with a
// consistent status without ever retrying under a different identity.
type apiStatusError struct {
	url        string
	statusCode int
	status     string
	body       string
}

func (e *apiStatusError) Error() string {
	return fmt.Sprintf("upstream request failed: URL: %s. Status: %s, message: %s", e.url, e.status, e.body)
}

// reportSnapshot is the immutable, per-request view of everything a single
// report generation is allowed to use. It is frozen at the handler entry:
//
//   - the requesting user
//   - the Grafana app URL
//   - the data plane authentication header (cookie, explicit token or
//     service account token, in the existing precedence order)
//   - the report options (a deep copy of the instance config with query
//     parameter overrides applied)
//   - the feature toggle state
//   - the authorization client and the user's id token
//   - the plugin instance generation and a context tied to that generation
//
// Dashboard model and parent folder reads, permission checks, panel PNG/CSV
// fetches and Chrome PDF rendering MUST exclusively use the snapshot. Config
// updates, token rotations or instance disposal happening after the snapshot
// is taken can never alter the report's data view.
type reportSnapshot struct {
	app *App

	// ctx is canceled when the request is done or when the owning plugin
	// instance generation is explicitly canceled during disposal.
	ctx    context.Context
	cancel context.CancelCauseFunc
	// stopReqChain detaches the request-context cancellation hook.
	stopReqChain func() bool

	logger     log.Logger
	generation uint64

	user         string
	dashboardUID string
	query        url.Values

	grafanaAppURL string
	conf          plugin_config.Config
	authHeader    http.Header

	// Authorization state. authzClient is non nil iff authzEnabled is true and
	// the client for the frozen service account token has been built
	// successfully before any downstream work started.
	idToken      string
	authzEnabled bool
	authzClient  authz.EnforcementClient
}

// close releases the lifecycle resources associated with the snapshot. Safe
// to call multiple times.
func (s *reportSnapshot) close() {
	if s.stopReqChain != nil {
		s.stopReqChain()
	}

	if s.cancel != nil {
		s.cancel(nil)
	}
}

// hasAccess checks that the frozen requesting user is allowed to perform
// action on all the given resources. It always uses the id token and the
// authorization client captured at entry and never re-reads the instance
// configuration or re-resolves a token.
func (s *reportSnapshot) hasAccess(action string, resources ...authz.Resource) (bool, error) {
	hasAccess, err := s.authzClient.HasAccess(s.ctx, s.idToken, action, resources...)
	if err != nil || !hasAccess {
		return false, err
	}

	return true, nil
}

// cloneConfig returns a deep enough copy of the plugin config so that slices
// owned by the instance's shared config can never be mutated through the
// per-request snapshot.
func cloneConfig(conf plugin_config.Config) plugin_config.Config {
	clone := conf
	clone.IncludePanelIDs = slices.Clone(conf.IncludePanelIDs)
	clone.ExcludePanelIDs = slices.Clone(conf.ExcludePanelIDs)
	clone.IncludePanelDataIDs = slices.Clone(conf.IncludePanelDataIDs)

	return clone
}

// newReportSnapshot freezes all request state at the handler entry point.
// It returns the HTTP status code that must be sent when error is non nil.
//
// Token resolution and authorization client construction happen here. A
// failure guarantees that no downstream work (model fetch, panel rendering,
// PDF rendering) has been started.
func (app *App) newReportSnapshot(req *http.Request) (*reportSnapshot, int, error) {
	ctxLogger := log.DefaultLogger.FromContext(req.Context())

	// Get Dashboard ID
	dashboardUID := req.URL.Query().Get("dashUid")
	if dashboardUID == "" {
		ctxLogger.Debug("Query parameter dashUid not found")

		return nil, http.StatusBadRequest, fmt.Errorf("%w: missing dashUid query parameter", errInvalidRequest)
	}

	// Freeze the requesting user from the request context
	pluginConfig := backend.PluginConfigFromContext(req.Context())
	currentUser := pluginConfig.User.Login
	ctxLogger = ctxLogger.With("user", currentUser, "dash_uid", dashboardUID)

	// Parse query parameters once and reuse that frozen copy
	query := req.URL.Query()

	// Freeze the Grafana App URL. User configured URL has higher precedence
	// than the App URL in the request's context.
	grafanaConfig := sdkconfig.GrafanaConfigFromContext(req.Context())

	grafanaAppURL, err := app.grafanaAppURL(grafanaConfig)
	if err != nil {
		ctxLogger.Error("failed to get app URL", "err", err)

		return nil, http.StatusInternalServerError, fmt.Errorf("failed to get Grafana app URL: %w", err)
	}

	// Deep copy the current generation's config and apply request overrides on
	// the copy. The instance config itself is never touched.
	conf := cloneConfig(app.conf)
	app.updateConfigFromValues(query, &conf)

	// Validate the frozen config
	err = conf.RTValidate()
	if err != nil {
		ctxLogger.Debug("invalid config: "+conf.String(), "err", err)

		return nil, http.StatusBadRequest, fmt.Errorf("%w: %w", errInvalidRequest, err)
	}

	ctxLogger.Info("generate report using config: " + conf.String())

	// Freeze feature toggle state once for the whole report.
	authzEnabled := app.featureTogglesEnabled(req.Context())

	// Resolve the per-request service account token at entry. The secret is
	// provisioned by Grafana in the request context and can be rotated between
	// requests. This is the only place where it is read.
	saToken, saTokenErr := grafanaConfig.PluginAppClientSecret()

	// Build the data plane authentication header. The historical precedence
	// (cookie > explicitly configured token > service account token) is kept
	// unchanged for compatibility, including cookie based auth on older
	// Grafana and reports for public/anonymous dashboards.
	authHeader := http.Header{}

	switch {
	case req.Header.Get(backend.CookiesHeaderName) != "":
		ctxLogger.Debug("using user cookie")

		authHeader.Add(backend.CookiesHeaderName, req.Header.Get(backend.CookiesHeaderName))
	case conf.Token != "":
		ctxLogger.Debug("using user configured token")

		authHeader.Add(backend.OAuthIdentityTokenHeaderName, "Bearer "+conf.Token)
	default:
		ctxLogger.Debug("using service account token")

		if saTokenErr != nil {
			ctxLogger.Error("failed to get plugin app client secret", "err", saTokenErr)

			return nil, http.StatusInternalServerError, fmt.Errorf("failed to get service account token: %w", saTokenErr)
		}

		if saToken == "" {
			ctxLogger.Error("failed to get plugin app client secret", "err", "empty client secret")

			return nil, http.StatusInternalServerError, errors.New("empty service account token")
		}

		authHeader.Add(backend.OAuthIdentityTokenHeaderName, "Bearer "+saToken)
	}

	// When authorization checks are enabled, resolve the user's id token and
	// build the authorization client before any downstream work starts. A
	// failure here aborts the report instead of starting model/panel fetches.
	var (
		idToken     string
		authzClient authz.EnforcementClient
	)

	if authzEnabled {
		idToken = req.Header.Get(GrafanaUserSignInTokenHeaderName)
		if idToken == "" {
			ctxLogger.Error("id token not found")

			return nil, http.StatusForbidden, fmt.Errorf("%w: id token not found", errPermissionDenied)
		}

		// The enforcement client authenticates with the service account token
		// and falls back to the explicitly configured token, matching the
		// historical behavior.
		authzToken := saToken

		if saTokenErr != nil && conf.Token == "" {
			ctxLogger.Error("failed to fetch service account and configured token", "err", saTokenErr)

			return nil, http.StatusForbidden, fmt.Errorf("failed to resolve authorization token: %w", saTokenErr)
		}

		if authzToken == "" {
			authzToken = conf.Token
		}

		authzClient, err = app.authzClientFor(grafanaAppURL, authzToken)
		if err != nil {
			ctxLogger.Error("failed to initialize authz client", "err", err)

			return nil, http.StatusForbidden, fmt.Errorf("failed to initialize authorization client: %w", err)
		}
	}

	// Tie the report to the owning instance generation. The snapshot context
	// is canceled either when the HTTP request ends or when the instance
	// generation is explicitly canceled during disposal.
	ctx, cancel := context.WithCancelCause(app.instanceCtx)
	stopReqChain := context.AfterFunc(req.Context(), func() {
		cancel(context.Cause(req.Context()))
	})

	return &reportSnapshot{
		app:           app,
		ctx:           ctx,
		cancel:        cancel,
		stopReqChain:  stopReqChain,
		logger:        ctxLogger,
		generation:    app.generation,
		user:          currentUser,
		dashboardUID:  dashboardUID,
		query:         query,
		grafanaAppURL: grafanaAppURL,
		conf:          conf,
		authHeader:    authHeader,
		idToken:       idToken,
		authzEnabled:  authzEnabled,
		authzClient:   authzClient,
	}, http.StatusOK, nil
}
