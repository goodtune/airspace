// Package app wires the parts together. Both the binary and the tests use it.
package app

import (
	"context"
	"errors"
	"fmt"
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
	if len(a.cfg.ACMEDomains) == 0 {
		ln, err := net.Listen("tcp", a.cfg.ListenAddr)
		if err != nil {
			return err
		}
		return a.Serve(ctx, ln)
	}
	m, err := newACMEManager(a.cfg)
	if err != nil {
		return err
	}
	tlsLn, err := net.Listen("tcp", a.cfg.TLSListenAddr)
	if err != nil {
		return fmt.Errorf("TLS_LISTEN_ADDR: %w", err)
	}
	// The plain listener is a convenience. Without it there is no redirect
	// and no HTTP challenge, but the TLS challenge on the HTTPS port works.
	plainLn, err := net.Listen("tcp", a.cfg.ListenAddr)
	if err != nil {
		a.log.Warn("plain HTTP listener unavailable; no redirect to HTTPS and no HTTP-01 challenge", "addr", a.cfg.ListenAddr, "error", err)
		plainLn = nil
	}
	return a.ServeTLS(ctx, tlsLn, plainLn, m)
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
