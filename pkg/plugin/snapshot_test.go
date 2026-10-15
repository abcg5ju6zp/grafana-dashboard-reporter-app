package plugin

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/httpclient"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	sdkconfig "github.com/grafana/grafana-plugin-sdk-go/config"
	"github.com/grafana/grafana-plugin-sdk-go/experimental/featuretoggles"
	"github.com/mahendrapaipuri/authlib/authz"
	plugin_config "github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newLightweightTestApp(t *testing.T, conf plugin_config.Config, semver string) *App {
	t.Helper()

	ctx, cancel := context.WithCancelCause(context.Background())

	t.Cleanup(func() { cancel(nil) })

	return &App{
		generation:    1,
		grafanaSemVer: semver,
		httpClient:    http.DefaultClient,
		conf:          conf,
		authzClients:  make(map[string]authz.EnforcementClient),
		instanceCtx:   ctx,
		cancelInstance: func(cause error) {
			cancel(cause)
		},
		ctxLogger: log.NewNullLogger(),
	}
}

// newSnapshotRequest builds a GET /report request carrying a plugin user and
// grafana config in its context.
func newSnapshotRequest(t *testing.T, ctx context.Context, cfgMap map[string]string, target string, headers map[string]string) *http.Request {
	t.Helper()

	if cfgMap != nil {
		ctx = sdkconfig.WithGrafanaConfig(ctx, sdkconfig.NewGrafanaCfg(cfgMap))
	}

	ctx = backend.WithPluginContext(ctx, backend.PluginContext{
		User: &backend.User{Login: "alice"},
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	require.NoError(t, err)

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	return req
}

func validTestConf() plugin_config.Config {
	return plugin_config.Config{
		Theme:         "light",
		Orientation:   "portrait",
		Layout:        "simple",
		DashboardMode: "default",
		ReportFormat:  "pdf",
		HTTPClientOptions: httpclient.Options{
			Timeouts: &httpclient.DefaultTimeoutOptions,
		},
	}
}

const testGrafanaURL = "https://grafana.example"

// testCfgMap returns a grafana config map with an AppURL plus overrides.
func testCfgMap(extra map[string]string) map[string]string {
	cfgMap := map[string]string{sdkconfig.AppURL: testGrafanaURL}
	maps.Copy(cfgMap, extra)

	return cfgMap
}

func TestSnapshotMissingDashUID(t *testing.T) {
	t.Parallel()

	app := newLightweightTestApp(t, validTestConf(), "v10.4.0")

	req := newSnapshotRequest(t, t.Context(), nil, "https://grafana.example/report", nil)

	_, status, err := app.newReportSnapshot(req)
	require.ErrorIs(t, err, errInvalidRequest)
	assert.Equal(t, http.StatusBadRequest, status)
}

func TestSnapshotInvalidQueryParameter(t *testing.T) {
	t.Parallel()

	app := newLightweightTestApp(t, validTestConf(), "v10.4.0")

	req := newSnapshotRequest(t, t.Context(),
		map[string]string{sdkconfig.AppURL: "https://grafana.example"},
		"https://grafana.example/report?dashUid=d1&theme=purple", nil)

	_, status, err := app.newReportSnapshot(req)
	require.Error(t, err)
	assert.Equal(t, http.StatusBadRequest, status)
}

func TestSnapshotAuthPriorityCookie(t *testing.T) {
	t.Parallel()

	conf := validTestConf()
	conf.Token = "explicit-token"

	app := newLightweightTestApp(t, conf, "v10.4.0")

	req := newSnapshotRequest(t, t.Context(),
		testCfgMap(map[string]string{sdkconfig.AppClientSecret: "sa-token"}),
		"https://grafana.example/report?dashUid=d1",
		map[string]string{backend.CookiesHeaderName: "grafana_session=cookie-value"})

	snap, status, err := app.newReportSnapshot(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)

	assert.Equal(t, "grafana_session=cookie-value",
		snap.authHeader.Get(backend.CookiesHeaderName))
	assert.Empty(t, snap.authHeader.Get(backend.OAuthIdentityTokenHeaderName))
}

func TestSnapshotAuthPriorityExplicitToken(t *testing.T) {
	t.Parallel()

	conf := validTestConf()
	conf.Token = "explicit-token"

	app := newLightweightTestApp(t, conf, "v10.4.0")

	req := newSnapshotRequest(t, t.Context(),
		testCfgMap(map[string]string{sdkconfig.AppClientSecret: "sa-token"}),
		"https://grafana.example/report?dashUid=d1", nil)

	snap, status, err := app.newReportSnapshot(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)

	assert.Empty(t, snap.authHeader.Get(backend.CookiesHeaderName))
	assert.Equal(t, "Bearer explicit-token",
		snap.authHeader.Get(backend.OAuthIdentityTokenHeaderName))
}

func TestSnapshotAuthPriorityServiceAccount(t *testing.T) {
	t.Parallel()

	app := newLightweightTestApp(t, validTestConf(), "v10.4.0")

	req := newSnapshotRequest(t, t.Context(),
		testCfgMap(map[string]string{sdkconfig.AppClientSecret: "sa-token"}),
		"https://grafana.example/report?dashUid=d1", nil)

	snap, status, err := app.newReportSnapshot(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)

	assert.Equal(t, "Bearer sa-token",
		snap.authHeader.Get(backend.OAuthIdentityTokenHeaderName))
}

// Token resolution must fail at entry, before any downstream work.
func TestSnapshotSATokenFailureStartsNoDownstream(t *testing.T) {
	t.Parallel()

	hits := 0

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++

		w.WriteHeader(http.StatusOK)
	}))

	defer ts.Close()

	app := newLightweightTestApp(t, validTestConf(), "v10.4.0")

	// Explicitly empty secret with no cookie/token: deterministic failure.
	req := newSnapshotRequest(t, t.Context(),
		map[string]string{
			sdkconfig.AppClientSecret: "",
			sdkconfig.AppURL:          ts.URL,
		},
		ts.URL+"/report?dashUid=d1", nil)

	_, status, err := app.newReportSnapshot(req)
	require.Error(t, err)
	assert.Equal(t, http.StatusInternalServerError, status)
	assert.Equal(t, 0, hits, "no downstream request must be made on token failure")
}

