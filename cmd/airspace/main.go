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

	"github.com/goodtune/airspace/internal/app"
	"github.com/goodtune/airspace/internal/config"
	"github.com/goodtune/airspace/web"
)

func main() {
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
	if err := app.New(cfg, web.Static(), log).Run(ctx); err != nil {
		log.Error("stopped", "error", err)
		os.Exit(1)
	}
}
