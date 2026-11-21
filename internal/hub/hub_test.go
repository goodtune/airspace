package hub

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goodtune/airspace/internal/config"
	"github.com/goodtune/airspace/internal/fakeopensky"
	"github.com/goodtune/airspace/internal/model"
	"github.com/goodtune/airspace/internal/opensky"
	"github.com/goodtune/airspace/internal/scope"
)

const tick = 40 * time.Millisecond // test poll interval

type memRec struct {
	mu     sync.Mutex
	starts []int64
	ends   []int64
	snaps  []*model.Snapshot
	events []string
	meta   map[string]any
	health string
}

func (m *memRec) SessionStart(id int64, _ time.Time) {
	m.mu.Lock()
	m.starts = append(m.starts, id)
	m.mu.Unlock()
}
func (m *memRec) SessionEnd(id int64, _ time.Time) {
	m.mu.Lock()
	m.ends = append(m.ends, id)
	m.mu.Unlock()
}
func (m *memRec) Snapshot(_ int64, s *model.Snapshot) {
	m.mu.Lock()
	m.snaps = append(m.snaps, s)
	m.mu.Unlock()
}
func (m *memRec) Event(_ int64, _ time.Time, typ string, _ any) {
	m.mu.Lock()
	m.events = append(m.events, typ)
	m.mu.Unlock()
}
func (m *memRec) SaveMeta(k string, v any) {
	m.mu.Lock()
	if m.meta == nil {
		m.meta = map[string]any{}
	}
	m.meta[k] = v
	m.mu.Unlock()
}
func (m *memRec) Health() string { return m.health }
func (m *memRec) counts() (starts, ends, snaps int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.starts), len(m.ends), len(m.snaps)
}
func (m *memRec) has(typ string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.events {
		if e == typ {
			return true
		}
	}
	return false
}

type rig struct {
	t    *testing.T
	fake *fakeopensky.Fake
	hub  *Hub
	rec  *memRec
	cfg  *config.Config
}

func testConfig() *config.Config {
	return &config.Config{
		PollInterval: tick, MinInterval: 20 * time.Millisecond, Resolution: 20 * time.Millisecond,
		DailyCredits: 4000, DailyUseHours: 2, CreditReserve: 200,
		HiddenTimeout: 150 * time.Millisecond, IdleTimeout: 0, SessionGrace: 200 * time.Millisecond,
		StartView: [3]float64{151.2, -33.9, 450000},
	}
}