// The snapshot config is a deep copy. Mutating the instance config after the
// snapshot is taken must not change the report's frozen options.
func TestSnapshotConfigIsDeepCopied(t *testing.T) {
	t.Parallel()

	conf := validTestConf()
	conf.IncludePanelIDs = []string{"1"}
	conf.ExcludePanelIDs = []string{"2"}
	conf.IncludePanelDataIDs = []string{"3"}

	app := newLightweightTestApp(t, conf, "v10.4.0")

	req := newSnapshotRequest(t, t.Context(),
		testCfgMap(map[string]string{sdkconfig.AppClientSecret: "sa-token"}),
		"https://grafana.example/report?dashUid=d1&theme=dark", nil)

	snap, status, err := app.newReportSnapshot(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)

	assert.Equal(t, "dark", snap.conf.Theme)
	assert.Equal(t, []string{"1"}, snap.conf.IncludePanelIDs)

	// Mutate the shared instance configuration after the snapshot was frozen
	app.conf.IncludePanelIDs[0] = "99"
	app.conf.IncludePanelIDs = append(app.conf.IncludePanelIDs, "100")
	app.conf.ExcludePanelIDs[0] = "99"
	app.conf.IncludePanelDataIDs[0] = "99"
	app.conf.Token = "rotated-token"

	assert.Equal(t, []string{"1"}, snap.conf.IncludePanelIDs)
	assert.Equal(t, []string{"2"}, snap.conf.ExcludePanelIDs)
	assert.Equal(t, []string{"3"}, snap.conf.IncludePanelDataIDs)
	assert.Empty(t, snap.conf.Token)

	snap.close()
}

// Query parameters are frozen as parsed at entry.
func TestSnapshotFrozenUserURLAndQuery(t *testing.T) {
	t.Parallel()

	app := newLightweightTestApp(t, validTestConf(), "v10.4.0")

	req := newSnapshotRequest(t, t.Context(),
		testCfgMap(map[string]string{sdkconfig.AppClientSecret: "sa-token"}),
		"https://grafana.example/report?dashUid=myDash&var-host=a&from=now-1h", nil)

	snap, _, err := app.newReportSnapshot(req)
	require.NoError(t, err)

	defer snap.close()

	assert.Equal(t, "alice", snap.user)
	assert.Equal(t, "myDash", snap.dashboardUID)
	assert.Equal(t, "https://grafana.example", snap.grafanaAppURL)
	assert.Equal(t, "a", snap.query.Get("var-host"))
	assert.Equal(t, uint64(1), snap.generation)
}

