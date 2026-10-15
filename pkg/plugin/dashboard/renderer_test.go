package dashboard

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/chrome"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/config"
	. "github.com/smartystreets/goconvey/convey"
)

// We want our tests to run fast.
func init() {
	getPanelRetrySleepTime = time.Duration(1) * time.Millisecond
}

func newTestModel(uid string, variables url.Values) *Model {
	model := &Model{}
	model.Dashboard.UID = uid
	model.Dashboard.Version = 3
	model.Dashboard.Variables = variables

	return model
}

func TestFetchPanelPNG(t *testing.T) {
	Convey("When fetching a panel PNG", t, func() {
		requestURI := ""
		requestHeaders := http.Header{}

		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestURI = r.RequestURI
			requestHeaders = r.Header
		}))
		defer ts.Close()

		conf := config.Config{
			Layout:        "simple",
			DashboardMode: "default",
		}
		variables := url.Values{}
		variables.Add("var-host", "servername")
		variables.Add("var-port", "adapter")
		variables.Add("from", "now-1h")
		variables.Add("to", "now")

		// Relative from/to are resolved once against this fixed reference time
		// and pinned to epoch milliseconds for the whole snapshot.
		refTime := time.UnixMilli(1734198065000)

		snap, snapErr := NewSnapshot(newTestModel("randomUID", variables), refTime)

		dash, err := New(
			log.NewNullLogger(),
			&conf,
			http.DefaultClient,
			&chrome.LocalInstance{},
			ts.URL+"/grafana", // With path prefix
			"v11.1.0",
			snap,
			http.Header{
				backend.OAuthIdentityTokenHeaderName: []string{"Bearer token"},
			},
		)

		Convey("Snapshot should resolve relative times to one absolute window", func() {
			So(snapErr, ShouldBeNil)
			So(snap.To.UnixMilli(), ShouldEqual, int64(1734198065000))
			So(snap.From.UnixMilli(), ShouldEqual, int64(1734194465000))
			So(snap.Variables().Get("from"), ShouldEqual, "1734194465000")
			So(snap.Variables().Get("to"), ShouldEqual, "1734198065000")
		})

		Convey("New dashboard should receive no errors", func() {
			So(err, ShouldBeNil)
		})

		_, err = dash.PanelPNG(t.Context(), Panel{ID: "44", Type: "singlestat", Title: "title", GridPos: GridPos{}})

		Convey("It should receives no errors", func() {
			So(err, ShouldBeNil)
		})

		Convey("The httpClient should use the render endpoint with the dashboard name", func() {
			So(requestURI, ShouldStartWith, "/grafana/render/d-solo/randomUID/_")
		})

		Convey("The httpClient should request the panel ID", func() {
			So(requestURI, ShouldContainSubstring, "panelId=44")
		})

		Convey("The httpClient should request the pinned absolute time window", func() {
			So(requestURI, ShouldContainSubstring, "from=1734194465000")
			So(requestURI, ShouldContainSubstring, "to=1734198065000")
			So(requestURI, ShouldNotContainSubstring, "from=now-1h")
			So(requestURI, ShouldNotContainSubstring, "to=now")
		})

		Convey("The httpClient should insert auth token should in request header", func() {
			So(requestHeaders.Get("Authorization"), ShouldEqual, "Bearer token")
		})

		Convey("The httpClient should pass variables in the request parameters", func() {
			So(requestURI, ShouldContainSubstring, "var-host=servername")
			So(requestURI, ShouldContainSubstring, "var-port=adapter")
		})

		Convey("The httpClient should request singlestat panels at a smaller size", func() {
			So(requestURI, ShouldContainSubstring, "width=1000")
			So(requestURI, ShouldContainSubstring, "height=500")
		})

		// Use grid layout
		conf.Layout = "grid"

		dash, err = New(
			log.NewNullLogger(),
			&conf,
			http.DefaultClient,
			&chrome.LocalInstance{},
			ts.URL,
			"v11.1.0",
			snap,
			http.Header{
				backend.OAuthIdentityTokenHeaderName: []string{"token"},
			},
		)

		Convey("New dashboard should receive no errors using grid layout", func() {
			So(err, ShouldBeNil)
		})

		_, err = dash.PanelPNG(t.Context(), Panel{ID: "44", Type: "graph", Title: "title", GridPos: GridPos{H: 6, W: 24}})

		Convey("It should receives no errors using grid layout", func() {
			So(err, ShouldBeNil)
		})

		Convey("The httpClient should still use the same absolute time window", func() {
			So(requestURI, ShouldContainSubstring, "from=1734194465000")
			So(requestURI, ShouldContainSubstring, "to=1734198065000")
		})

		Convey("The httpClient should request singlestat panels at grid layout size", func() {
			So(requestURI, ShouldContainSubstring, "width=1536")
			So(requestURI, ShouldContainSubstring, "height=216")
		})
	})
}
