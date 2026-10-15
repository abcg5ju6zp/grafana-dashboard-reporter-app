package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend/app"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin"
)

// shutdownTimeout bounds how long in-flight reports may continue after the
// plugin process has been asked to stop before their leases are terminated and
// the local/remote Chromium instances are cleaned up.
const shutdownTimeout = 30 * time.Second

func main() {
	// Bounded graceful shutdown on SIGTERM/SIGINT: stop accepting new reports,
	// wait for in-flight reports to return their generation leases, then close
	// worker pools, HTTP clients and local/remote Chromium.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		sig, ok := <-sigCh
		if !ok {
			return
		}

		log.DefaultLogger.Info("received signal, shutting down plugin generations", "signal", sig.String())

		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		plugin.Shutdown(ctx)
		cancel()

		// Signal default disposition: exit once cleanup is done (or the bounded
		// wait has elapsed).
		os.Exit(0)
	}()

	// Start listening to requests sent from Grafana. This call is blocking so
	// it won't finish until Grafana shuts down the process or the plugin choose
	// to exit by itself using os.Exit. Manage automatically manages life cycle
	// of app instances. It accepts app instance factory as first
	// argument. This factory will be automatically called on incoming request
	// from Grafana to create different instances of `App` (per plugin
	// ID).
	err := app.Manage(plugin.Name, plugin.NewDashboardReporterApp, app.ManageOpts{})
	if err != nil {
		log.DefaultLogger.Error("failed to start "+plugin.Name, "err", err.Error())

		// Make sure generations (including Chromium) are cleaned up if Manage
		// returns on its own.
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		plugin.Shutdown(ctx)
		cancel()

		os.Exit(1)
	}
}