// With authz enabled (Grafana >= 11.6.0 toggles are always on), a missing id
// token must fail before any model or panel is fetched.
func TestSnapshotAuthzMissingIDTokenFailsEarly(t *testing.T) {
	t.Parallel()

	app := newLightweightTestApp(t, validTestConf(), "v11.6.0")

	req := newSnapshotRequest(t, t.Context(),
		testCfgMap(map[string]string{sdkconfig.AppClientSecret: "sa-token"}),
		"https://grafana.example/report?dashUid=d1", nil)

	_, status, err := app.newReportSnapshot(req)
	require.ErrorIs(t, err, errPermissionDenied)
	assert.Equal(t, http.StatusForbidden, status)
}

// On 10.4.4..11.2.0 both feature toggles must be present for authz to be on.
func TestSnapshotAuthzFeatureToggles(t *testing.T) {
	t.Parallel()

	headers := map[string]string{GrafanaUserSignInTokenHeaderName: "id-token"}

	cases := []struct {
		name    string
		semver  string
		toggles string
		enabled bool
	}{
		{"old grafana uses cookies, authz off", "v10.4.3", "", false},
		{"both toggles enabled", "v11.1.0", "accessControlOnCall,idForwarding", true},
		{"missing one toggle", "v11.1.0", "accessControlOnCall", false},
		{"11.2+ only accessControl required", "v11.2.0", "accessControlOnCall", true},
		{"11.6+ toggles always on", "v11.6.0", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app := newLightweightTestApp(t, validTestConf(), tc.semver)

			cfgMap := testCfgMap(map[string]string{sdkconfig.AppClientSecret: "sa-token"})
			if tc.toggles != "" {
				cfgMap[featuretoggles.EnabledFeatures] = tc.toggles
			}

			req := newSnapshotRequest(t, t.Context(), cfgMap,
				"https://grafana.example/report?dashUid=d1", headers)

			snap, status, err := app.newReportSnapshot(req)
			if tc.enabled {
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, status)
				assert.True(t, snap.authzEnabled)
				assert.NotNil(t, snap.authzClient)
				snap.close()
			} else {
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, status)
				assert.False(t, snap.authzEnabled)
				assert.Nil(t, snap.authzClient)
				snap.close()
			}
		})
	}
}

// The authz client is pinned to the frozen token: a rotation observed by a
// later request never replaces the client of an earlier snapshot.
func TestSnapshotAuthzClientPinnedToToken(t *testing.T) {
	t.Parallel()

	app := newLightweightTestApp(t, validTestConf(), "v11.6.0")

	headers := map[string]string{GrafanaUserSignInTokenHeaderName: "id-token"}

	build := func(secret string) *reportSnapshot {
		req := newSnapshotRequest(t, t.Context(),
			testCfgMap(map[string]string{sdkconfig.AppClientSecret: secret}),
			"https://grafana.example/report?dashUid=d1", headers)

		snap, status, err := app.newReportSnapshot(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, status)

		return snap
	}

	snap1 := build("sa-token-1")
	snap2 := build("sa-token-2")
	snap3 := build("sa-token-1")

	assert.NotSame(t, snap1.authzClient, snap2.authzClient)
	assert.Same(t, snap1.authzClient, snap3.authzClient,
		"a snapshot must always get the client for its own frozen token")

	snap1.close()
	snap2.close()
	snap3.close()
}

// Canceling the request cancels the snapshot context.
func TestSnapshotRequestContextCancel(t *testing.T) {
	t.Parallel()

	app := newLightweightTestApp(t, validTestConf(), "v10.4.0")

	reqCtx, cancelReq := context.WithCancel(t.Context())
	req := newSnapshotRequest(t, reqCtx,
		testCfgMap(map[string]string{sdkconfig.AppClientSecret: "sa-token"}),
		"https://grafana.example/report?dashUid=d1", nil)

	snap, _, err := app.newReportSnapshot(req)
	require.NoError(t, err)

	defer snap.close()

	cancelReq()

	select {
	case <-snap.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("snapshot context not canceled with request")
	}
}

