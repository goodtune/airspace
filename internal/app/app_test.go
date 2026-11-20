package app

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/goodtune/airspace/internal/config"
	"github.com/goodtune/airspace/internal/fakeopensky"
	"github.com/goodtune/airspace/internal/hub"
)

const secret = "s3cr3t-value-never-shown"

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type rig struct {
	t    *testing.T
	fake *fakeopensky.Fake
	app  *App
	url  string
	logs *syncBuf
}

func start(t *testing.T, mod func(*config.Config)) *rig {
	t.Helper()
	fake := fakeopensky.New()
	up := httptest.NewServer(fake.Handler())
	t.Cleanup(up.Close)
	cfg := &config.Config{
		ClientID: "client-id", ClientSecret: secret,
		TokenURL: up.URL + "/token", APIURL: up.URL + "/api",
		PollInterval: 50 * time.Millisecond, MinInterval: 20 * time.Millisecond, Resolution: 20 * time.Millisecond,
		DailyCredits: 4000, DailyUseHours: 2, CreditReserve: 200,
		HiddenTimeout: 150 * time.Millisecond, SessionGrace: 150 * time.Millisecond,
		DBPath: filepath.Join(t.TempDir(), "a.db"), StartView: [3]float64{151.2, -33.9, 450000},
	}
	if mod != nil {
		mod(cfg)
	}
	logs := &syncBuf{}
	static := fstest.MapFS{
		"index.html":       {Data: []byte("<!doctype html><title>Airspace</title>")},
		"app.js":           {Data: []byte("console.log(1)")},
		"vendor/lib.js":    {Data: []byte("console.log(2)")},
		"vendor/data.wasm": {Data: []byte{0, 97, 115, 109}},
	}
	a := New(cfg, static, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return &rig{t: t, fake: fake, app: a, url: "http://" + ln.Addr().String(), logs: logs}
}

func (r *rig) waitFor(what string, cond func() bool) {
	r.t.Helper()
	for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	r.t.Fatalf("timed out waiting for %s", what)
}

func (r *rig) get(path string, hdr ...string) *http.Response {
	r.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, r.url+path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	// No transparent gzip, so the tests see the real encoding.
	resp, err := (&http.Transport{DisableCompression: true}).RoundTrip(req)
	if err != nil {
		r.t.Fatal(err)
	}
	return resp
}

func (r *rig) getJSON(path string, into any) int {
	r.t.Helper()
	resp := r.get(path)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if into != nil {
		if err := json.Unmarshal(body, into); err != nil {
			r.t.Fatalf("GET %s: bad JSON %q: %v", path, body, err)
		}
	}
	return resp.StatusCode
}

func (r *rig) send(method, path string, body any) int {
	r.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, r.url+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// sseClient is a simulated browser holding the Datastar stream.
type sseClient struct {
	id     string
	mu     sync.Mutex
	state  hub.State
	frames int
	cancel context.CancelFunc
	done   chan struct{}
}

func (r *rig) connect() *sseClient {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, r.url+"/api/stream", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Datastar-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		r.t.Fatal(err)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		cancel()
		r.t.Fatalf("stream content type %q", ct)
	}
	c := &sseClient{cancel: cancel, done: make(chan struct{})}
	first := make(chan struct{})
	go func() {
		defer close(c.done)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<16), 1<<22)
		event := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: signals ") && event == "datastar-patch-signals":
				var p struct {
					ClientID string `json:"clientId"`
					hub.State
				}
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: signals ")), &p) != nil {
					continue
				}
				c.mu.Lock()
				c.id, c.state = p.ClientID, p.State
				c.frames++
				if c.frames == 1 {
					close(first)
				}
				c.mu.Unlock()
			}
		}
	}()
	select {
	case <-first:
	case <-time.After(3 * time.Second):
		cancel()
		r.t.Fatal("no state received on connect")
	}
	r.t.Cleanup(c.close)
	return c
}

func (c *sseClient) ID() string { c.mu.Lock(); defer c.mu.Unlock(); return c.id }
func (c *sseClient) close()     { c.cancel(); <-c.done }
func (c *sseClient) get() hub.State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (r *rig) act(c *sseClient, name string, body map[string]any) {
	r.t.Helper()
	body["clientId"] = c.ID()
	if code := r.send(http.MethodPost, "/api/action/"+name, body); code != http.StatusNoContent {
		r.t.Fatalf("action %s: HTTP %d", name, code)
	}
}

// quiet asserts no upstream request of any kind happens for d.
func (r *rig) quiet(d time.Duration) {
	r.t.Helper()
	time.Sleep(80 * time.Millisecond)
	before := r.fake.Requests()
	time.Sleep(d)
	if after := r.fake.Requests(); after != before {
		r.t.Fatalf("expected no upstream requests, saw %d", after-before)
	}
}

