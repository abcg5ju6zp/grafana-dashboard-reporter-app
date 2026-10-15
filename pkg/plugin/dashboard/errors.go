package dashboard

import "errors"

var (
	ErrNoPanels                 = errors.New("no panels found in browser data")
	ErrNoDashboardData          = errors.New("no dashboard data found")
	ErrJavaScriptReturnedNoData = errors.New("javascript did not return any dashboard data")
	ErrDashboardHTTPError       = errors.New("dashboard request does not return 200 OK")
	ErrEmptyBlobURL             = errors.New("empty blob URL")
	ErrEmptyPanelElement        = errors.New("no data element found in the panel")
	ErrEmptyCSVData             = errors.New("empty csv data")
	// ErrDashboardVersionChanged is returned when the dashboard stored in
	// Grafana no longer has the version recorded by the report snapshot. The
	// whole in-flight candidate must be discarded and rebuilt from the latest
	// version; panels from different versions must never be mixed.
	ErrDashboardVersionChanged = errors.New("dashboard version changed since report snapshot")
)
