package chrome

import (
	"context"

	"github.com/chromedp/chromedp"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/config"
)

// RemoteInstance is a remotely running browser instance.
type RemoteInstance struct {
	allocCtx       context.Context
	allocCtxCancel context.CancelFunc
}

// NewRemoteBrowserInstance creates a new remote browser instance.
func NewRemoteBrowserInstance(ctx context.Context, _ log.Logger, remoteChromeURL string) (*RemoteInstance, error) {
	allocCtx, allocCtxCancel := chromedp.NewRemoteAllocator(ctx, remoteChromeURL)

	return &RemoteInstance{allocCtx, allocCtxCancel}, nil
}

// Name returns the kind of browser instance.
func (i *RemoteInstance) Name() string {
	return "remote"
}

// Ready forces the connection to the remote Chromium by opening a tab and
// loading about:blank. The remote allocator connects lazily, so without this
// check an unreachable Chromium would only fail when the first report runs.
func (i *RemoteInstance) Ready(ctx context.Context, logger log.Logger) error {
	return verifyReady(ctx, logger, i)
}

// NewTab starts and returns a new tab on current browser instance.
func (i *RemoteInstance) NewTab(logger log.Logger, _ *config.Config) *Tab {
	chromeLogger := logger.With("subsystem", "chromium")
	browserCtx, _ := chromedp.NewContext(i.allocCtx,
		chromedp.WithErrorf(chromeLogger.Error),
		chromedp.WithLogf(chromeLogger.Debug),
	)

	return &Tab{
		ctx: browserCtx,
	}
}

// Close releases the resources of browser instance.
func (i *RemoteInstance) Close(_ log.Logger) {
	if i.allocCtxCancel != nil {
		i.allocCtxCancel()
	}
}