func TestStartupAndReadEndpointsMakeNoUpstreamRequest(t *testing.T) {
	r := start(t, nil)
	time.Sleep(300 * time.Millisecond)
	for i := 0; i < 5; i++ {
		for _, p := range []string{"/api/states", "/api/status", "/healthz", "/api/sessions", "/", "/app.js"} {
			resp := r.get(p)
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s: HTTP %d", p, resp.StatusCode)
			}
		}
		time.Sleep(60 * time.Millisecond)
	}
	if n := r.fake.Requests(); n != 0 {
		t.Fatalf("%d upstream requests with no client connected", n)
	}
	var st hub.State
	r.getJSON("/api/status", &st)
	if st.Status.State != "stopped" || st.Status.Active != 0 {
		t.Errorf("status %+v: read endpoints must not count as activity", st.Status)
	}
	// An action from an unknown client does nothing either.
	r.send(http.MethodPost, "/api/action/input", map[string]any{"clientId": "nobody"})
	r.quiet(200 * time.Millisecond)
}

func TestStreamDisconnectStopsPolling(t *testing.T) {
	r := start(t, nil)
	r.fake.SetTokenTTL(120 * time.Millisecond)
	c := r.connect()
	r.waitFor("polls", func() bool { return r.fake.StateRequests() >= 2 })
	c.close()
	r.quiet(500 * time.Millisecond) // many intervals, and past the token expiry
}

func TestTwoClientsConverge(t *testing.T) {
	r := start(t, nil)
	a, b := r.connect(), r.connect()
	if a.ID() == "" || a.ID() == b.ID() {
		t.Fatalf("client ids %q %q", a.ID(), b.ID())
	}
	r.act(a, "interval", map[string]any{"seconds": 0.08})
	r.act(a, "select", map[string]any{"icao24": "a00001"})
	r.act(a, "camera", map[string]any{
		"camera": map[string]any{"lon": 10, "lat": 20, "height": 300000, "heading": 0, "pitch": -90},
		"view":   map[string]any{"west": 8, "south": 18, "east": 12, "north": 22},
	})
	r.waitFor("B to see A's changes", func() bool {
		s := b.get()
		return s.Interval.Seconds == 0.08 && s.Selected == "a00001" && s.Camera.Lon == 10 && s.Camera.By == a.ID()
	})
	// A is told who moved the camera, so it can ignore its own update.
	r.waitFor("A to see its own id on the camera", func() bool { return a.get().Camera.By == a.ID() })
	r.act(b, "recording", map[string]any{"value": "off"})
	r.act(b, "coverage", map[string]any{"value": "global"})
	r.waitFor("A to see B's changes", func() bool {
		s := a.get()
		return s.Recording == "off" && s.Coverage == "global" && s.Scope.Cost == 4 && s.Status.Connected == 2
	})
	if code := r.send(http.MethodPost, "/api/action/coverage", map[string]any{"clientId": a.ID(), "value": "everything"}); code != http.StatusBadRequest {
		t.Errorf("bad coverage value: HTTP %d", code)
	}
}

func TestReconnectKeepsClientID(t *testing.T) {
	r := start(t, nil)
	a := r.connect()
	q := fmt.Sprintf(`/api/stream?datastar={"clientId":%q}`, a.ID())
	req, _ := http.NewRequest(http.MethodGet, r.url+strings.ReplaceAll(strings.ReplaceAll(q, `"`, "%22"), "{", "%7B"), nil)
	req.URL.RawQuery = strings.ReplaceAll(req.URL.RawQuery, "}", "%7D")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<16), 1<<22)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: signals ") {
			if !strings.Contains(sc.Text(), `"clientId":"`+a.ID()+`"`) {
				t.Fatalf("reconnect got a new id: %s", sc.Text()[:80])
			}
			break
		}
	}
	var st hub.State
	r.getJSON("/api/status", &st)
	if st.Status.Connected != 1 {
		t.Errorf("connected %d, want 1 client with two connections", st.Status.Connected)
	}
}

