package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/goodtune/airspace/internal/config"
)

// newACMEManager builds the certificate manager. It contacts nothing: the
// first certificate is requested when the server starts serving.
func newACMEManager(cfg *config.Config) (*autocert.Manager, error) {
	if err := os.MkdirAll(cfg.ACMECacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("ACME_CACHE_DIR: %w", err)
	}
	client := &acme.Client{DirectoryURL: cfg.ACMEDirectoryURL, UserAgent: "airspace"}
	if cfg.ACMECAFile != "" {
		pem, err := os.ReadFile(cfg.ACMECAFile)
		if err != nil {
			return nil, fmt.Errorf("ACME_CA_FILE: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ACME_CA_FILE: %s holds no PEM certificate", cfg.ACMECAFile)
		}
		client.HTTPClient = &http.Client{
			Timeout:   60 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}, Proxy: http.ProxyFromEnvironment},
		}
	}
	// The HTTP challenge is checked against the request's Host header, which
	// carries a port when the listener is not on port 80.
	allowed := autocert.HostWhitelist(cfg.ACMEDomains...)
	policy := func(ctx context.Context, host string) error {
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		return allowed(ctx, host)
	}
	m := &autocert.Manager{
		Prompt:      autocert.AcceptTOS,
		HostPolicy:  policy,
		Cache:       autocert.DirCache(cfg.ACMECacheDir),
		Email:       cfg.ACMEEmail,
		Client:      client,
		RenewBefore: cfg.ACMERenewBefore,
	}
	if cfg.ACMEEABKID != "" {
		m.ExternalAccountBinding = &acme.ExternalAccountBinding{KID: cfg.ACMEEABKID, Key: cfg.ACMEEABKey}
	}
	return m, nil
}

// helloFor is the handshake of a modern browser asking for name. It is used
// to fetch a certificate ahead of the first visitor, and to read it back.
func helloFor(name string) *tls.ClientHelloInfo {
	return &tls.ClientHelloInfo{
		ServerName:        name,
		CipherSuites:      []uint16{tls.TLS_AES_128_GCM_SHA256, tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		SignatureSchemes:  []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256},
		SupportedCurves:   []tls.CurveID{tls.X25519, tls.CurveP256},
		SupportedVersions: []uint16{tls.VersionTLS13, tls.VersionTLS12},
	}
}

// autoRenewBefore picks a renewal margin from the certificate's own life: a
// third of it. A 90 day certificate renews with 30 days left, a 7 day one
// with a little over 2. The library's fixed 30 days would make a short-lived
// certificate count as due for renewal the moment it is issued.
func autoRenewBefore(leaf *x509.Certificate) time.Duration {
	return leaf.NotAfter.Sub(leaf.NotBefore) / 3
}

// redirectToHTTPS sends plain HTTP requests to the HTTPS address.
func redirectToHTTPS(tlsAddr string) http.Handler {
	_, port, _ := net.SplitHostPort(tlsAddr)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "use HTTPS", http.StatusBadRequest)
			return
		}
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if port != "" && port != "443" {
			host = net.JoinHostPort(host, port)
		}
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusPermanentRedirect)
	})
}

// ServeTLS serves the app over HTTPS on tlsLn with certificates from m. The
// plain listener, when there is one, answers the ACME HTTP challenge and
// redirects everything else to HTTPS.
func (a *App) ServeTLS(ctx context.Context, tlsLn, plainLn net.Listener, m *autocert.Manager) error {
	hubDone := make(chan struct{})
	go func() { a.Hub.Run(ctx); close(hubDone) }()

	main := &http.Server{Handler: a.Handler, TLSConfig: m.TLSConfig(), ReadHeaderTimeout: 10 * time.Second}
	servers := []*http.Server{main}
	errc := make(chan error, 2)
	go func() { errc <- main.ServeTLS(tlsLn, "", "") }()
	a.log.Info("listening with TLS", "addr", tlsLn.Addr().String(), "domains", a.cfg.ACMEDomains, "acme", a.cfg.ACMEDirectoryURL)
	if plainLn != nil {
		plain := &http.Server{Handler: m.HTTPHandler(redirectToHTTPS(a.cfg.TLSListenAddr)), ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, plain)
		go func() { errc <- plain.Serve(plainLn) }()
		a.log.Info("redirecting plain HTTP", "addr", plainLn.Addr().String())
	}

	// Ask for the certificates now, so the first visitor does not wait and
	// a problem shows in the log at once.
	go func() {
		for _, name := range a.cfg.ACMEDomains {
			cert, err := m.GetCertificate(helloFor(name))
			if err != nil {
				a.log.Error("no certificate; will try again when a browser connects", "domain", name, "error", err)
				continue
			}
			if leaf := cert.Leaf; leaf != nil {
				if a.cfg.ACMERenewBefore == 0 {
					m.RenewBefore = autoRenewBefore(leaf)
				}
				a.log.Info("certificate ready", "domain", name, "issuer", leaf.Issuer.CommonName,
					"expires", leaf.NotAfter.Format(time.RFC3339), "renew_before", m.RenewBefore.String())
			}
		}
	}()

	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	for _, s := range servers {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		s.Shutdown(sctx)
		cancel()
		s.Close() // streams never finish on their own
	}
	<-hubDone
	if a.Store != nil {
		a.Store.Close()
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}
