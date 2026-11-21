//go:build acme

// A real ACME exchange against Pebble, Let's Encrypt's test server. Run with
//
//	make acme-test
//
// which installs Pebble and sets PEBBLE to its path.
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
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/goodtune/airspace/internal/config"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// privateCA makes a root and a server certificate for localhost, the way a
// private ACME server such as Vault might be set up. It returns the paths of
// the root, the server certificate and the server key.
func privateCA(t *testing.T, dir string) (root, cert, key string) {
	t.Helper()
	write := func(name, typ string, der []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Airspace test private root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	srvKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caTmpl, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(srvKey)
	return write("root.pem", "CERTIFICATE", caDER), write("cert.pem", "CERTIFICATE", srvDER), write("key.pem", "EC PRIVATE KEY", keyDER)
}

// dnsAnswer answers an A query with 127.0.0.1 and anything else with no
// records. It returns nil for a packet it cannot read.
func dnsAnswer(q []byte) []byte {
	end := 12 // walk the one question's name
	for end < len(q) && q[end] != 0 {
		end += int(q[end]) + 1
	}
	if len(q) < 12 || end+5 > len(q) {
		return nil
	}
	qtype := int(q[end+1])<<8 | int(q[end+2])
	reply := append([]byte{}, q[:end+5]...)
	reply[2], reply[3] = 0x85, 0x80 // response, authoritative, no error
	reply[4], reply[5] = 0, 1       // one question
	reply[6], reply[7], reply[8], reply[9], reply[10], reply[11] = 0, 0, 0, 0, 0, 0
	if qtype == 1 { // A
		reply[7] = 1
		reply = append(reply, 0xC0, 0x0C, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 127, 0, 0, 1)
	}
	return reply
}

// loopbackDNS is a DNS server that resolves every name to 127.0.0.1. Pebble
// resolves the name it is validating through it (over TCP), so the test can
// use a real-looking hostname without touching the machine's DNS.
func loopbackDNS(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				for {
					var size [2]byte // each message has a two-byte length in front
					if _, err := io.ReadFull(conn, size[:]); err != nil {
						return
					}
					q := make([]byte, int(size[0])<<8|int(size[1]))
					if _, err := io.ReadFull(conn, q); err != nil {
						return
					}
					reply := dnsAnswer(q)
					if reply == nil {
						return
					}
					conn.Write(append([]byte{byte(len(reply) >> 8), byte(len(reply))}, reply...))
				}
			}()
		}
	}()
	return ln.Addr().String()
}

const acmeName = "airspace.test"

