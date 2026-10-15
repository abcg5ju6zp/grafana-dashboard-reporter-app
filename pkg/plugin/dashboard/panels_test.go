package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/httpclient"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/chrome"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/config"
	. "github.com/smartystreets/goconvey/convey"
)

var muLock sync.RWMutex

// testSnapshot builds a snapshot for a dashboard UID without explicit
// variables; relative from/to still get pinned once.
func testSnapshot(uid string) *Snapshot {
	model := &Model{}
	model.Dashboard.UID = uid

	snap, err := NewSnapshot(model, time.UnixMilli(1734198065000))
	if err != nil {
		panic(err)
	}

	return snap
}

func TestDashboardFetchWithLocalChrome(t *testing.T) {
	var execPath string

	locations := []string{
		// Mac
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		// Windows
		"chrome.exe",
		// Linux
		"google-chrome",
		"chrome",
	}

	for _, path := range locations {
		found, err := exec.LookPath(path)
		if err == nil {
			execPath = found

			break
		}
	}

	// Skip test if chrome is not available
	if execPath == "" {
		t.Skip("Chrome not found. Skipping test")
	}

	Convey("When fetching a Dashboard", t, func() {
		chromeInstance, err := chrome.NewLocalBrowserInstance(t.Context(), log.NewNullLogger(), true)
		defer chromeInstance.Close(log.NewNullLogger()) //nolint:staticcheck

		Convey("setup a chrome browser should not error", func() {
			So(err, ShouldBeNil)
		})

		var requestURI []string

		requestCookie := ""

		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Get CWD
			cwd, err := os.Getwd()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)

				return
			}

			// Read sample HTML file
			data, err := os.ReadFile(filepath.Join(cwd, "testdata/dashboard.html"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)

				return
			}

			muLock.Lock()

			requestURI = append(requestURI, r.RequestURI)
			if requestCookie == "" {
				requestCookie = r.Header.Get(backend.CookiesHeaderName)
			}

			muLock.Unlock()

			_, err = w.Write(data)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)

				return
			}
		}))
		defer ts.Close()

		Convey("When using the panels fetcher", func() {
			conf := config.Config{
				Layout:            "simple",
				DashboardMode:     "default",
				HTTPClientOptions: httpclient.Options{Timeouts: &httpclient.DefaultTimeoutOptions},
			}

			dash, err := New(
				log.NewNullLogger(),
				&conf,
				http.DefaultClient,
				chromeInstance,
				ts.URL,
				"v11.4.0",
				testSnapshot("randomUID"),
				http.Header{
					backend.CookiesHeaderName: []string{"cookie"},
				},
			)

			Convey("New dashboard should receive no errors", func() {
				So(err, ShouldBeNil)
			})

			d, err := dash.panelMetaData(t.Context())

			Convey("It should receive no errors", func() {
				So(err, ShouldBeNil)
			})
			Convey("It should use dashboards endpoint with the pinned absolute time window", func() {
				So(strings.Join(requestURI, ","), ShouldContainSubstring, "/d/randomUID/_?from=1734194465000&to=1734198065000")
			})
			Convey("It should use cookie", func() {
				So(requestCookie, ShouldEqual, "cookie")
			})
			Convey("It should return dashboard data", func() {
				So(d, ShouldHaveLength, 4)
			})
		})
	})
}

func TestDashboardFetchWithRemoteChrome(t *testing.T) {
	// Skip test if chrome is not available
	chromeRemoteAddr, ok := os.LookupEnv("CHROME_REMOTE_URL")
	if !ok {
		t.Skip("CHROME_REMOTE_URL unset. Skipping test")
	}

	Convey("When fetching a Dashboard", t, func() {
		chromeInstance, err := chrome.NewRemoteBrowserInstance(
			t.Context(),
			log.NewNullLogger(),
			chromeRemoteAddr,
		)

		Convey("setup a chrome browser should not error", func() {
			So(err, ShouldBeNil)
		})

		var requestURI []string

		requestCookie := ""

		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Get CWD
			cwd, err := os.Getwd()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)

				return
			}

			// Read sample HTML file
			data, err := os.ReadFile(filepath.Join(cwd, "testdata/dashboard.html"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)

				return
			}

			muLock.Lock()

			requestURI = append(requestURI, r.RequestURI)
			requestCookie = r.Header.Get(backend.CookiesHeaderName)

			muLock.Unlock()

			_, err = w.Write(data)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)

				return
			}
		}))
		defer ts.Close()

		Convey("When using the Grafana httpClient", func() {
			conf := config.Config{
				Layout:            "simple",
				DashboardMode:     "default",
				HTTPClientOptions: httpclient.Options{Timeouts: &httpclient.DefaultTimeoutOptions},
			}

			dash, err := New(
				log.NewNullLogger(),
				&conf,
				http.DefaultClient,
				chromeInstance,
				ts.URL,
				"v11.4.0",
				testSnapshot("randomUID"),
				http.Header{
					backend.CookiesHeaderName: []string{"cookie"},
				},
			)

			Convey("New dashboard should receive no errors", func() {
				So(err, ShouldBeNil)
			})

			d, err := dash.panelMetaData(t.Context())

			Convey("It should receive no errors", func() {
				So(err, ShouldBeNil)
			})
			Convey("It should use the v5 dashboards endpoint with the pinned absolute time window", func() {
				So(strings.Join(requestURI, ","), ShouldContainSubstring, "/d/randomUID/_?from=1734194465000&to=1734198065000")
			})
			Convey("It should use cookie", func() {
				So(requestCookie, ShouldEqual, "cookie")
			})
			Convey("It should return dashboard data", func() {
				So(d, ShouldHaveLength, 4)
			})
		})
	})
}

func TestDashboardCreatePanels(t *testing.T) {
	Convey("When creating panels for Dashboard", t, func() {
		dash, err := New(
			log.NewNullLogger(),
			nil,
			nil,
			nil,
			"http://localhost:3000",
			"v11.4.0",
			testSnapshot("randomUID"),
			nil,
		)

		Convey("New dashboard should receive no errors", func() {
			So(err, ShouldBeNil)
		})

		dashDataString := `[{"width":940,"height":258,"x":0,"y":0,"id":"12"},{"width":940,"height":258,"x":940,"y":0,"id":"26"},{"width":940,"height":258,"x":0,"y":0,"id":"27"}]`

		var dashData []any

		err = json.Unmarshal([]byte(dashDataString), &dashData)

		Convey("setup dashboard data unmarshal", func() {
			So(err, ShouldBeNil)
		})

		panels, err := dash.createPanels(dashData)

		Convey("It should receive no errors", func() {
			So(err, ShouldBeNil)
		})
		Convey("It should all panels from dashboard browser data", func() {
			So(panels, ShouldHaveLength, 3)
			So(panels[0].ID, ShouldEqual, "12")
			So(panels[1].ID, ShouldEqual, "26")
			So(panels[2].ID, ShouldEqual, "27")
		})
	})
}
