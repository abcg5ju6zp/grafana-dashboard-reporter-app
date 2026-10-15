package report

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/config"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/dashboard"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/worker"
	. "github.com/smartystreets/goconvey/convey"
)

// fakeDashboard is a browser-free dashboard.ReportDashboard implementation for
// testing the snapshot rebuild loop.
type fakeDashboard struct {
	mu sync.Mutex

	activeVersion  int
	currentVersion int
	panelsByVer    map[int][]dashboard.Panel

	getDataCalls int
	checkCalls   int
	replaced     []int
	pngTitles    []string

	// saveDuringFirstAttempt simulates a save landing between the first and
	// the second version check of the first attempt.
	saveDuringFirstAttempt bool
	// bumpVersionEveryCheck makes the stored version advance on every version
	// check, simulating a dashboard that is continuously saved.
	bumpVersionEveryCheck bool
}

func (f *fakeDashboard) GetData(_ context.Context) (*dashboard.Data, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.getDataCalls++

	return &dashboard.Data{
		Title:       fmt.Sprintf("dashboard-v%d", f.activeVersion),
		GeneratedAt: time.UnixMilli(1452098072000),
		TimeRange:   dashboard.NewTimeRange("1452094472000", "1452098072000"),
		Panels:      f.panelsByVer[f.activeVersion],
	}, nil
}

func (f *fakeDashboard) CheckVersion(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	call := f.checkCalls
	f.checkCalls++

	// Simulate a save landing between the first (post-metadata) and the second
	// (pre-publish) version checks of the first attempt.
	if (call == 1 && f.saveDuringFirstAttempt) || f.bumpVersionEveryCheck {
		f.currentVersion++
	}

	if f.currentVersion != f.activeVersion {
		return fmt.Errorf("active %d current %d: %w",
			f.activeVersion, f.currentVersion, dashboard.ErrDashboardVersionChanged)
	}

	return nil
}

func (f *fakeDashboard) ReplaceSnapshot(snapshot *dashboard.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.activeVersion = snapshot.Version()
	f.replaced = append(f.replaced, f.activeVersion)
}

func (f *fakeDashboard) PanelPNG(_ context.Context, panel dashboard.Panel) (dashboard.PanelImage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.pngTitles = append(f.pngTitles, panel.Title)

	return dashboard.PanelImage{Image: "iVBORw0KGgo" + panel.Title, MimeType: "image/png"}, nil
}

func (f *fakeDashboard) PanelCSV(_ context.Context, _ dashboard.Panel) (dashboard.CSVData, error) {
	return nil, nil
}

func newTestPools(t *testing.T) worker.Pools {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	return worker.Pools{
		worker.Browser:  worker.New(ctx, 2),
		worker.Renderer: worker.New(ctx, 2),
	}
}

func snapshotForVersion(version int) *dashboard.Snapshot {
	model := &dashboard.Model{}
	model.Dashboard.Version = version

	snap, err := dashboard.NewSnapshot(model, time.UnixMilli(1452098072000))
	if err != nil {
		panic(err)
	}

	return snap
}

func gridPanel(id, title string) dashboard.Panel {
	return dashboard.Panel{ID: id, Type: "graph", Title: title, GridPos: dashboard.GridPos{W: 24, H: 6}}
}

func testReportConfig() *config.Config {
	return &config.Config{
		ReportFormat: "html",
		Theme:        "light",
		Layout:       "simple",
		TimeFormat:   time.UnixDate,
		Location:     time.UTC,
	}
}