func newRig(t *testing.T, mod func(*config.Config)) *rig {
	t.Helper()
	cfg := testConfig()
	if mod != nil {
		mod(cfg)
	}
	fake := fakeopensky.New()
	srv := httptest.NewServer(fake.Handler())
	t.Cleanup(srv.Close)
	up := &opensky.Client{HTTP: srv.Client(), TokenURL: srv.URL + "/token", APIURL: srv.URL + "/api", ID: "id", Secret: "secret"}
	rec := &memRec{}
	h := New(cfg, up, rec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return &rig{t: t, fake: fake, hub: h, rec: rec, cfg: cfg}
}

func (r *rig) waitFor(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.t.Fatalf("timed out waiting for %s", what)
}

// quiet asserts that no upstream request of any kind happens for d.
func (r *rig) quiet(d time.Duration) {
	r.t.Helper()
	time.Sleep(60 * time.Millisecond) // let an in-flight request finish
	before := r.fake.Requests()
	time.Sleep(d)
	if after := r.fake.Requests(); after != before {
		r.t.Fatalf("expected no upstream requests, saw %d", after-before)
	}
}

func TestNoClientsNoRequests(t *testing.T) {
	r := newRig(t, nil)
	time.Sleep(8 * tick)
	if n := r.fake.Requests(); n != 0 {
		t.Fatalf("%d upstream requests with nobody watching", n)
	}
	if s := r.hub.State(); s.Status.State != "stopped" {
		t.Errorf("state %q, want stopped", s.Status.State)
	}
}

func TestDisconnectStopsEverything(t *testing.T) {
	r := newRig(t, nil)
	r.fake.SetTokenTTL(100 * time.Millisecond)
	_, _, release := r.hub.Connect("")
	r.waitFor("polls", func() bool { return r.fake.StateRequests() >= 2 })
	release()
	// Several intervals, and well past the token expiry.
	r.quiet(10 * tick)
	if r.hub.State().Status.State != "stopped" {
		t.Error("want stopped after the last client left")
	}
}

func TestLazyTokenReuseAndRefresh(t *testing.T) {
	r := newRig(t, nil)
	if r.fake.TokenRequests() != 0 {
		t.Fatal("token fetched before any poll")
	}
	_, _, release := r.hub.Connect("")
	defer release()
	r.waitFor("polls", func() bool { return r.fake.StateRequests() >= 4 })
	if n := r.fake.TokenRequests(); n != 1 {
		t.Fatalf("%d token requests, want 1 (reuse)", n)
	}
	// A 401 leads to one refresh and one retry, and the poll still succeeds.
	v := r.hub.State().Snapshot.Version
	r.fake.ExpireTokens()
	r.waitFor("a poll after the 401", func() bool { return r.hub.State().Snapshot.Version > v+1 })
	if n := r.fake.TokenRequests(); n != 2 {
		t.Fatalf("%d token requests after a 401, want 2", n)
	}
	if e := r.hub.State().Status.Error; e != "" {
		t.Errorf("unexpected error after refresh: %s", e)
	}
}

func TestHiddenTabStopsCounting(t *testing.T) {
	r := newRig(t, nil)
	id, _, release := r.hub.Connect("")
	defer release()
	r.waitFor("polls", func() bool { return r.fake.StateRequests() >= 1 })
	r.hub.Presence(id, false, false, false)
	// Still active inside HIDDEN_TIMEOUT.
	n := r.fake.StateRequests()
	r.waitFor("polls while briefly hidden", func() bool { return r.fake.StateRequests() > n })
	r.waitFor("stop after hidden timeout", func() bool { return r.hub.State().Status.Active == 0 })
	r.quiet(6 * tick)
	r.hub.Presence(id, true, false, false)
	n = r.fake.StateRequests()
	r.waitFor("polls after becoming visible", func() bool { return r.fake.StateRequests() > n })
}

func TestIdlePauseAndResume(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.IdleTimeout = 150 * time.Millisecond })
	id, _, release := r.hub.Connect("")
	defer release()
	r.waitFor("polls", func() bool { return r.fake.StateRequests() >= 1 })
	r.waitFor("idle pause", func() bool { return r.hub.State().Status.State == "paused" })
	r.quiet(6 * tick)
	r.hub.Input(id)
	n := r.fake.StateRequests()
	r.waitFor("resume", func() bool { return r.fake.StateRequests() > n && r.hub.State().Status.State == "polling" })
	if !r.rec.has("pause") || !r.rec.has("resume") {
		t.Error("pause and resume events were not recorded")
	}
}

func TestReplayClientIsNotActive(t *testing.T) {
	r := newRig(t, nil)
	id, _, release := r.hub.Connect("")
	defer release()
	r.hub.Presence(id, true, true, false)
	r.waitFor("inactive", func() bool { return r.hub.State().Status.Active == 0 })
	r.quiet(6 * tick)
}

func TestRateLimitRetryAfter(t *testing.T) {
	r := newRig(t, nil)
	r.fake.SetRemaining(0)
	r.fake.SetRetryAfter(1)
	_, _, release := r.hub.Connect("")
	defer release()
	r.waitFor("a 429", func() bool { return r.hub.State().Status.LimitedUntil != 0 })
	s := r.hub.State()
	if s.Interval.Reason != config.ReasonLimited {
		t.Errorf("reason %q, want rate limited", s.Interval.Reason)
	}
	n := r.fake.StateRequests()
	time.Sleep(600 * time.Millisecond)
	if got := r.fake.StateRequests(); got != n {
		t.Fatalf("%d requests during retry-after, want 0", got-n)
	}
	r.fake.SetRemaining(1000)
	r.waitFor("recovery after retry-after", func() bool { return r.hub.State().Snapshot.Version > 0 })
	if s := r.hub.State(); s.Interval.Reason != "" || s.Status.Error != "" {
		t.Errorf("did not clear after recovery: %+v %+v", s.Interval, s.Status)
	}
}

