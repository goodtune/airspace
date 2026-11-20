// Package hub owns the one shared session: who is watching, the poll
// interval, the camera, the query scope, credits, and recording. It runs the
// single upstream poller.
package hub

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/goodtune/airspace/internal/config"
	"github.com/goodtune/airspace/internal/model"
	"github.com/goodtune/airspace/internal/opensky"
	"github.com/goodtune/airspace/internal/scope"
)

// Upstream fetches state vectors. box is nil for the whole world.
type Upstream interface {
	States(ctx context.Context, box *scope.Box) (*opensky.Result, error)
}

// Recorder stores sessions. Every method must return without blocking.
type Recorder interface {
	SessionStart(id int64, start time.Time)
	SessionEnd(id int64, end time.Time)
	Snapshot(session int64, s *model.Snapshot)
	Event(session int64, t time.Time, typ string, payload any)
	SaveMeta(key string, v any)
	// Health is empty when storage works, else a message for the UI.
	Health() string
}

// Camera is the shared camera. By is the id of the client that set it and
// Seq increases on every change.
type Camera struct {
	Lon     float64 `json:"lon"`
	Lat     float64 `json:"lat"`
	Height  float64 `json:"height"`
	Heading float64 `json:"heading"`
	Pitch   float64 `json:"pitch"`
	By      string  `json:"by"`
	Seq     int64   `json:"seq"`
}

// SavedCamera is what is kept across restarts.
type SavedCamera struct {
	Camera Camera      `json:"camera"`
	View   *scope.View `json:"view,omitempty"`
}

// State is the full shared state, sent to clients as Datastar signals.
type State struct {
	Interval  IntervalState `json:"interval"`
	Bounds    BoundsState   `json:"bounds"`
	Camera    Camera        `json:"camera"`
	Selected  string        `json:"selected"`
	Recording string        `json:"recording"`
	Coverage  string        `json:"coverage"`
	Scope     scope.Plan    `json:"scope"`
	Snapshot  SnapshotState `json:"snapshot"`
	Credits   CreditState   `json:"credits"`
	Status    StatusState   `json:"status"`
}

type IntervalState struct {
	Seconds   float64 `json:"seconds"`
	Requested float64 `json:"requested"`
	Reason    string  `json:"reason"`
}

type BoundsState struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"` // 0 means no maximum
}

type SnapshotState struct {
	Version int64      `json:"version"`
	Time    int64      `json:"time"`
	Fetched int64      `json:"fetched"`
	Count   int        `json:"count"`
	Scope   scope.Plan `json:"scope"`
}

type Forecast struct {
	Cost        int     `json:"cost"`
	Interval    float64 `json:"interval"`
	Burn        float64 `json:"burn"`        // credits an hour
	SecondsLeft float64 `json:"secondsLeft"` // -1 when unknown
}

type CreditState struct {
	Known     bool `json:"known"`
	Remaining int  `json:"remaining"`
	Forecast
	ExhaustAt   int64    `json:"exhaustAt"` // unix ms, 0 when unknown or not polling
	RefillKnown bool     `json:"refillKnown"`
	Global      Forecast `json:"global"` // what coverage=global would cost
}

type StatusState struct {
	State        string `json:"state"` // polling, paused, stopped
	Error        string `json:"error"`
	ErrorAt      int64  `json:"errorAt"`
	LimitedUntil int64  `json:"limitedUntil"`
	Active       int    `json:"active"`
	Connected    int    `json:"connected"`
	Storage      string `json:"storage"`
	Session      int64  `json:"session"`
}

type client struct {
	id          string
	conns       int
	visible     bool
	hiddenSince time.Time
	replay      bool
	detached    bool
	view        *scope.View
}