func TestStatesETagAndGzip(t *testing.T) {
	r := start(t, nil)
	// Before any poll: an empty snapshot.
	var empty struct {
		Version  int64   `json:"version"`
		Aircraft [][]any `json:"aircraft"`
	}
	if code := r.getJSON("/api/states", &empty); code != 200 || empty.Version != 0 || len(empty.Aircraft) != 0 {
		t.Fatalf("empty snapshot: %d %+v", code, empty)
	}
	r.connect()
	r.waitFor("snapshot", func() bool { v, _, _, ok := r.app.Hub.Snapshot(); return ok && v >= 1 })

	resp := r.get("/api/states", "Accept-Encoding", "gzip")
	if resp.Header.Get("Content-Encoding") != "gzip" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("headers %v", resp.Header)
	}
	etag := resp.Header.Get("ETag")
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var snap struct {
		Version  int64    `json:"version"`
		Time     int64    `json:"time"`
		Fields   []string `json:"fields"`
		Scope    struct{ Cost int }
		Aircraft [][]any `json:"aircraft"`
	}
	if err := json.NewDecoder(zr).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if etag == "" || snap.Version < 1 || snap.Time == 0 || len(snap.Fields) != 14 || snap.Scope.Cost != 1 {
		t.Fatalf("snapshot %+v etag %q", snap, etag)
	}
	for _, a := range snap.Aircraft {
		if len(a) != 14 || a[3] == nil || a[4] == nil {
			t.Fatalf("aircraft with a null position or wrong shape: %v", a)
		}
	}
	// Uncompressed when not asked for.
	plain := r.get("/api/states")
	if plain.Header.Get("Content-Encoding") != "" {
		t.Error("gzip sent without Accept-Encoding")
	}
	plain.Body.Close()
	// A matching ETag gets 304, unless a newer snapshot has landed.
	for i := 0; i < 20; i++ {
		probe := r.get("/api/states")
		tag := probe.Header.Get("ETag")
		probe.Body.Close()
		again := r.get("/api/states", "If-None-Match", tag)
		again.Body.Close()
		if again.StatusCode == http.StatusNotModified {
			return
		}
	}
	t.Error("never got a 304 for a matching ETag")
}

func TestStaticAssets(t *testing.T) {
	r := start(t, nil)
	for p, want := range map[string][2]string{
		"/":                 {"text/html", "no-cache"},
		"/app.js":           {"text/javascript", "no-cache"},
		"/vendor/lib.js":    {"text/javascript", "max-age"},
		"/vendor/data.wasm": {"application/wasm", "max-age"},
	} {
		resp := r.get(p)
		resp.Body.Close()
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), want[0]) || !strings.Contains(resp.Header.Get("Cache-Control"), want[1]) || resp.Header.Get("ETag") == "" {
			t.Errorf("%s: headers %v", p, resp.Header)
		}
		again := r.get(p, "If-None-Match", resp.Header.Get("ETag"))
		again.Body.Close()
		if again.StatusCode != http.StatusNotModified {
			t.Errorf("%s: conditional request got %d", p, again.StatusCode)
		}
	}
	if resp := r.get("/missing.js"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing file: HTTP %d", resp.StatusCode)
	}
}

type sessionsResp struct {
	Sessions []struct {
		ID        int64
		Snapshots int
		Name      string
		Coverage  string
		End       int64
	}
}