func TestReserveGuardStretchesInterval(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.PollInterval = time.Second; c.MaxInterval = 2 * time.Second })
	r.fake.SetRemaining(51) // 50 after the first 1-credit poll: a quarter of the reserve
	_, _, release := r.hub.Connect("")
	defer release()
	r.waitFor("guard", func() bool { return r.hub.State().Interval.Reason == config.ReasonReserve })
	s := r.hub.State()
	// Stretched fourfold, beyond MAX_INTERVAL.
	if s.Interval.Seconds != 4 || s.Interval.Requested != 1 {
		t.Errorf("interval %+v, want 4 s from a 1 s request", s.Interval)
	}
	if !r.rec.has("interval") {
		t.Error("server override was not recorded as an event")
	}
}

func TestForecastArithmetic(t *testing.T) {
	// Burn rates from the table in the requirements.
	for _, c := range []struct {
		cost int
		ivl  time.Duration
		burn float64
	}{{1, 5 * time.Second, 720}, {2, 5 * time.Second, 1440}, {3, 5 * time.Second, 2160}, {4, 5 * time.Second, 2880},
		{1, 10 * time.Second, 360}, {4, 10 * time.Second, 1440}, {3, 60 * time.Second, 180}} {
		f := forecast(c.cost, c.ivl, 3600, true)
		if f.Burn != c.burn {
			t.Errorf("cost %d at %s: burn %v, want %v", c.cost, c.ivl, f.Burn, c.burn)
		}
		if want := 3600 / c.burn * 3600; f.SecondsLeft != want {
			t.Errorf("cost %d at %s: %v s left, want %v", c.cost, c.ivl, f.SecondsLeft, want)
		}
	}
	if f := forecast(1, 10*time.Second, 0, false); f.SecondsLeft != -1 {
		t.Errorf("unknown balance should give -1, got %v", f.SecondsLeft)
	}
}

func TestIntervalBounds(t *testing.T) {
	r := newRig(t, func(c *config.Config) {
		c.MinInterval, c.MaxInterval, c.PollInterval = 30*time.Second, 2*time.Minute, time.Minute
	})
	id, _, release := r.hub.Connect("")
	defer release()
	for _, c := range []struct {
		ask, want time.Duration
		reason    string
	}{{10 * time.Second, 30 * time.Second, config.ReasonMin}, {45 * time.Second, 45 * time.Second, ""},
		{10 * time.Minute, 2 * time.Minute, config.ReasonMax}} {
		r.hub.SetInterval(id, c.ask)
		s := r.hub.State()
		if s.Interval.Seconds != c.want.Seconds() || s.Interval.Reason != c.reason {
			t.Errorf("ask %s: got %+v, want %s %q", c.ask, s.Interval, c.want, c.reason)
		}
		if s.Bounds.Min != 30 || s.Bounds.Max != 120 {
			t.Errorf("bounds %+v", s.Bounds)
		}
	}
}

func TestAutoFloorFollowsCost(t *testing.T) {
	r := newRig(t, func(c *config.Config) {
		c.MinAuto, c.Resolution, c.PollInterval, c.IdleTimeout = true, 5*time.Second, 5*time.Second, 0
	})
	id, _, release := r.hub.Connect("")
	defer release()
	cam := Camera{Lon: 10, Lat: 10, Height: 1e6, Pitch: -90}
	set := func(side float64) State {
		r.hub.SetCamera(id, cam, &scope.View{West: 0, South: 0, East: side, North: side})
		return r.hub.State()
	}
	if s := set(4); s.Scope.Cost != 1 || s.Interval.Seconds != 5 || s.Bounds.Min != 5 || s.Interval.Reason != "" {
		t.Errorf("tier 1: %+v %+v cost %d", s.Interval, s.Bounds, s.Scope.Cost)
	}
	if s := set(19); s.Scope.Cost != 3 || s.Interval.Seconds != 6 || s.Bounds.Min != 6 || s.Interval.Reason != config.ReasonMin {
		t.Errorf("tier 3: %+v %+v cost %d", s.Interval, s.Bounds, s.Scope.Cost)
	}
	if s := set(60); !s.Scope.Global || s.Interval.Seconds != 8 || s.Bounds.Min != 8 {
		t.Errorf("global: %+v %+v", s.Interval, s.Bounds)
	}
	// Zooming back in returns to the interval the client asked for.
	if s := set(4); s.Interval.Seconds != 5 || s.Interval.Reason != "" || s.Interval.Requested != 5 {
		t.Errorf("back to tier 1: %+v", s.Interval)
	}
}

