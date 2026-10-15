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

// NewTab starts and returns a new tab on current browser instance. The tab is
// bound to ctx: canceling ctx (request snapshot canceled or instance
// generation disposed) cancels every in-flight browser action of the tab.
func (i *RemoteInstance) NewTab(ctx context.Context, logger log.Logger, _ *config.Config) *Tab {
	chromeLogger := logger.With("subsystem", "chromium")
	browserTabCtx, _ := chromedp.NewContext(i.allocCtx,
		chromedp.WithErrorf(chromeLogger.Error),
		chromedp.WithLogf(chromeLogger.Debug),
	)

	tabCtx, cancelTab := context.WithCancel(browserTabCtx)

	// Propagate snapshot/request cancellation to the browser tab.
	stopParentWatch := context.AfterFunc(ctx, cancelTab)

	return &Tab{
		ctx: tabCtx,
		cancel: func() {
			stopParentWatch()
			cancelTab()
		},
	}
}

// Close releases the resources of browser instance.
func (i *RemoteInstance) Close(_ log.Logger) {
	if i.allocCtxCancel != nil {
		i.allocCtxCancel()
	}
}
