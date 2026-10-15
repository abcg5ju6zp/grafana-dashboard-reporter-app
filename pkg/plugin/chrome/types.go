package chrome

import (
	"context"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/config"
)

// PDFOptions contains the templated HTML Body, Header and Footer strings.
type PDFOptions struct {
	Header string
	Body   string
	Footer string

	Orientation string
}

// Instance is the interface remote and local chrome must implement.
type Instance interface {
	NewTab(logger log.Logger, conf *config.Config) *Tab
	// Ready verifies that the browser instance is usable. For local browsers
	// the browser process is already started at construction, while for remote
	// browsers this forces the actual connection to the remote Chromium.
	// A configuration generation is promoted only after Ready succeeds.
	Ready(ctx context.Context, logger log.Logger) error
	Name() string
	Close(logger log.Logger)
}