func TestPrecedence(t *testing.T) {
	// Client request < operator bounds < reserve guard < 429 < nobody watching.
	r := newRig(t, func(c *config.Config) {
		c.PollInterval, c.MinInterval, c.MaxInterval = 100*time.Millisecond, 60*time.Millisecond, 200*time.Millisecond
	})
	id, _, release := r.hub.Connect("")
	// 4: operator bounds beat the client request.
	r.hub.SetInterval(id, time.Millisecond)
	if s := r.hub.State(); s.Interval.Seconds != 0.06 || s.Interval.Reason != config.ReasonMin {
		t.Fatalf("bounds did not beat the request: %+v", s.Interval)
	}
	// 3: the reserve guard beats the operator maximum.
	r.fake.SetRemaining(21)
	r.waitFor("guard", func() bool { return r.hub.State().Interval.Reason == config.ReasonReserve })
	if s := r.hub.State(); s.Interval.Seconds <= 0.2 {
		t.Fatalf("guard should exceed MAX_INTERVAL: %+v", s.Interval)
	}
	// 2: a 429 beats the guard.
	r.fake.SetRemaining(0)
	r.fake.SetRetryAfter(30)
	r.waitFor("429", func() bool { return r.hub.State().Interval.Reason == config.ReasonLimited })
	r.quiet(300 * time.Millisecond)
	// 1: with nobody watching, nothing is requested even after credits return.
	release()
	r.fake.SetRemaining(4000)
	r.quiet(300 * time.Millisecond)
	if r.hub.State().Status.State != "stopped" {
		t.Error("want stopped")
	}
}

func TestCoverageGlobalAndReset(t *testing.T) {
	r := newRig(t, nil)
	id, _, release := r.hub.Connect("")
	r.waitFor("viewport poll", func() bool { return r.fake.StateRequests() >= 1 })
	if q := r.fake.Queries()[0]; !strings.Contains(q, "lamin=") {
		t.Fatalf("default coverage should query a box, got %q", q)
	}
	r.hub.SetCoverage(id, CoverageGlobal)
	if s := r.hub.State(); s.Scope.Cost != 4 || !s.Scope.Global || s.Credits.Cost != 4 {
		t.Fatalf("global coverage: %+v", s.Scope)
	}
	n := r.fake.StateRequests()
	r.waitFor("global polls", func() bool { return r.fake.StateRequests() >= n+2 })
	qs := r.fake.Queries()
	if q := qs[len(qs)-1]; q != "" {
		t.Errorf("global coverage sent a box query %q", q)
	}
	if s := r.hub.State(); !s.Snapshot.Scope.Global {
		t.Errorf("snapshot scope should be global: %+v", s.Snapshot.Scope)
	}
	release()
	r.waitFor("session end", func() bool { _, ends, _ := r.rec.counts(); return ends == 1 })
	if c := r.hub.State().Coverage; c != CoverageViewport {
		t.Errorf("coverage %q after session end, want viewport", c)
	}
}

