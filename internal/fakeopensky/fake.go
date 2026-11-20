// Package fakeopensky is a stand-in for the OpenSky API and its token
// endpoint. Tests use it to count every upstream request, and the development
// server uses it to replay captured data at no credit cost.
package fakeopensky

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goodtune/airspace/internal/scope"
)

// Source returns the data time and state vectors for a query. box is nil for
// a global query.
type Source func(box *scope.Box, now time.Time) (int64, [][]any)

// Fake is the fake upstream. Set fields before serving; use the methods to
// change behaviour while it runs.
type Fake struct {
	mu            sync.Mutex
	tokenRequests int
	stateRequests int
	queries       []string
	remaining     int
	tokenTTL      time.Duration
	tokens        map[string]time.Time
	failing       bool
	retryAfter    int
	nextToken     int
	source        Source
}

// New returns a fake with a full daily allowance and synthetic traffic.
func New() *Fake {
	return &Fake{remaining: 4000, tokenTTL: 30 * time.Minute, tokens: map[string]time.Time{}, retryAfter: 60, source: Synthetic}
}

func (f *Fake) with(fn func()) { f.mu.Lock(); defer f.mu.Unlock(); fn() }

// SetRemaining sets the credit balance.
func (f *Fake) SetRemaining(n int) { f.with(func() { f.remaining = n }) }

// SetTokenTTL sets the lifetime of tokens issued from now on.
func (f *Fake) SetTokenTTL(d time.Duration) { f.with(func() { f.tokenTTL = d }) }

// SetFailing makes every state request return HTTP 503.
func (f *Fake) SetFailing(b bool) { f.with(func() { f.failing = b }) }

// SetRetryAfter sets the seconds sent with a 429.
func (f *Fake) SetRetryAfter(s int) { f.with(func() { f.retryAfter = s }) }

// SetSource replaces the data source.
func (f *Fake) SetSource(s Source) { f.with(func() { f.source = s }) }

// ExpireTokens invalidates every issued token, so the next call gets a 401.
func (f *Fake) ExpireTokens() { f.with(func() { f.tokens = map[string]time.Time{} }) }

// TokenRequests is the number of token requests received.
func (f *Fake) TokenRequests() int { f.mu.Lock(); defer f.mu.Unlock(); return f.tokenRequests }

// StateRequests is the number of state requests received, including rejected ones.
func (f *Fake) StateRequests() int { f.mu.Lock(); defer f.mu.Unlock(); return f.stateRequests }

// Requests is the total number of requests of any kind.
func (f *Fake) Requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenRequests + f.stateRequests
}

// Remaining is the credit balance.
func (f *Fake) Remaining() int { f.mu.Lock(); defer f.mu.Unlock(); return f.remaining }

// Queries lists the raw query string of every state request, "" for global.
func (f *Fake) Queries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
}

// Handler serves POST /token and GET /api/states/all.
func (f *Fake) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", f.token)
	mux.HandleFunc("/api/states/all", f.states)
	return mux
}

func (f *Fake) token(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenRequests++
	if r.Method != http.MethodPost || r.FormValue("grant_type") != "client_credentials" ||
		r.FormValue("client_id") == "" || r.FormValue("client_secret") == "" || r.FormValue("client_secret") == "wrong" {
		http.Error(w, "bad credentials", http.StatusUnauthorized)
		return
	}
	f.nextToken++
	tok := fmt.Sprintf("fake-token-%d", f.nextToken)
	f.tokens[tok] = time.Now().Add(f.tokenTTL)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "expires_in": int(math.Round(f.tokenTTL.Seconds()))})
}

