// Command airspace shows live aircraft on a shared 3D globe.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/goodtune/airspace/internal/app"
	"github.com/goodtune/airspace/internal/config"
	"github.com/goodtune/airspace/internal/credentials"
	"github.com/goodtune/airspace/internal/telemetry"
	"github.com/goodtune/airspace/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "anomalies" {
		os.Exit(runAnomalies(os.Args[2:], os.Stdout, os.Stderr))
	}
	cfg, err := config.Load(os.Args[1:], os.Getenv)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "airspace: "+err.Error())
		os.Exit(1)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	for _, n := range cfg.Notes {
		log.Warn(n)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cfg.Dotvault != "" {
		// This talks to Vault, not to OpenSky.
		loadCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		cfg.ClientID, cfg.ClientSecret, err = credentials.FromDotvault(loadCtx, cfg.Dotvault, cfg.DotvaultConfig)
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "airspace: "+err.Error())
			os.Exit(1)
		}
		log.Info("OpenSky credentials loaded from dotvault", "secret", cfg.Dotvault)
	}
	build := telemetry.ReadBuild(version)
	log.Info("starting", "version", build.Version, "revision", build.Revision, "go", build.GoVersion)
	// Metrics are exported only when OTEL_EXPORTER_OTLP_ENDPOINT is set.
	metrics, shutdownMetrics, err := telemetry.Setup(ctx, build, log)
	if err != nil {
		log.Error("metrics are off: the exporter could not be set up", "error", err)
	}
	a := app.New(cfg, web.Static(), log)
	if err := a.Instrument(metrics); err != nil {
		log.Error("some metrics could not be registered", "error", err)
	}
	runErr := a.Run(ctx)
	// Send the last batch before exit.
	flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	shutdownMetrics(flushCtx)
	cancel()
	if runErr != nil {
		log.Error("stopped", "error", runErr)
		os.Exit(1)
	}
}