// Hub is the shared session.
type Hub struct {
	cfg *config.Config
	up  Upstream
	rec Recorder
	log *slog.Logger

	wake chan struct{}

	mu        sync.Mutex
	clients   map[string]*client
	subs      map[chan struct{}]struct{}
	requested time.Duration
	interval  time.Duration
	reason    string
	camera    Camera
	view      *scope.View // what the shared camera sees
	selected  string
	recording bool
	coverage  string
	plan      scope.Plan

	active    int
	paused    bool
	lastInput time.Time

	session      int64
	zeroSince    time.Time
	lastAttempt  time.Time
	failures     int
	retryUntil   time.Time
	limited      bool // a 429 is in force until the next successful poll
	lastErr      string
	lastErrAt    time.Time
	remaining    int
	hasRemaining bool
	cameraSaved  time.Time
	cameraDirty  bool

	snap     *model.Snapshot
	snapJSON []byte
	snapGzip []byte
	version  int64
}

const (
	CoverageViewport = "viewport"
	CoverageGlobal   = "global"
	maxBackoff       = 5 * time.Minute
)

// New makes a hub. It makes no upstream request.
func New(cfg *config.Config, up Upstream, rec Recorder, log *slog.Logger) *Hub {
	h := &Hub{
		cfg: cfg, up: up, rec: rec, log: log,
		wake:      make(chan struct{}, 1),
		clients:   map[string]*client{},
		subs:      map[chan struct{}]struct{}{},
		requested: cfg.PollInterval,
		recording: true,
		coverage:  CoverageViewport,
		camera:    Camera{Lon: cfg.StartView[0], Lat: cfg.StartView[1], Height: cfg.StartView[2], Pitch: -90},
	}
	h.replan()
	return h
}

// Restore sets the camera saved by an earlier run.
func (h *Hub) Restore(s SavedCamera) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.Camera.Height <= 0 {
		return
	}
	s.Camera.By, s.Camera.Seq = "", 1
	h.camera, h.view = s.Camera, s.View
	h.replan()
}

// --- client lifecycle ---

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Connect registers an SSE connection. Pass the id from an earlier
// connection to keep it, or "" for a new one. Call release when the
// connection closes. notify fires whenever shared state changes.
func (h *Hub) Connect(id string) (clientID string, notify <-chan struct{}, release func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	c := h.clients[id]
	if c == nil {
		if len(id) != 16 {
			id = newID()
		}
		c = &client{id: id, visible: true}
		h.clients[id] = c
	}
	c.conns++
	if c.conns == 1 {
		h.lastInput = now
	}
	ch := make(chan struct{}, 1)
	ch <- struct{}{} // send the full state at once
	h.subs[ch] = struct{}{}
	h.changed(now) // starts a session if this is the first active client
	if c.conns == 1 {
		h.event(now, "connect", map[string]any{"client": c.id})
	}
	var once sync.Once
	return c.id, ch, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			now := time.Now()
			delete(h.subs, ch)
			c.conns--
			if c.conns <= 0 {
				delete(h.clients, c.id)
				h.event(now, "disconnect", map[string]any{"client": c.id})
			}
			h.changed(now)
		})
	}
}

func (h *Hub) isActive(c *client, now time.Time) bool {
	if c.conns <= 0 || c.replay {
		return false
	}
	return c.visible || now.Sub(c.hiddenSince) < h.cfg.HiddenTimeout
}

// Presence records a client's tab visibility and mode.
func (h *Hub) Presence(id string, visible, replay, detached bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.clients[id]
	if c == nil {
		return
	}
	now := time.Now()
	if c.visible && !visible {
		c.hiddenSince = now
	}
	c.visible, c.replay = visible, replay
	if c.detached && !detached {
		c.view = nil
	}
	c.detached = detached
	h.changed(now)
}

// Input records user input from a client. It resumes an idle pause.
func (h *Hub) Input(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clients[id] == nil {
		return
	}
	h.input(time.Now())
}

func (h *Hub) input(now time.Time) {
	h.lastInput = now
	if h.paused {
		h.paused = false
		h.event(now, "resume", nil)
	}
	h.changed(now)
}

// --- shared state changes ---

