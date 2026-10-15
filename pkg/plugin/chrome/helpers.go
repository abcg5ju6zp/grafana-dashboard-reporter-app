package chrome

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
)

// browserReadyTimeout is the maximum time to wait for a browser instance to
// prove that it can open a tab and execute an action during generation build.
const browserReadyTimeout = 15 * time.Second

// verifyReady opens a new tab on the instance, loads about:blank and closes the
// tab. For a remote browser this forces the WebSocket connection to the remote
// Chromium and therefore fails fast when it is unreachable. A configuration
// generation is promoted only after this check succeeds.
func verifyReady(_ context.Context, logger log.Logger, instance Instance) error {
	tab := instance.NewTab(logger, nil)

	// RunWithTimeout derives a bounded context from the tab context, so an
	// unreachable remote Chromium cannot stall the generation build forever.
	err := tab.RunWithTimeout(browserReadyTimeout, chromedp.Navigate("about:blank"))

	// Tear the probe tab down directly. Tab.Close runs a browser-level
	// ClearBrowserCookies round trip, which can block indefinitely on a bare
	// tab that has not enabled the page session; the readiness probe neither
	// navigated to authenticated content nor needs cookie cleanup.
	cancelErr := chromedp.Cancel(tab.ctx)
	if cancelErr != nil {
		logger.Debug("error canceling readiness probe tab", "error", cancelErr)
	}

	if err != nil {
		return fmt.Errorf("browser is not ready: %w", err)
	}

	return nil
}

/*
	This file contains chromedp package related helper functions.
	Sources:
		- https://github.com/chromedp/chromedp/issues/1044
		- https://github.com/chromedp/chromedp/issues/431#issuecomment-592950397
		- https://github.com/chromedp/chromedp/issues/87
		- https://github.com/chromedp/examples/tree/master
*/

// enableLifeCycleEvents enables the chromedp life cycle events.
func enableLifeCycleEvents() chromedp.ActionFunc {
	return func(ctx context.Context) error {
		err := page.Enable().Do(ctx)
		if err != nil {
			return fmt.Errorf("failed to enable page: %w", err)
		}

		err = page.SetLifecycleEventsEnabled(true).Do(ctx)
		if err != nil {
			return fmt.Errorf("failed to enable lifecycle events: %w", err)
		}

		return nil
	}
}

// waitFor blocks until eventName is received.
// Examples of events you can wait for:
//
//	init, DOMContentLoaded, firstPaint,
//	firstContentfulPaint, firstImagePaint,
//	firstMeaningfulPaintCandidate,
//	load, networkAlmostIdle, firstMeaningfulPaint, networkIdle
//
// This is not super reliable, I've already found incidental cases where
// networkIdle was sent before load. It's probably smart to see how
// puppeteer implements this exactly.
func waitFor(eventName string) chromedp.ActionFunc {
	return func(ctx context.Context) error {
		ch := make(chan struct{})
		cctx, cancel := context.WithCancel(ctx)
		chromedp.ListenTarget(cctx, func(ev any) {
			switch e := ev.(type) {
			case *page.EventLifecycleEvent:
				if e.Name == eventName {
					cancel()
					close(ch)
				}
			}
		})

		select {
		case <-ch:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// setHeaders adds headers to requests made to the origin of addr only, so
// credentials are never sent to third-party origins a panel loads from.
func setHeaders(addr string, headers map[string]any) chromedp.ActionFunc {
	return func(ctx context.Context) error {
		pattern, err := originPattern(addr)
		if err != nil {
			return err
		}

		injected := make([]*fetch.HeaderEntry, 0, len(headers))
		replaced := make(map[string]bool, len(headers))

		for name, value := range headers {
			injected = append(injected, &fetch.HeaderEntry{Name: name, Value: fmt.Sprint(value)})
			replaced[strings.ToLower(name)] = true
		}

		executor := cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Target)

		chromedp.ListenTarget(ctx, func(ev any) {
			e, ok := ev.(*fetch.EventRequestPaused)
			if !ok {
				return
			}

			// Listeners must not block, so the request is continued from a goroutine.
			go func() {
				entries := slices.Clone(injected)

				for name, value := range e.Request.Headers {
					if !replaced[strings.ToLower(name)] {
						entries = append(entries, &fetch.HeaderEntry{Name: name, Value: fmt.Sprint(value)})
					}
				}

				err := fetch.ContinueRequest(e.RequestID).WithHeaders(entries).Do(executor)
				if err != nil {
					// A paused request that is never resumed would hang the page load.
					_ = fetch.FailRequest(e.RequestID, network.ErrorReasonFailed).Do(executor)
				}
			}()
		})

		return fetch.Enable().
			WithPatterns([]*fetch.RequestPattern{{URLPattern: pattern, RequestStage: fetch.RequestStageRequest}}).
			Do(ctx)
	}
}

// originPattern returns a Fetch URL pattern matching every URL on the origin of
// addr, normalised the way Chrome normalises request URLs.
func originPattern(addr string) (string, error) {
	u, err := url.Parse(addr)
	if err != nil {
		return "", fmt.Errorf("failed to parse url %s: %w", addr, err)
	}

	host := strings.ToLower(u.Host)

	switch u.Scheme {
	case "http":
		host = strings.TrimSuffix(host, ":80")
	case "https":
		host = strings.TrimSuffix(host, ":443")
	}

	return u.Scheme + "://" + host + "/*", nil
}