func TestIssuesFromAnACMEServer(t *testing.T) {
	pebble := os.Getenv("PEBBLE")
	if pebble == "" {
		t.Skip("PEBBLE is not set; run make acme-test")
	}
	dir := t.TempDir()
	rootFile, certFile, keyFile := privateCA(t, dir)
	acmePort, mgmtPort, tlsPort, plainPort := freePort(t), freePort(t), freePort(t), freePort(t)

	// Pebble validates a name by connecting back on these two ports, where
	// Let's Encrypt and Vault use 443 and 80.
	pebbleCfg, _ := json.Marshal(map[string]any{"pebble": map[string]any{
		"listenAddress": fmt.Sprintf("127.0.0.1:%d", acmePort), "managementListenAddress": fmt.Sprintf("127.0.0.1:%d", mgmtPort),
		"certificate": certFile, "privateKey": keyFile, "httpPort": plainPort, "tlsPort": tlsPort,
		"ocspResponderURL": "", "externalAccountBindingRequired": false,
	}})
	cfgFile := filepath.Join(dir, "pebble.json")
	os.WriteFile(cfgFile, pebbleCfg, 0o600)
	var pebbleLog syncBuf
	cmd := exec.Command(pebble, "-config", cfgFile, "-dnsserver", loopbackDNS(t))
	cmd.Env = append(os.Environ(), "PEBBLE_VA_NOSLEEP=1", "PEBBLE_WFE_NONCEREJECT=20") // some bad nonces, as in real life
	cmd.Stdout, cmd.Stderr = &pebbleLog, &pebbleLog
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting pebble: %v", err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()

	rootPEM, _ := os.ReadFile(rootFile)
	private := x509.NewCertPool()
	private.AppendCertsFromPEM(rootPEM)
	admin := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: private}}}
	get := func(url string) []byte {
		t.Helper()
		var last error
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			resp, err := admin.Get(url)
			if err != nil {
				last = err
				continue
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return body
			}
			last = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		t.Fatalf("GET %s: %v\npebble said:\n%s", url, last, pebbleLog.String())
		return nil
	}
	directory := fmt.Sprintf("https://127.0.0.1:%d/dir", acmePort)
	get(directory)
	// The root that signs what Pebble issues. A browser would need to trust it.
	issuing := x509.NewCertPool()
	if !issuing.AppendCertsFromPEM(get(fmt.Sprintf("https://127.0.0.1:%d/roots/0", mgmtPort))) {
		t.Fatal("pebble returned no issuing root")
	}

	cfg := &config.Config{
		ClientID: "id", ClientSecret: secret, TokenURL: "http://127.0.0.1:9/token", APIURL: "http://127.0.0.1:9/api",
		PollInterval: time.Second, MinInterval: time.Second, Resolution: time.Second,
		DailyCredits: 4000, DailyUseHours: 2, CreditReserve: 200, HiddenTimeout: time.Second, SessionGrace: time.Second,
		DBPath: filepath.Join(dir, "a.db"), StartView: [3]float64{151, -33, 4e5},
		ACMEDomains: []string{acmeName}, ACMEDirectoryURL: directory, ACMECAFile: rootFile, ACMEEmail: "ops@example.org",
		ACMECacheDir: filepath.Join(dir, "acme"), TLSListenAddr: fmt.Sprintf(":%d", tlsPort), ListenAddr: fmt.Sprintf(":%d", plainPort),
	}
	issued := func() int { return strings.Count(pebbleLog.String(), "Issued certificate") }

	// run starts the app as the binary does, waits for its certificate, and
	// returns the certificate it serves.
	run := func() *x509.Certificate {
		t.Helper()
		logs := &syncBuf{}
		a := New(cfg, fstest.MapFS{"index.html": {Data: []byte("<title>Airspace</title>")}}, slog.New(slog.NewTextHandler(logs, nil)))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- a.Run(ctx) }()
		defer func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("app stopped with: %v", err)
			}
		}()
		for deadline := time.Now().Add(30 * time.Second); !strings.Contains(logs.String(), "certificate ready"); time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) || strings.Contains(logs.String(), "no certificate") {
				t.Fatalf("no certificate.\napp said:\n%s\npebble said:\n%s", logs.String(), pebbleLog.String())
			}
		}
		if strings.Contains(logs.String(), secret) {
			t.Error("logs contain the OpenSky secret")
		}
		client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: issuing},
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, fmt.Sprintf("127.0.0.1:%d", tlsPort))
			},
		}}
		resp, err := client.Get(fmt.Sprintf("https://%s:%d/healthz", acmeName, tlsPort))
		if err != nil {
			t.Fatalf("HTTPS with the issued certificate failed: %v", err)
		}
		defer resp.Body.Close()
		if body, _ := io.ReadAll(resp.Body); resp.StatusCode != 200 || !bytes.Contains(body, []byte("ok")) {
			t.Fatalf("healthz over HTTPS: %d %q", resp.StatusCode, body)
		}
		// Plain HTTP still redirects while the challenge handler is in front.
		noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", plainPort), nil)
		req.Host = acmeName
		r2, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r2.Body.Close()
		if want := fmt.Sprintf("https://%s:%d/", acmeName, tlsPort); r2.Header.Get("Location") != want {
			t.Errorf("redirect to %q, want %q", r2.Header.Get("Location"), want)
		}
		return resp.TLS.PeerCertificates[0]
	}

	first := run()
	if len(first.DNSNames) != 1 || first.DNSNames[0] != acmeName || !strings.Contains(first.Issuer.CommonName, "Pebble") {
		t.Errorf("issued certificate: names %v, issuer %q", first.DNSNames, first.Issuer.CommonName)
	}
	if n := issued(); n != 1 {
		t.Fatalf("pebble issued %d certificates, want 1\n%s", n, pebbleLog.String())
	}
	for _, f := range []string{acmeName, "acme_account+key"} {
		st, err := os.Stat(filepath.Join(cfg.ACMECacheDir, f))
		if err != nil {
			t.Errorf("cache file %s: %v", f, err)
		} else if st.Mode().Perm()&0o077 != 0 {
			t.Errorf("cache file %s is readable by others: %v", f, st.Mode())
		}
	}

	// A restart uses the cached certificate: no new order.
	second := run()
	if second.SerialNumber.Cmp(first.SerialNumber) != 0 || issued() != 1 {
		t.Errorf("restart asked for a new certificate (issued %d)", issued())
	}

	// If the cache is lost, the next start gets a fresh certificate.
	if err := os.Remove(filepath.Join(cfg.ACMECacheDir, acmeName)); err != nil {
		t.Fatal(err)
	}
	third := run()
	if third.SerialNumber.Cmp(first.SerialNumber) == 0 || issued() != 2 {
		t.Errorf("expected a second certificate, issued %d", issued())
	}
}