// SetInterval requests a new poll interval. It is clamped, never rejected.
func (h *Hub) SetInterval(id string, d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clients[id] == nil || d <= 0 {
		return
	}
	now := time.Now()
	// Keep the request as asked, within sane limits, so a later change of
	// floor can return to it. Clamping happens in reinterval.
	if d < time.Millisecond {
		d = time.Millisecond
	}
	if d > 24*time.Hour {
		d = 24 * time.Hour
	}
	h.requested = d
	h.lastInput = now
	h.changed(now)
}

// SetCamera moves the shared camera. view is what that camera sees.
func (h *Hub) SetCamera(id string, cam Camera, view *scope.View) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.clients[id]
	if c == nil || c.detached || c.replay || badCamera(cam) {
		return
	}
	now := time.Now()
	cam.By, cam.Seq = id, h.camera.Seq+1
	h.camera = cam
	if view != nil {
		h.view = view
	}
	h.cameraDirty = true
	h.event(now, "camera", cam)
	h.lastInput = now
	h.changed(now)
}

func badCamera(c Camera) bool {
	for _, f := range []float64{c.Lon, c.Lat, c.Height, c.Heading, c.Pitch} {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return true
		}
	}
	return c.Height <= 0 || c.Lat < -90 || c.Lat > 90
}

// SetView records what a detached client sees. It affects query scope only.
func (h *Hub) SetView(id string, view scope.View) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.clients[id]
	if c == nil || !c.detached {
		return
	}
	now := time.Now()
	c.view = &view
	h.lastInput = now
	h.changed(now)
}

// Select sets the selected aircraft, or clears it with "".
func (h *Hub) Select(id, icao24 string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clients[id] == nil || len(icao24) > 16 || icao24 == h.selected {
		return
	}
	now := time.Now()
	h.selected = icao24
	h.event(now, "selected", map[string]any{"icao24": icao24, "client": id})
	h.lastInput = now
	h.changed(now)
}

// SetRecording turns recording on or off.
func (h *Hub) SetRecording(id string, on bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clients[id] == nil || on == h.recording {
		return
	}
	now := time.Now()
	h.recording = true // so the event itself is always stored
	h.event(now, "recording", map[string]any{"value": onOff(on), "client": id})
	h.recording = on
	h.lastInput = now
	h.changed(now)
}