func TestSharedStateReachesOtherClient(t *testing.T) {
	r := newRig(t, nil)
	a, _, relA := r.hub.Connect("")
	defer relA()
	b, notifyB, relB := r.hub.Connect("")
	defer relB()
	if a == b {
		t.Fatal("clients share an id")
	}
	drain := func() {
		for {
			select {
			case <-notifyB:
			default:
				return
			}
		}
	}
	expect := func(what string) {
		t.Helper()
		select {
		case <-notifyB:
		case <-time.After(time.Second):
			t.Fatalf("client B was not told about %s", what)
		}
	}
	drain()
	r.hub.SetInterval(a, 70*time.Millisecond)
	expect("interval")
	if s := r.hub.State(); s.Interval.Seconds != 0.07 {
		t.Errorf("interval %+v", s.Interval)
	}
	drain()
	r.hub.Select(a, "abc123")
	expect("selection")
	drain()
	r.hub.SetCamera(a, Camera{Lon: 1, Lat: 2, Height: 3000, Pitch: -45}, nil)
	expect("camera")
	s := r.hub.State()
	if s.Selected != "abc123" || s.Camera.By != a || s.Camera.Lon != 1 || s.Camera.Seq == 0 {
		t.Errorf("state after changes: %+v", s)
	}
	seq := s.Camera.Seq
	r.hub.SetCamera(b, Camera{Lon: 5, Lat: 6, Height: 3000, Pitch: -45}, nil)
	if s := r.hub.State(); s.Camera.By != b || s.Camera.Seq != seq+1 {
		t.Errorf("last writer should win: %+v", s.Camera)
	}
	if s := r.hub.State(); s.Status.Connected != 2 || s.Status.Active != 2 {
		t.Errorf("client counts: %+v", s.Status)
	}
}

func TestRequestCountSameWithOneOrFiveClients(t *testing.T) {
	count := func(n int) int {
		r := newRig(t, func(c *config.Config) { c.PollInterval, c.MinInterval = 100*time.Millisecond, 100*time.Millisecond })
		for i := 0; i < n; i++ {
			_, _, release := r.hub.Connect("")
			defer release()
		}
		time.Sleep(1050 * time.Millisecond)
		return r.fake.StateRequests()
	}
	one, five := count(1), count(5)
	if d := one - five; d < -1 || d > 1 || one < 8 || one > 12 {
		t.Errorf("1 client made %d requests, 5 clients made %d", one, five)
	}
}

func TestCameraMoveDoesNotTriggerPoll(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.PollInterval, c.MinInterval = 400*time.Millisecond, 400*time.Millisecond })
	id, _, release := r.hub.Connect("")
	defer release()
	r.waitFor("first poll", func() bool { return r.fake.StateRequests() == 1 })
	for i := 0; i < 20; i++ {
		r.hub.SetCamera(id, Camera{Lon: float64(i), Lat: 0, Height: 5e5, Pitch: -90},
			&scope.View{West: float64(i), South: 0, East: float64(i) + 3, North: 3})
		time.Sleep(5 * time.Millisecond)
	}
	if n := r.fake.StateRequests(); n != 1 {
		t.Fatalf("camera moves caused %d extra requests", n-1)
	}
	// The new scope applies at the next scheduled poll.
	r.waitFor("next poll", func() bool { return r.fake.StateRequests() == 2 })
	if q := r.fake.Queries()[1]; !strings.Contains(q, "lomin=18.") {
		t.Errorf("next poll did not use the new scope: %q", q)
	}
}

func TestDetachedClientAddsBox(t *testing.T) {
	r := newRig(t, nil)
	a, _, relA := r.hub.Connect("")
	defer relA()
	b, _, relB := r.hub.Connect("")
	r.hub.SetCamera(a, Camera{Lon: 2, Lat: 2, Height: 5e5, Pitch: -90}, &scope.View{West: 0, South: 0, East: 4, North: 4})
	r.hub.Presence(b, true, false, true)
	r.hub.SetView(b, scope.View{West: 100, South: 30, East: 104, North: 34})
	if s := r.hub.State(); len(s.Scope.Boxes) != 2 || s.Scope.Cost != 2 {
		t.Fatalf("want two boxes at cost 2, got %+v", s.Scope)
	}
	n := r.fake.StateRequests()
	r.waitFor("two-box poll", func() bool { return r.fake.StateRequests() >= n+4 })
	if s := r.hub.State(); len(s.Snapshot.Scope.Boxes) != 2 {
		t.Errorf("snapshot scope: %+v", s.Snapshot.Scope)
	}
	// A detached client must not move the shared camera.
	r.hub.SetCamera(b, Camera{Lon: 50, Lat: 50, Height: 5e5, Pitch: -90}, nil)
	if s := r.hub.State(); s.Camera.By != a {
		t.Errorf("detached client moved the shared camera: %+v", s.Camera)
	}
	relB()
	if s := r.hub.State(); len(s.Scope.Boxes) != 1 || s.Scope.Cost != 1 {
		t.Errorf("after detach client left: %+v", s.Scope)
	}
}

