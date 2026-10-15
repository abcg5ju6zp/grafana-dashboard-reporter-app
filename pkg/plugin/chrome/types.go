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
	// NewTab opens a tab bound to ctx. When ctx is canceled (request done or
	// plugin instance generation disposed) the tab and its in-flight browser
	// actions are canceled, so no work can outlive its request snapshot.
	NewTab(ctx context.Context, logger log.Logger, conf *config.Config) *Tab
	Name() string
	Close(logger log.Logger)
}