// SetCoverage switches between viewport and global coverage.
func (h *Hub) SetCoverage(id, coverage string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clients[id] == nil || (coverage != CoverageViewport && coverage != CoverageGlobal) || coverage == h.coverage {
		return
	}
	now := time.Now()
	h.coverage = coverage
	h.event(now, "coverage", map[string]any{"value": coverage, "client": id})
	h.lastInput = now
	h.changed(now)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// event records a shared-state change in the current session.
func (h *Hub) event(now time.Time, typ string, payload any) {
	if h.session != 0 && h.recording {
		h.rec.Event(h.session, now, typ, payload)
	}
}

// --- derived state ---

// changed re-derives everything after a state change, tells every client,
// and wakes the poller loop. Callers hold the lock.
func (h *Hub) changed(now time.Time) {
	h.evaluate(now)
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// evaluate applies the activity rules, session boundaries, scope, and
// interval. It reports whether anything a client can see changed.
func (h *Hub) evaluate(now time.Time) bool {
	before := h.fingerprint()

	h.active = 0
	for _, c := range h.clients {
		if h.isActive(c, now) {
			h.active++
		}
	}
	if h.active > 0 && !h.paused && h.cfg.IdleTimeout > 0 && now.Sub(h.lastInput) >= h.cfg.IdleTimeout {
		h.paused = true
		h.event(now, "pause", map[string]any{"reason": "idle"})
	}

	switch {
	case h.active > 0:
		h.zeroSince = time.Time{}
		if h.session == 0 {
			h.session = now.UnixMilli()
			h.rec.SessionStart(h.session, now)
			h.rec.Event(h.session, now, "state", h.state(now))
		}
	case h.session != 0:
		if h.zeroSince.IsZero() {
			h.zeroSince = now
		}
		if now.Sub(h.zeroSince) >= h.cfg.SessionGrace {
			h.saveCamera(now)
			h.rec.SessionEnd(h.session, h.zeroSince)
			h.session, h.zeroSince = 0, time.Time{}
			h.coverage = CoverageViewport // global is never sticky
			h.recording = true
		}
	}
	if h.cameraDirty && now.Sub(h.cameraSaved) > 10*time.Second {
		h.saveCamera(now)
	}

	h.replan()
	h.reinterval(now)
	return before != h.fingerprint()
}

func (h *Hub) saveCamera(now time.Time) {
	if h.cameraDirty {
		h.rec.SaveMeta("camera", SavedCamera{Camera: h.camera, View: h.view})
		h.cameraDirty, h.cameraSaved = false, now
	}
}

func (h *Hub) fingerprint() string {
	b, _ := json.Marshal([]any{h.active, h.paused, h.session, h.coverage, h.plan, h.interval, h.reason, len(h.clients)})
	return string(b)
}

// replan rebuilds the query plan from what clients are looking at.
func (h *Hub) replan() {
	var plan scope.Plan
	if h.coverage == CoverageGlobal {
		plan = scope.GlobalPlan()
	} else {
		var views []scope.View
		attached := false
		now := time.Now()
		for _, c := range h.clients {
			if !h.isActive(c, now) {
				continue
			}
			if !c.detached {
				attached = true
			} else if c.view != nil {
				views = append(views, *c.view)
			}
		}
		if attached || len(views) == 0 {
			views = append(views, h.sharedView())
		}
		prev := h.plan
		plan = scope.Build(views, &prev)
	}
	if !plan.Equal(h.plan) || plan.Cost != h.plan.Cost {
		h.plan = plan
		h.event(time.Now(), "scope", plan)
	}
}

// sharedView is what the shared camera sees. Before any client has reported
// a view, it is estimated from the camera height.
func (h *Hub) sharedView() scope.View {
	if h.view != nil {
		return *h.view
	}
	half := math.Min(40, math.Max(0.2, h.camera.Height/111000*0.55))
	return scope.View{
		West: wrapLon(h.camera.Lon - half), East: wrapLon(h.camera.Lon + half),
		South: math.Max(-90, h.camera.Lat-half), North: math.Min(90, h.camera.Lat+half),
	}
}

func wrapLon(l float64) float64 {
	for l > 180 {
		l -= 360
	}
	for l < -180 {
		l += 360
	}
	return l
}

func (h *Hub) cost() int {
	if h.plan.Cost < 1 {
		return 1
	}
	return h.plan.Cost
}

// reinterval works out the effective interval from, weakest first: the
// client request, the operator bounds, the credit reserve guard, and a 429.
func (h *Hub) reinterval(now time.Time) {
	eff, reason := h.cfg.Clamp(h.requested, h.cost())
	if g := h.guard(eff); g > eff {
		eff, reason = g, config.ReasonReserve
	}
	if h.limited {
		reason = config.ReasonLimited
	}
	if eff != h.interval || reason != h.reason {
		h.interval, h.reason = eff, reason
		h.event(now, "interval", map[string]any{
			"seconds": eff.Seconds(), "requested": h.requested.Seconds(), "reason": reason, "override": reason != "",
		})
	}
}

// guard stretches the interval once credits fall below the reserve. The
// interval grows as the balance shrinks, so the balance decays towards zero
// without reaching it.
func (h *Hub) guard(eff time.Duration) time.Duration {
	if h.limited || !h.hasRemaining || h.remaining >= h.cfg.CreditReserve {
		return eff
	}
	left := h.remaining
	if left < 1 {
		left = 1
	}
	g := time.Duration(float64(eff) * float64(h.cfg.CreditReserve) / float64(left))
	return g.Round(time.Second)
}

func forecast(cost int, interval time.Duration, remaining int, known bool) Forecast {
	f := Forecast{Cost: cost, Interval: interval.Seconds(), SecondsLeft: -1}
	if interval > 0 {
		f.Burn = float64(cost) * 3600 / interval.Seconds()
	}
	if known && f.Burn > 0 {
		f.SecondsLeft = float64(remaining) / f.Burn * 3600
	}
	return f
}

// State returns the full shared state.
func (h *Hub) State() State {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state(time.Now())
}

func (h *Hub) state(now time.Time) State {
	s := State{
		Interval:  IntervalState{Seconds: h.interval.Seconds(), Requested: h.requested.Seconds(), Reason: h.reason},
		Bounds:    BoundsState{Min: h.cfg.Floor(h.cost()).Seconds(), Max: h.cfg.Ceiling(h.cost()).Seconds()},
		Camera:    h.camera,
		Selected:  h.selected,
		Recording: onOff(h.recording),
		Coverage:  h.coverage,
		Scope:     h.plan,
		Status: StatusState{
			State: "stopped", Error: h.lastErr, Active: h.active, Connected: len(h.clients),
			Storage: h.rec.Health(), Session: h.session,
		},
	}
	if h.active > 0 {
		s.Status.State = "polling"
		if h.paused {
			s.Status.State = "paused"
		}
	}
	if h.lastErr != "" {
		s.Status.ErrorAt = h.lastErrAt.UnixMilli()
	}
	if now.Before(h.retryUntil) {
		s.Status.LimitedUntil = h.retryUntil.UnixMilli()
	}
	if h.snap != nil {
		s.Snapshot = SnapshotState{Version: h.snap.Version, Time: h.snap.Time, Fetched: h.snap.Fetched,
			Count: len(h.snap.Aircraft), Scope: h.snap.Scope}
	}
	s.Credits = CreditState{
		Known: h.hasRemaining, Remaining: h.remaining,
		Forecast: forecast(h.cost(), h.interval, h.remaining, h.hasRemaining),
	}
	if s.Credits.SecondsLeft >= 0 && s.Status.State == "polling" {
		s.Credits.ExhaustAt = now.Add(time.Duration(s.Credits.SecondsLeft * float64(time.Second))).UnixMilli()
	}
	gi, _ := h.cfg.Clamp(h.requested, scope.MaxCost)
	if g := h.guard(gi); g > gi {
		gi = g
	}
	s.Credits.Global = forecast(scope.MaxCost, gi, h.remaining, h.hasRemaining)
	return s
}

// Snapshot returns the latest snapshot as JSON and as gzip, with its
// version. ok is false before the first poll.
func (h *Hub) Snapshot() (version int64, plain, gz []byte, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.snap == nil {
		return 0, nil, nil, false
	}
	return h.version, h.snapJSON, h.snapGzip, true
}

// --- the poller ---

func (h *Hub) polling() bool { return h.active > 0 && !h.paused }

// nextPoll is when the next poll is due. Callers hold the lock.
func (h *Hub) nextPoll() time.Time {
	delay := h.interval
	if h.failures > 0 {
		// Exponential backoff after failures.
		shift := h.failures
		if shift > 10 {
			shift = 10
		}
		if delay = h.interval << shift; delay > maxBackoff || delay <= 0 {
			delay = maxBackoff
		}
		if delay < h.interval {
			delay = h.interval
		}
	}
	if h.limited {
		// The 429 said when to come back. Try then, not sooner or later.
		return h.retryUntil
	}
	return h.lastAttempt.Add(delay)
}

// nextDeadline is the next moment the loop must look again without being
// woken. Callers hold the lock.
func (h *Hub) nextDeadline(now time.Time) time.Time {
	next := now.Add(time.Hour)
	consider := func(t time.Time) {
		if t.Before(next) {
			next = t
		}
	}
	if h.polling() {
		consider(h.nextPoll())
		if h.cfg.IdleTimeout > 0 {
			consider(h.lastInput.Add(h.cfg.IdleTimeout))
		}
	}
	for _, c := range h.clients {
		if c.conns > 0 && !c.visible && !c.replay && now.Sub(c.hiddenSince) < h.cfg.HiddenTimeout {
			consider(c.hiddenSince.Add(h.cfg.HiddenTimeout))
		}
	}
	if h.session != 0 && !h.zeroSince.IsZero() {
		consider(h.zeroSince.Add(h.cfg.SessionGrace))
	}
	if h.cameraDirty {
		consider(h.cameraSaved.Add(10*time.Second + time.Millisecond))
	}
	if now.Before(h.retryUntil) {
		consider(h.retryUntil)
	}
	return next
}

// Run is the single poller loop. It returns when ctx is done.
func (h *Hub) Run(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		h.mu.Lock()
		now := time.Now()
		if h.evaluate(now) {
			h.changed(now)
		}
		due := h.polling() && !now.Before(h.nextPoll())
		wait := h.nextDeadline(now).Sub(now)
		h.mu.Unlock()

		if due {
			h.poll(ctx)
			continue
		}
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			h.mu.Lock()
			h.saveCamera(time.Now())
			h.mu.Unlock()
			return
		case <-h.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
	}
}

// poll makes one poll: one request for each box in the plan.
func (h *Hub) poll(ctx context.Context) {
	h.mu.Lock()
	plan := h.plan
	h.lastAttempt = time.Now()
	h.mu.Unlock()

	var boxes []*scope.Box
	if plan.Global || len(plan.Boxes) == 0 {
		boxes = []*scope.Box{nil}
	} else {
		for i := range plan.Boxes {
			boxes = append(boxes, &plan.Boxes[i])
		}
	}

	merged := map[string]model.Aircraft{}
	var order []string
	var dataTime int64
	remaining, hasRemaining := 0, false
	var err error
	for _, box := range boxes {
		// Nothing new starts once nobody is watching.
		h.mu.Lock()
		h.evaluate(time.Now())
		ok := h.polling()
		h.mu.Unlock()
		if !ok {
			return
		}
		var res *opensky.Result
		if res, err = h.up.States(ctx, box); err != nil {
			break
		}
		if res.Time > dataTime {
			dataTime = res.Time
		}
		if res.HasRemaining {
			remaining, hasRemaining = res.Remaining, true
		}
		for _, a := range res.Aircraft {
			old, seen := merged[a.ICAO24]
			if !seen {
				order = append(order, a.ICAO24)
			}
			if !seen || a.LastContact >= old.LastContact {
				merged[a.ICAO24] = a
			}
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		h.failures++
		h.lastErr, h.lastErrAt = err.Error(), now
		var rl *opensky.RateLimitError
		if errors.As(err, &rl) {
			h.retryUntil, h.limited = now.Add(rl.RetryAfter), true
			h.remaining, h.hasRemaining = 0, true
			h.failures = 0 // retry-after governs, not backoff
		}
		h.log.Warn("upstream poll failed", "error", h.lastErr, "failures", h.failures)
		h.changed(now)
		return
	}

	h.failures, h.lastErr, h.limited = 0, "", false
	if hasRemaining {
		h.remaining, h.hasRemaining = remaining, true
	}
	h.version++
	snap := &model.Snapshot{Version: h.version, Time: dataTime, Fetched: now.UnixMilli(), Scope: plan,
		Fields: model.Fields, Aircraft: make([]model.Aircraft, 0, len(order))}
	for _, id := range order {
		snap.Aircraft = append(snap.Aircraft, merged[id])
	}
	h.snap = snap
	h.snapJSON, _ = json.Marshal(snap)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(h.snapJSON)
	zw.Close()
	h.snapGzip = buf.Bytes()
	if h.session != 0 && h.recording {
		h.rec.Snapshot(h.session, snap)
	}
	h.log.Info("upstream poll", "version", h.version, "aircraft", len(snap.Aircraft), "cost", plan.Cost,
		"requests", len(boxes), "remaining", strconv.Itoa(h.remaining))
	h.changed(now)
}

// NopRecorder discards everything. It is used when storage is unavailable.
type NopRecorder struct{ Msg string }

func (NopRecorder) SessionStart(int64, time.Time)       {}
func (NopRecorder) SessionEnd(int64, time.Time)         {}
func (NopRecorder) Snapshot(int64, *model.Snapshot)     {}
func (NopRecorder) Event(int64, time.Time, string, any) {}
func (NopRecorder) SaveMeta(string, any)                {}
func (n NopRecorder) Health() string                    { return n.Msg }