func TestRecordReplayEndpoints(t *testing.T) {
	r := start(t, nil)
	c := r.connect()
	r.act(c, "camera", map[string]any{
		"camera": map[string]any{"lon": 10, "lat": 20, "height": 300000, "heading": 0, "pitch": -90},
		"view":   map[string]any{"west": 8, "south": 18, "east": 12, "north": 22},
	})
	r.waitFor("a dozen polls", func() bool { return r.fake.StateRequests() >= 12 })
	c.close()
	var list sessionsResp
	r.waitFor("the session to close", func() bool {
		r.getJSON("/api/sessions", &list)
		return len(list.Sessions) == 1 && list.Sessions[0].End != 0
	})
	sess := list.Sessions[0]
	if sess.Snapshots < 11 || sess.Coverage != "viewport" {
		t.Fatalf("session %+v", sess)
	}
	upstream := r.fake.Requests()
	base := fmt.Sprintf("/api/sessions/%d", sess.ID)

	var detail struct {
		Session struct{ Snapshots int }
		Index   []struct {
			Time     int64
			Aircraft int
		}
	}
	if code := r.getJSON(base, &detail); code != 200 || len(detail.Index) != sess.Snapshots {
		t.Fatalf("detail: %d, %d index rows for %d snapshots", code, len(detail.Index), sess.Snapshots)
	}

	// Page through every snapshot, five at a time.
	type page struct {
		Snapshots []struct {
			Version  int64
			Fetched  int64
			Aircraft [][]any
		}
		More bool
		Next int64
	}
	var times []int64
	from, pages := int64(0), 0
	for {
		var p page
		if code := r.getJSON(fmt.Sprintf("%s/snapshots?from=%d&limit=5", base, from), &p); code != 200 {
			t.Fatalf("snapshots page: HTTP %d", code)
		}
		pages++
		for _, s := range p.Snapshots {
			times = append(times, s.Fetched)
		}
		if len(p.Snapshots) > 5 {
			t.Fatalf("page of %d exceeds the limit", len(p.Snapshots))
		}
		if !p.More {
			break
		}
		from = p.Next
	}
	if len(times) != sess.Snapshots || pages < 3 {
		t.Fatalf("paged %d snapshots in %d pages, want %d", len(times), pages, sess.Snapshots)
	}
	for i := range times {
		if times[i] != detail.Index[i].Time {
			t.Fatalf("snapshot %d out of order", i)
		}
	}
	// A time range returns only what is inside it.
	var p page
	r.getJSON(fmt.Sprintf("%s/snapshots?from=%d&to=%d&limit=40", base, times[2], times[4]), &p)
	if len(p.Snapshots) != 3 || p.More {
		t.Errorf("range returned %d snapshots, want 3", len(p.Snapshots))
	}
	// Gzip when asked for.
	zresp := r.get(base+"/snapshots?limit=2", "Accept-Encoding", "gzip")
	if zresp.Header.Get("Content-Encoding") != "gzip" {
		t.Error("snapshot page not gzipped")
	}
	zresp.Body.Close()

	// Events: ordered, paged, and holding the camera track.
	var all []struct {
		ID   int64
		Time int64
		Type string
	}
	after := int64(0)
	for {
		var ev struct {
			Events []struct {
				ID   int64
				Time int64
				Type string
			}
			More  bool
			After int64
		}
		if code := r.getJSON(fmt.Sprintf("%s/events?limit=2&after=%d", base, after), &ev); code != 200 {
			t.Fatalf("events: HTTP %d", code)
		}
		all = append(all, ev.Events...)
		if !ev.More {
			break
		}
		after = ev.After
	}
	types := map[string]int{}
	for i, e := range all {
		types[e.Type]++
		if i > 0 && (e.Time < all[i-1].Time || e.ID <= all[i-1].ID) {
			t.Fatalf("event %d out of order", i)
		}
	}
	if all[0].Type != "state" || types["camera"] != 1 || types["connect"] == 0 || types["scope"] == 0 || types["disconnect"] != 1 {
		t.Errorf("event track %v, first %q", types, all[0].Type)
	}

	// Rename, then delete.
	if code := r.send(http.MethodPatch, base, map[string]string{"name": "Test flight"}); code != 200 {
		t.Fatalf("rename: HTTP %d", code)
	}
	r.getJSON("/api/sessions", &list)
	if list.Sessions[0].Name != "Test flight" {
		t.Errorf("name %q", list.Sessions[0].Name)
	}
	if code := r.send(http.MethodDelete, base, nil); code != http.StatusNoContent {
		t.Fatalf("delete: HTTP %d", code)
	}
	if code := r.getJSON(base, nil); code != http.StatusNotFound {
		t.Errorf("deleted session: HTTP %d", code)
	}
	if code := r.getJSON(base+"/snapshots", nil); code != http.StatusNotFound {
		t.Errorf("deleted session snapshots: HTTP %d", code)
	}
	if code := r.getJSON("/api/sessions/abc", nil); code != http.StatusBadRequest {
		t.Errorf("bad id: HTTP %d", code)
	}
	// None of the replay endpoints touched OpenSky.
	if n := r.fake.Requests(); n != upstream {
		t.Fatalf("replay endpoints caused %d upstream requests", n-upstream)
	}
}

func TestStorageFailureDegradesToLiveOnly(t *testing.T) {
	r := start(t, func(c *config.Config) { c.DBPath = filepath.Join(c.DBPath, "not", "a", "dir", "x.db") })
	c := r.connect()
	r.waitFor("live polling", func() bool { return c.get().Snapshot.Version >= 2 })
	if s := c.get(); !strings.Contains(s.Status.Storage, "storage unavailable") {
		t.Errorf("no storage warning: %+v", s.Status)
	}
	if code := r.getJSON("/api/sessions", nil); code != http.StatusServiceUnavailable {
		t.Errorf("sessions with no storage: HTTP %d", code)
	}
}

func TestNoSecretOrTokenLeaks(t *testing.T) {
	r := start(t, nil)
	c := r.connect()
	r.waitFor("polls", func() bool { return c.get().Snapshot.Version >= 2 })
	r.fake.SetFailing(true)
	r.waitFor("an upstream error", func() bool { return c.get().Status.Error != "" })
	var bodies strings.Builder
	for _, p := range []string{"/api/status", "/api/states", "/api/sessions", "/"} {
		resp := r.get(p)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		bodies.Write(b)
	}
	state, _ := json.Marshal(c.get())
	for name, text := range map[string]string{"logs": r.logs.String(), "responses": bodies.String(), "signals": string(state)} {
		if strings.Contains(text, secret) || strings.Contains(text, "fake-token-") {
			t.Errorf("%s contain the secret or a token", name)
		}
	}
	if !strings.Contains(r.logs.String(), "upstream poll") {
		t.Error("expected upstream polls to be logged")
	}
}