func (f *Fake) states(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.stateRequests++
	f.queries = append(f.queries, r.URL.RawQuery)
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	exp, ok := f.tokens[tok]
	if !ok || time.Now().After(exp) {
		f.mu.Unlock()
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if f.failing {
		f.mu.Unlock()
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	var box *scope.Box
	cost := scope.MaxCost
	if q := r.URL.Query(); q.Get("lamin") != "" {
		box = &scope.Box{}
		for k, p := range map[string]*float64{"lamin": &box.LaMin, "lomin": &box.LoMin, "lamax": &box.LaMax, "lomax": &box.LoMax} {
			v, err := strconv.ParseFloat(q.Get(k), 64)
			if err != nil {
				f.mu.Unlock()
				http.Error(w, "bad box", http.StatusBadRequest)
				return
			}
			*p = v
		}
		cost = scope.Cost(box.Area())
	}
	if f.remaining < cost {
		w.Header().Set("X-Rate-Limit-Retry-After-Seconds", strconv.Itoa(f.retryAfter))
		f.mu.Unlock()
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	f.remaining -= cost
	remaining, src := f.remaining, f.source
	f.mu.Unlock()

	t, states := src(box, time.Now())
	w.Header().Set("X-Rate-Limit-Remaining", strconv.Itoa(remaining))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"time": t, "states": states})
}

// Synthetic is a small, deterministic set of moving aircraft spread over the
// globe, including one with no position.
func Synthetic(box *scope.Box, now time.Time) (int64, [][]any) {
	t := now.Unix()
	var out [][]any
	for i := 0; i < 400; i++ {
		lat := -60 + float64(i%20)*6.3
		lon := math.Mod(-180+float64(i)*7.3+float64(t%3600)*0.002, 360)
		if lon > 180 {
			lon -= 360
		}
		if box != nil && (lat < box.LaMin || lat > box.LaMax || lon < box.LoMin || lon > box.LoMax) {
			continue
		}
		out = append(out, []any{fmt.Sprintf("%06x", 0xa00000+i), fmt.Sprintf("SYN%d  ", i), "Testland", t - 1, t,
			lon, lat, 9000.0 + float64(i), false, 230.0, 90.0, 0.0, nil, 9100.0 + float64(i), "1000", false, 0})
	}
	out = append(out, []any{"ffffff", nil, "Nowhere", nil, t, nil, nil, nil, false, nil, nil, nil, nil, nil, nil, false, 0})
	return t, out
}

// Capture replays a file written by cmd/capture. Frames loop, and all times
// are shifted so the data looks current.
type Capture struct {
	frames [][]byte // raw JSON bodies
	times  []int64
	start  time.Time
	step   time.Duration
}

// LoadCapture reads a gzip JSONL capture file.
func LoadCapture(path string) (*Capture, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	zr, err := gzip.NewReader(fh)
	if err != nil {
		return nil, err
	}
	c := &Capture{start: time.Now()}
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 1<<20), 256<<20)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		var head struct {
			Time int64 `json:"time"`
		}
		if json.Unmarshal(line, &head) != nil || head.Time == 0 {
			continue
		}
		c.frames = append(c.frames, line)
		c.times = append(c.times, head.Time)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(c.frames) < 2 {
		return nil, fmt.Errorf("%s holds %d usable frames, need at least 2", path, len(c.frames))
	}
	span := time.Duration(c.times[len(c.times)-1]-c.times[0]) * time.Second
	c.step = span / time.Duration(len(c.frames)-1)
	if c.step < time.Second {
		c.step = time.Second
	}
	return c, nil
}

// Frames is the number of frames loaded.
func (c *Capture) Frames() int { return len(c.frames) }

// Source serves the frame for the current wall-clock time.
func (c *Capture) Source(box *scope.Box, now time.Time) (int64, [][]any) {
	i := int(now.Sub(c.start)/c.step) % len(c.frames)
	var resp struct {
		Time   int64   `json:"time"`
		States [][]any `json:"states"`
	}
	if json.Unmarshal(c.frames[i], &resp) != nil {
		return now.Unix(), nil
	}
	// Shift so this frame's time is the start of the current step.
	frameNow := c.start.Add(time.Duration(int(now.Sub(c.start)/c.step)) * c.step).Unix()
	delta := float64(frameNow - resp.Time)
	out := resp.States[:0]
	for _, s := range resp.States {
		if len(s) < 17 {
			continue
		}
		if box != nil {
			lon, ok1 := s[5].(float64)
			lat, ok2 := s[6].(float64)
			if !ok1 || !ok2 || lat < box.LaMin || lat > box.LaMax || lon < box.LoMin || lon > box.LoMax {
				continue
			}
		}
		for _, k := range []int{3, 4} {
			if v, ok := s[k].(float64); ok {
				s[k] = v + delta
			}
		}
		out = append(out, s)
	}
	return frameNow, out
}
