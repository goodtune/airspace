// Package app wires the parts together. Both the binary and the tests use it.
package app

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/goodtune/airspace/internal/config"
	"github.com/goodtune/airspace/internal/hub"
	"github.com/goodtune/airspace/internal/opensky"
	"github.com/goodtune/airspace/internal/server"
	"github.com/goodtune/airspace/internal/store"
)

// App is a running instance.
type App struct {
	Hub     *hub.Hub
	Store   *store.Store // nil when storage could not be opened
	Handler http.Handler
	cfg     *config.Config
	log     *slog.Logger
}

// New builds the app. It makes no request to OpenSky.
func New(cfg *config.Config, static fs.FS, log *slog.Logger) *App {
	a := &App{cfg: cfg, log: log}
	var rec hub.Recorder
	var sessions server.Sessions
	st, err := store.Open(cfg.DBPath, store.Options{MaxSize: cfg.MaxDBSize, RetentionDays: cfg.RetentionDays}, log)
	if err != nil {
		// Storage is optional: degrade to live-only with a visible warning.
		log.Error("storage unavailable; running live-only", "path", cfg.DBPath, "error", err)
		rec = hub.NopRecorder{Msg: "storage unavailable: " + err.Error()}
	} else {
		a.Store, rec, sessions = st, st, st
	}
	up := &opensky.Client{
		HTTP:     &http.Client{Timeout: 45 * time.Second},
		TokenURL: cfg.TokenURL, APIURL: cfg.APIURL, ID: cfg.ClientID, Secret: cfg.ClientSecret,
	}
	a.Hub = hub.New(cfg, up, rec, log)
	if a.Store != nil {
		var saved hub.SavedCamera
		if ok, err := a.Store.LoadMeta("camera", &saved); ok && err == nil {
			a.Hub.Restore(saved)
		}
	}
	a.Handler = (&server.Server{Hub: a.Hub, Sessions: sessions, Static: static, Log: log}).Handler()
	return a
}

// Run serves until ctx is done.
func (a *App) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", a.cfg.ListenAddr)
	if err != nil {
		return err
	}
	return a.Serve(ctx, ln)
}

// Serve serves on ln until ctx is done.
func (a *App) Serve(ctx context.Context, ln net.Listener) error {
	hubDone := make(chan struct{})
	go func() { a.Hub.Run(ctx); close(hubDone) }()

	srv := &http.Server{Handler: a.Handler, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	a.log.Info("listening", "addr", ln.Addr().String())

	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	srv.Close() // streams never finish on their own
	<-hubDone
	if a.Store != nil {
		a.Store.Close()
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}
