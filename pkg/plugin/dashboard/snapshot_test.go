package dashboard

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	. "github.com/smartystreets/goconvey/convey"
)

func snapshotModel(version int, from, to string) *Model {
	model := &Model{}
	model.Dashboard.Version = version
	model.Dashboard.UID = "uid"
	model.Dashboard.Title = "title"
	model.Dashboard.Variables = url.Values{}

	if from != "" {
		model.Dashboard.Variables.Set("from", from)
	}

	if to != "" {
		model.Dashboard.Variables.Set("to", to)
	}

	model.Dashboard.Variables.Add("var-host", "server")

	return model
}

func TestSnapshot(t *testing.T) {
	// Fixed reference instant: Wed, 06 Jan 2016 16:34:32 UTC
	ref, _ := time.Parse(time.RFC1123, "Wed, 06 Jan 2016 16:34:32 UTC")

	Convey("When creating a snapshot", t, func() {
		Convey("Relative from/to are resolved once into one absolute window", func() {
			snap, err := NewSnapshot(snapshotModel(3, "now-1h", "now"), ref)
			So(err, ShouldBeNil)
			So(snap.Version(), ShouldEqual, 3)
			So(snap.To.UnixMilli(), ShouldEqual, ref.UnixMilli())
			So(snap.From.UnixMilli(), ShouldEqual, ref.Add(-time.Hour).UnixMilli())
			So(snap.FetchedAt.UnixMilli(), ShouldEqual, ref.UnixMilli())
		})

		Convey("Snapshot variables carry the pinned absolute window in epoch milliseconds", func() {
			snap, err := NewSnapshot(snapshotModel(3, "now-1h", "now"), ref)
			So(err, ShouldBeNil)
			So(snap.Variables().Get("from"), ShouldEqual, "1452094472000")
			So(snap.Variables().Get("to"), ShouldEqual, "1452098072000")
			So(snap.TimeRange().From, ShouldEqual, "1452094472000")
			So(snap.TimeRange().To, ShouldEqual, "1452098072000")
			// Non-time variables are preserved.
			So(snap.Variables().Get("var-host"), ShouldEqual, "server")
		})

		Convey("Default relative window is used when from/to are missing", func() {
			snap, err := NewSnapshot(snapshotModel(1, "", ""), ref)
			So(err, ShouldBeNil)
			So(snap.To.UnixMilli(), ShouldEqual, ref.UnixMilli())
			So(snap.From.UnixMilli(), ShouldEqual, ref.Add(-time.Hour).UnixMilli())
		})

		Convey("Explicit absolute epoch milliseconds are preserved unchanged", func() {
			snap, err := NewSnapshot(snapshotModel(1, "1452094472000", "1452098072000"), ref)
			So(err, ShouldBeNil)
			So(snap.Variables().Get("from"), ShouldEqual, "1452094472000")
			So(snap.Variables().Get("to"), ShouldEqual, "1452098072000")
			So(snap.From.Unix(), ShouldEqual, int64(1452094472))
		})

		Convey("Explicit RFC3339 absolute times are preserved", func() {
			snap, err := NewSnapshot(snapshotModel(1, "2016-01-06T15:34:32.000Z", "2016-01-06T16:34:32.000Z"), ref)
			So(err, ShouldBeNil)
			So(snap.From.UTC().Format(time.RFC3339), ShouldEqual, "2016-01-06T15:34:32Z")
			So(snap.To.UTC().Format(time.RFC3339), ShouldEqual, "2016-01-06T16:34:32Z")
		})

		Convey("Unrecognised time specs return an error instead of panicking", func() {
			_, err := NewSnapshot(snapshotModel(1, "not-a-time", "now"), ref)
			So(err, ShouldNotBeNil)
		})

		Convey("The model variables are not mutated by the snapshot", func() {
			model := snapshotModel(3, "now-1h", "now")
			_, err := NewSnapshot(model, ref)
			So(err, ShouldBeNil)
			So(model.Dashboard.Variables.Get("from"), ShouldEqual, "now-1h")
			So(model.Dashboard.Variables.Get("to"), ShouldEqual, "now")
		})
	})

	Convey("When checking the dashboard version", t, func() {
		snap, err := NewSnapshot(snapshotModel(3, "now-1h", "now"), ref)
		So(err, ShouldBeNil)

		dash, err := New(log.NewNullLogger(), nil, nil, nil, "http://localhost:3000", "v11.4.0", snap, nil)
		So(err, ShouldBeNil)

		Convey("Without a version fetcher the check is a no-op", func() {
			So(dash.CheckVersion(context.Background()), ShouldBeNil)
		})

		Convey("Matching versions pass the check", func() {
			dash.SetVersionFetcher(func(_ context.Context) (int, error) { return 3, nil })
			So(dash.CheckVersion(context.Background()), ShouldBeNil)
		})

		Convey("A newer stored version is reported as a change", func() {
			dash.SetVersionFetcher(func(_ context.Context) (int, error) { return 4, nil })
			err := dash.CheckVersion(context.Background())
			So(errors.Is(err, ErrDashboardVersionChanged), ShouldBeTrue)
		})

		Convey("Fetcher errors are propagated", func() {
			fetchErr := errors.New("boom")

			dash.SetVersionFetcher(func(_ context.Context) (int, error) { return 0, fetchErr })
			err := dash.CheckVersion(context.Background())
			So(errors.Is(err, fetchErr), ShouldBeTrue)
		})

		Convey("Replacing the snapshot rebinds model and window", func() {
			newSnap, err := NewSnapshot(snapshotModel(4, "now-2h", "now"), ref)
			So(err, ShouldBeNil)
			dash.ReplaceSnapshot(newSnap)
			So(dash.snapshot.Version(), ShouldEqual, 4)
			So(dash.snapshot.From.UnixMilli(), ShouldEqual, ref.Add(-2*time.Hour).UnixMilli())
		})
	})
}