// When the owning instance generation is canceled during disposal, the
// snapshot is explicitly canceled with the disposed cause.
//
// This test mutates the package level inFlightDrainTimeout and therefore must
// not run in parallel with other disposal tests.
func TestSnapshotCanceledOnInstanceDisposal(t *testing.T) {
	app := newLightweightTestApp(t, validTestConf(), "v10.4.0")
	app.httpClient = http.DefaultClient
	app.workerPools = nil
	app.chromeInstance = nil

	// Speed up the drain timeout for this test
	original := inFlightDrainTimeout

	inFlightDrainTimeout = 100 * time.Millisecond

	t.Cleanup(func() { inFlightDrainTimeout = original })

	req := newSnapshotRequest(t, t.Context(),
		testCfgMap(map[string]string{sdkconfig.AppClientSecret: "sa-token"}),
		"https://grafana.example/report?dashUid=d1", nil)

	snap, _, err := app.newReportSnapshot(req)
	require.NoError(t, err)

	defer snap.close()

	require.True(t, app.beginRequest(), "request accepted before disposal")
	// Intentionally do not call inFlight.Done() until after Dispose

	finished := make(chan struct{})

	go func() {
		app.Dispose()
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Dispose did not return after drain timeout")
	}

	select {
	case <-snap.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("snapshot context not explicitly canceled on disposal")
	}

	require.ErrorIs(t, context.Cause(snap.ctx), errInstanceDisposed)

	// New requests must not start on the disposed generation
	assert.False(t, app.beginRequest())

	app.inFlight.Done()
}

// In-flight reports that finish during the drain period let Dispose return
// promptly without canceling them.
//
// This test mutates the package level inFlightDrainTimeout and therefore must
// not run in parallel with other disposal tests.
func TestDisposeWaitsForInFlight(t *testing.T) {
	app := newLightweightTestApp(t, validTestConf(), "v10.4.0")

	original := inFlightDrainTimeout

	inFlightDrainTimeout = 10 * time.Second

	t.Cleanup(func() { inFlightDrainTimeout = original })

	require.True(t, app.beginRequest())

	go func() {
		time.Sleep(100 * time.Millisecond)
		app.inFlight.Done()
	}()

	done := make(chan struct{})

	go func() {
		app.Dispose()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Dispose should return once in-flight reports complete")
	}

	assert.False(t, app.beginRequest())
}

// The handler responds 503 if the generation is already disposing.
func TestHandlerRejectsWhenDisposing(t *testing.T) {
	t.Parallel()

	app := newLightweightTestApp(t, validTestConf(), "v10.4.0")
	app.disposing.Store(true)

	req := newSnapshotRequest(t, t.Context(),
		testCfgMap(map[string]string{sdkconfig.AppClientSecret: "sa-token"}),
		"https://grafana.example/report?dashUid=d1", nil)

	rec := httptest.NewRecorder()
	app.handleReport(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// Downstream 401/403 while fetching the dashboard model must surface as a
// consistent 403 failure rather than retrying under another identity.
func TestHandlerUpstreamAuthFailureIsConsistent(t *testing.T) {
	t.Parallel()

	for _, upstreamStatus := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(upstreamStatus), func(t *testing.T) {
			t.Parallel()

			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(upstreamStatus)
			}))
			defer ts.Close()

			app := newLightweightTestApp(t, validTestConf(), "v10.4.0")

			req := newSnapshotRequest(t, t.Context(),
				map[string]string{sdkconfig.AppURL: ts.URL},
				ts.URL+"/report?dashUid=d1",
				map[string]string{backend.CookiesHeaderName: "grafana_session=cookie"})

			rec := httptest.NewRecorder()
			app.handleReport(rec, req)

			if upstreamStatus == http.StatusNotFound {
				assert.Equal(t, http.StatusInternalServerError, rec.Code)
			} else {
				assert.Equal(t, http.StatusForbidden, rec.Code)
			}
		})
	}
}
