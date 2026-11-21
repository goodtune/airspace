package app

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/goodtune/airspace/internal/config"
	"github.com/goodtune/airspace/internal/fakeopensky"
)

// seedCert writes a certificate for name into an autocert cache directory,
// as if an ACME server had issued it. It returns the certificate.
func seedCert(t *testing.T, dir, name string, life time.Duration) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(life),
		DNSNames: []string{name}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	var buf bytes.Buffer
	pem.Encode(&buf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return cert
}

func TestServesTLSFromTheCertificateCache(t *testing.T) {
	const name = "airspace.test"
	dir := t.TempDir()
	cert := seedCert(t, dir, name, 90*24*time.Hour)

	// An ACME server that must never be called: the cached certificate is good.
	var acmeCalls atomic.Int32
	acmeSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acmeCalls.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer acmeSrv.Close()
	fake := fakeopensky.New()
	up := httptest.NewServer(fake.Handler())
	defer up.Close()

	tlsLn, _ := net.Listen("tcp", "127.0.0.1:0")
	plainLn, _ := net.Listen("tcp", "127.0.0.1:0")
	_, tlsPort, _ := net.SplitHostPort(tlsLn.Addr().String())
	cfg := &config.Config{
		ClientID: "id", ClientSecret: secret, TokenURL: up.URL + "/token", APIURL: up.URL + "/api",
		PollInterval: time.Second, MinInterval: time.Second, Resolution: time.Second,
		DailyCredits: 4000, DailyUseHours: 2, CreditReserve: 200, HiddenTimeout: time.Second, SessionGrace: time.Second,
		DBPath: filepath.Join(t.TempDir(), "a.db"), StartView: [3]float64{151, -33, 4e5},
		ACMEDomains: []string{name}, ACMEDirectoryURL: acmeSrv.URL + "/directory", ACMECacheDir: dir,
		TLSListenAddr: tlsLn.Addr().String(),
	}
	logs := &syncBuf{}
	a := New(cfg, fstest.MapFS{"index.html": {Data: []byte("<title>Airspace</title>")}}, slog.New(slog.NewTextHandler(logs, nil)))
	m, err := newACMEManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.ServeTLS(ctx, tlsLn, plainLn, m); close(done) }()
	defer func() { cancel(); <-done }()

	pool := x509.NewCertPool()
	pool.AddCert(cert)
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, tlsLn.Addr().String())
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: dial, TLSClientConfig: &tls.Config{RootCAs: pool}, ForceAttemptHTTP2: true,
	}}

	// The app answers over HTTPS with the certificate for its name.
	resp, err := client.Get("https://" + name + "/api/status")
	if err != nil {
		t.Fatalf("HTTPS request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"state":"stopped"`) {
		t.Fatalf("HTTPS status %d: %s", resp.StatusCode, body)
	}
	if resp.TLS == nil || resp.TLS.PeerCertificates[0].DNSNames[0] != name || resp.ProtoMajor != 2 {
		t.Errorf("want HTTP/2 with the %s certificate, got proto %s", name, resp.Proto)
	}

	// A name that is not configured gets no certificate, and no ACME order.
	bad := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: dial, TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "other.test"},
	}}
	if _, err := bad.Get("https://other.test/"); err == nil {
		t.Error("handshake for an unconfigured name should fail")
	}

	// Plain HTTP redirects to HTTPS, keeping the path and query.
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest(http.MethodGet, "http://"+plainLn.Addr().String()+"/some/path?replay=latest", nil)
	req.Host = name
	resp, err = noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if want := "https://" + name + ":" + tlsPort + "/some/path?replay=latest"; resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != want {
		t.Errorf("redirect: %d %q, want 308 %q", resp.StatusCode, resp.Header.Get("Location"), want)
	}

	// The certificate is announced, with a renewal margin taken from its life.
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(logs.String(), "certificate ready") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), "certificate ready") || !strings.Contains(logs.String(), "renew_before=7") {
		t.Errorf("expected a certificate log line with a renewal margin near 30 days:\n%s", logs.String())
	}
	if n := acmeCalls.Load(); n != 0 {
		t.Errorf("%d requests to the ACME server with a good cached certificate", n)
	}
	if n := fake.Requests(); n != 0 {
		t.Errorf("%d OpenSky requests", n)
	}
}

func TestRedirectToStandardPort(t *testing.T) {
	h := redirectToHTTPS(":443")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://air.example.org:80/x", nil)
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Location"); got != "https://air.example.org/x" {
		t.Errorf("Location %q", got)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://air.example.org/api/action/input", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST over plain HTTP: %d, want 400", rec.Code)
	}
}

func TestAutoRenewBefore(t *testing.T) {
	now := time.Now()
	for life, want := range map[time.Duration]time.Duration{90 * 24 * time.Hour: 30 * 24 * time.Hour, 7 * 24 * time.Hour: 56 * time.Hour} {
		if got := autoRenewBefore(&x509.Certificate{NotBefore: now, NotAfter: now.Add(life)}); got != want {
			t.Errorf("life %s: renew %s before expiry, want %s", life, got, want)
		}
	}
}

func TestACMEManagerRejectsBadCAFile(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(bad, []byte("not a certificate"), 0o600)
	cfg := &config.Config{ACMEDomains: []string{"a.test"}, ACMECacheDir: t.TempDir(), ACMECAFile: bad, ACMEDirectoryURL: "https://ca/dir"}
	if _, err := newACMEManager(cfg); err == nil || !strings.Contains(err.Error(), "no PEM certificate") {
		t.Errorf("got %v", err)
	}
}