func TestUpstreamFailureKeepsLastSnapshot(t *testing.T) {
	r := newRig(t, nil)
	_, _, release := r.hub.Connect("")
	defer release()
	r.waitFor("snapshot", func() bool { return r.hub.State().Snapshot.Version >= 1 })
	r.fake.SetFailing(true)
	r.waitFor("error", func() bool { return r.hub.State().Status.Error != "" })
	v, _, _, ok := r.hub.Snapshot()
	if !ok || v < 1 {
		t.Fatal("last good snapshot was dropped")
	}
	// Backoff: far fewer requests than one an interval.
	n := r.fake.StateRequests()
	time.Sleep(12 * tick)
	if got := r.fake.StateRequests() - n; got > 5 {
		t.Errorf("%d requests in 12 intervals while failing; want backoff", got)
	}
	r.fake.SetFailing(false)
	r.waitFor("recovery", func() bool { s := r.hub.State(); return s.Status.Error == "" && s.Snapshot.Version > v })
}

func TestSessionBoundariesAndGrace(t *testing.T) {
	r := newRig(t, nil)
	id, _, release := r.hub.Connect("")
	r.waitFor("session start", func() bool { s, _, n := r.rec.counts(); return s == 1 && n >= 1 })
	release()
	// A reload inside the grace period: polling is stopped, the session is not split.
	r.quiet(80 * time.Millisecond)
	if _, ends, _ := r.rec.counts(); ends != 0 {
		t.Fatal("session ended inside the grace period")
	}
	_, _, release = r.hub.Connect(id)
	_, _, n := r.rec.counts()
	r.waitFor("more snapshots", func() bool { _, _, m := r.rec.counts(); return m > n })
	if starts, _, _ := r.rec.counts(); starts != 1 {
		t.Fatalf("reload split the session: %d starts", starts)
	}
	release()
	r.waitFor("session end", func() bool { _, ends, _ := r.rec.counts(); return ends == 1 })
	r.quiet(4 * tick)
	_, _, release = r.hub.Connect("")
	defer release()
	r.waitFor("second session", func() bool { starts, _, _ := r.rec.counts(); return starts == 2 })
}

func TestRecordingDoesNotChangeScopeOrCount(t *testing.T) {
	run := func(record bool) (requests int, queries []string, snaps int) {
		r := newRig(t, func(c *config.Config) { c.PollInterval, c.MinInterval = 100*time.Millisecond, 100*time.Millisecond })
		id, _, release := r.hub.Connect("")
		defer release()
		r.hub.SetRecording(id, record)
		time.Sleep(650 * time.Millisecond)
		_, _, snaps = r.rec.counts()
		return r.fake.StateRequests(), r.fake.Queries(), snaps
	}
	onReq, onQ, onSnaps := run(true)
	offReq, offQ, offSnaps := run(false)
	if d := onReq - offReq; d < -1 || d > 1 {
		t.Errorf("recording changed the request count: on %d, off %d", onReq, offReq)
	}
	if onQ[0] != offQ[0] {
		t.Errorf("recording changed the query: %q vs %q", onQ[0], offQ[0])
	}
	if onSnaps < onReq-1 || offSnaps > 1 {
		t.Errorf("snapshots stored: on %d of %d polls, off %d", onSnaps, onReq, offSnaps)
	}
}

func TestStorageFailureLeavesLiveWorking(t *testing.T) {
	cfg := testConfig()
	fake := fakeopensky.New()
	srv := httptest.NewServer(fake.Handler())
	defer srv.Close()
	up := &opensky.Client{HTTP: http.DefaultClient, TokenURL: srv.URL + "/token", APIURL: srv.URL + "/api", ID: "id", Secret: "s"}
	h := New(cfg, up, NopRecorder{Msg: "disk is broken"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)
	_, _, release := h.Connect("")
	defer release()
	deadline := time.Now().Add(2 * time.Second)
	for h.State().Snapshot.Version < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	s := h.State()
	if s.Snapshot.Version < 2 || s.Status.Storage != "disk is broken" {
		t.Errorf("live view should work with a storage warning: %+v", s.Status)
	}
}