func TestReportSnapshotRebuild(t *testing.T) {
	Convey("When the dashboard version changes between stages", t, func() {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		fake := &fakeDashboard{
			activeVersion:          1,
			currentVersion:         1,
			saveDuringFirstAttempt: true,
			panelsByVer: map[int][]dashboard.Panel{
				1: {gridPanel("1", "OLD PANEL")},
				2: {gridPanel("1", "NEW PANEL")},
			},
		}

		buildCalls := 0

		rep := New(logger, testReportConfig(), nil, nil, newTestPools(t), fake)

		rep.SetSnapshotBuilder(func(_ context.Context) (*dashboard.Snapshot, error) {
			buildCalls++

			fake.mu.Lock()
			v := fake.currentVersion
			fake.mu.Unlock()

			return snapshotForVersion(v), nil
		})

		rec := httptest.NewRecorder()
		err := rep.Generate(ctx, rec)

		Convey("The report is rebuilt from the new version without error", func() {
			So(err, ShouldBeNil)
			So(buildCalls, ShouldEqual, 1)
			So(fake.replaced, ShouldResemble, []int{2})
		})

		Convey("The old candidate was discarded and GetData ran for both versions", func() {
			So(fake.getDataCalls, ShouldEqual, 2)
			So(fake.pngTitles, ShouldContain, "NEW PANEL")
		})

		Convey("The published HTML contains only the new version, never mixed panels", func() {
			body := rec.Body.String()
			So(body, ShouldContainSubstring, "NEW PANEL")
			So(body, ShouldContainSubstring, "dashboard-v2")
			So(body, ShouldNotContainSubstring, "OLD PANEL")
			So(body, ShouldNotContainSubstring, "dashboard-v1")
		})
	})

	Convey("When rebuilding is denied by permissions", t, func() {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		fake := &fakeDashboard{activeVersion: 1, currentVersion: 1, saveDuringFirstAttempt: true}

		rep := New(logger, testReportConfig(), nil, nil, newTestPools(t), fake)
		deniedErr := errors.New("permission denied")

		rep.SetSnapshotBuilder(func(_ context.Context) (*dashboard.Snapshot, error) {
			return nil, deniedErr
		})

		rec := httptest.NewRecorder()
		err := rep.Generate(ctx, rec)

		Convey("The report fails directly without publishing any candidate", func() {
			So(errors.Is(err, deniedErr), ShouldBeTrue)
			So(rec.Body.String(), ShouldBeBlank)
		})
	})

	Convey("When the dashboard keeps changing until the request deadline", t, func() {
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()

		fake := &fakeDashboard{
			activeVersion:         1,
			currentVersion:        1,
			bumpVersionEveryCheck: true,
		}

		rep := New(logger, testReportConfig(), nil, nil, newTestPools(t), fake)
		rep.SetSnapshotBuilder(func(_ context.Context) (*dashboard.Snapshot, error) {
			fake.mu.Lock()
			v := fake.currentVersion
			fake.mu.Unlock()

			return snapshotForVersion(v), nil
		})

		rec := httptest.NewRecorder()
		err := rep.Generate(ctx, rec)

		Convey("A conflict is returned and no stale report is published", func() {
			So(errors.Is(err, ErrReportConflict), ShouldBeTrue)
			So(rec.Body.String(), ShouldBeBlank)
		})
	})

	Convey("When a change is detected but no snapshot builder is configured", t, func() {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		// The stored dashboard already differs from the report snapshot.
		fake := &fakeDashboard{activeVersion: 1, currentVersion: 2}

		rep := New(logger, testReportConfig(), nil, nil, newTestPools(t), fake)

		rec := httptest.NewRecorder()
		err := rep.Generate(ctx, rec)

		Convey("A conflict is returned rather than reusing stale data", func() {
			So(errors.Is(err, ErrReportConflict), ShouldBeTrue)
			So(rec.Body.String(), ShouldBeBlank)
		})
	})

	Convey("When the dashboard does not change", t, func() {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		fake := &fakeDashboard{
			activeVersion:  7,
			currentVersion: 7,
			panelsByVer: map[int][]dashboard.Panel{
				7: {gridPanel("9", "STABLE PANEL")},
			},
		}

		rep := New(logger, testReportConfig(), nil, nil, newTestPools(t), fake)
		rep.SetSnapshotBuilder(func(_ context.Context) (*dashboard.Snapshot, error) {
			return snapshotForVersion(fake.currentVersion), nil
		})

		rec := httptest.NewRecorder()
		err := rep.Generate(ctx, rec)

		Convey("The single candidate is published without any rebuild", func() {
			So(err, ShouldBeNil)
			So(fake.getDataCalls, ShouldEqual, 1)
			So(fake.replaced, ShouldBeEmpty)
			So(rec.Body.String(), ShouldContainSubstring, "STABLE PANEL")
		})
	})
}
