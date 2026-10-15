package dashboard

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// SnapshotFunc builds a fresh, immutable report snapshot: it reads the current
// dashboard model and folder permission resources, enforces access and pins
// the report's time window. It is used both for the initial snapshot and for
// rebuilding a complete candidate after the dashboard has been saved.
type SnapshotFunc func(ctx context.Context) (*Snapshot, error)

// Snapshot is the immutable point-in-time state a single report candidate is
// built from. It binds together the exact dashboard model version that has
// been read from the API and the single absolute time window the request
// started with. Every downstream consumer (panel metadata, native or remote
// PNG rendering, CSV download and the final template) must use this snapshot.
type Snapshot struct {
	// model is the dashboard API model read at FetchedAt, including its version.
	model *Model
	// variables is a copy of the request variables with from/to pinned to the
	// resolved absolute window (epoch milliseconds). It must be used for every
	// dashboard URL opened during the report, so relative "now" cannot drift
	// between the model read, browser, image renderer and CSV download.
	variables url.Values
	// From and To are the absolute bounds of the report window resolved once.
	From time.Time
	To   time.Time
	// FetchedAt is the single reference instant ("now") used to resolve any
	// relative from/to specifications.
	FetchedAt time.Time
}

// NewSnapshot creates an immutable snapshot from a dashboard model. The
// possibly relative from/to specifications are resolved exactly once against
// fetchedAt and copied into the snapshot variables as epoch milliseconds, so
// the whole report chain observes the very same absolute window.
func NewSnapshot(model *Model, fetchedAt time.Time) (*Snapshot, error) {
	spec := NewTimeRange(model.Dashboard.Variables.Get("from"), model.Dashboard.Variables.Get("to"))

	ref := now(fetchedAt)

	from, err := safeParseTime(ref.parseFrom, spec.From)
	if err != nil {
		return nil, err
	}

	to, err := safeParseTime(ref.parseTo, spec.To)
	if err != nil {
		return nil, err
	}

	// Clone the request variables and pin from/to to the resolved absolute
	// window. Never mutate the model's own values.
	variables := make(url.Values, len(model.Dashboard.Variables)+2)

	for key, values := range model.Dashboard.Variables {
		cloned := append([]string(nil), values...)
		variables[key] = cloned
	}

	variables.Set("from", strconv.FormatInt(from.UnixMilli(), 10))
	variables.Set("to", strconv.FormatInt(to.UnixMilli(), 10))

	return &Snapshot{
		model:     model,
		variables: variables,
		From:      from,
		To:        to,
		FetchedAt: fetchedAt,
	}, nil
}

// safeParseTime resolves a Grafana time spec, converting the parser's panic
// on unrecognised formats into a regular error.
func safeParseTime(parse func(string) time.Time, spec string) (time.Time, error) {
	var (
		resolved time.Time
		parseErr error
	)

	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				parseErr = fmt.Errorf("invalid report time range: %v", recovered)
			}
		}()

		resolved = parse(spec)
	}()

	return resolved, parseErr
}

// Model returns the dashboard API model bound to the snapshot.
func (s *Snapshot) Model() *Model {
	return s.model
}

// Version returns the dashboard version recorded in the snapshot model.
func (s *Snapshot) Version() int {
	return s.model.Dashboard.Version
}

// Variables returns the effective request variables whose from/to are always
// the snapshot's absolute window in epoch milliseconds.
func (s *Snapshot) Variables() url.Values {
	return s.variables
}

// TimeRange returns the absolute time range carried by the snapshot.
func (s *Snapshot) TimeRange() TimeRange {
	return NewTimeRange(s.variables.Get("from"), s.variables.Get("to"))
}
