// Package server is the HTTP layer: the JSON API, the Datastar stream and
// actions, and the embedded frontend. No handler here causes an upstream
// request.
package server

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"github.com/goodtune/airspace/internal/hub"
	"github.com/goodtune/airspace/internal/model"
	"github.com/goodtune/airspace/internal/scope"
	"github.com/goodtune/airspace/internal/store"
)

// Sessions is the read side of storage. It is nil when storage is down.
type Sessions interface {
	Sessions(ctx context.Context) ([]store.Session, error)
	Session(ctx context.Context, id int64) (store.Session, error)
	Index(ctx context.Context, id int64) ([]store.SnapshotInfo, error)
	Rename(ctx context.Context, id int64, name string) error
	Delete(ctx context.Context, id int64) error
	Snapshots(ctx context.Context, id, from, to int64, limit int, fn func(store.SnapshotRow) error) (bool, error)
	Events(ctx context.Context, id, from, to, after int64, limit int, types []string, exclude bool) ([]store.Event, bool, error)
	Size() (int64, error)
}

// Server holds the handlers.
type Server struct {
	Hub      *hub.Hub
	Sessions Sessions
	Static   fs.FS
	Log      *slog.Logger
	// Heartbeat is how often an idle stream resends state. Zero means 15 s.
	Heartbeat time.Duration

	started string
}

// Handler builds the route table.
func (s *Server) Handler() http.Handler {
	s.started = strconv.FormatInt(time.Now().UnixNano(), 36)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /api/states", s.states)
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/stream", s.stream)
	mux.HandleFunc("POST /api/action/{name}", s.action)
	mux.HandleFunc("GET /api/sessions", s.sessionList)
	mux.HandleFunc("GET /api/sessions/{id}", s.sessionDetail)
	mux.HandleFunc("PATCH /api/sessions/{id}", s.sessionRename)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.sessionDelete)
	mux.HandleFunc("GET /api/sessions/{id}/snapshots", s.sessionSnapshots)
	mux.HandleFunc("GET /api/sessions/{id}/events", s.sessionEvents)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { fail(w, http.StatusNotFound, "no such endpoint") })
	mux.HandleFunc("/", s.static)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func acceptsGzip(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
}

// --- live ---

var emptySnapshot, _ = json.Marshal(model.Snapshot{Fields: model.Fields, Aircraft: []model.Aircraft{}})

func (s *Server) states(w http.ResponseWriter, r *http.Request) {
	version, plain, gz, ok := s.Hub.Snapshot()
	if !ok {
		plain, gz = emptySnapshot, nil
	}
	etag := `"v` + strconv.FormatInt(version, 10) + `"`
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Cache-Control", "no-cache")
	h.Set("Vary", "Accept-Encoding")
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "application/json")
	if gz != nil && acceptsGzip(r) {
		h.Set("Content-Encoding", "gzip")
		h.Set("Content-Length", strconv.Itoa(len(gz)))
		w.Write(gz)
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(plain)))
	w.Write(plain)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	out := struct {
		hub.State
		ServerTime int64 `json:"serverTime"`
		DBBytes    int64 `json:"dbBytes"`
	}{State: s.Hub.State(), ServerTime: time.Now().UnixMilli()}
	if s.Sessions != nil {
		out.DBBytes, _ = s.Sessions.Size()
	}
	writeJSON(w, http.StatusOK, out)
}

// stream is the one long-lived Datastar connection each client holds. It
// sends the full state on connect and again after every change.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	var sig struct {
		ClientID string `json:"clientId"`
	}
	datastar.ReadSignals(r, &sig) // a first connection has no signals yet
	id, notify, release := s.Hub.Connect(sig.ClientID)
	defer release()

	sse := datastar.NewSSE(w, r)
	beat := s.Heartbeat
	if beat <= 0 {
		beat = 15 * time.Second
	}
	heartbeat := time.NewTicker(beat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-notify:
		case <-heartbeat.C:
		}
		payload := struct {
			ClientID   string `json:"clientId"`
			ServerTime int64  `json:"serverTime"` // unix ms, for clock offset
			hub.State
		}{id, time.Now().UnixMilli(), s.Hub.State()}
		if err := sse.MarshalAndPatchSignals(payload); err != nil {
			return
		}
		// Coalesce bursts, such as camera moves, to 20 patches a second.
		select {
		case <-r.Context().Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

type actionBody struct {
	ClientID string      `json:"clientId"`
	Seconds  float64     `json:"seconds"`
	Camera   *hub.Camera `json:"camera"`
	View     *scope.View `json:"view"`
	ICAO24   string      `json:"icao24"`
	Value    string      `json:"value"`
	Visible  bool        `json:"visible"`
	Replay   bool        `json:"replay"`
	Detached bool        `json:"detached"`
	Seq      *int64      `json:"seq"`
}

// action applies a change to shared state from one client.
func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	var b actionBody
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.ClientID == "" {
		fail(w, http.StatusBadRequest, "body must be JSON with a clientId")
		return
	}
	switch r.PathValue("name") {
	case "interval":
		if b.Seconds <= 0 {
			fail(w, http.StatusBadRequest, "seconds must be positive")
			return
		}
		s.Hub.SetInterval(b.ClientID, time.Duration(b.Seconds*float64(time.Second)))
	case "camera":
		if b.Camera == nil {
			fail(w, http.StatusBadRequest, "camera is required")
			return
		}
		s.Hub.SetCamera(b.ClientID, *b.Camera, b.View)
	case "view":
		if b.View == nil {
			fail(w, http.StatusBadRequest, "view is required")
			return
		}
		if b.Seq != nil {
			// An attached client saying what the shared camera shows.
			s.Hub.SetSharedView(b.ClientID, *b.Seq, *b.View)
		} else {
			s.Hub.SetView(b.ClientID, *b.View)
		}
	case "select":
		s.Hub.Select(b.ClientID, b.ICAO24)
	case "recording":
		if b.Value != "on" && b.Value != "off" {
			fail(w, http.StatusBadRequest, "value must be on or off")
			return
		}
		s.Hub.SetRecording(b.ClientID, b.Value == "on")
	case "coverage":
		if b.Value != hub.CoverageViewport && b.Value != hub.CoverageGlobal {
			fail(w, http.StatusBadRequest, "value must be viewport or global")
			return
		}
		s.Hub.SetCoverage(b.ClientID, b.Value)
	case "presence":
		s.Hub.Presence(b.ClientID, b.Visible, b.Replay, b.Detached)
	case "input":
		s.Hub.Input(b.ClientID)
	default:
		fail(w, http.StatusNotFound, "no such action")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- recorded sessions ---

func (s *Server) sessionID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if s.Sessions == nil {
		fail(w, http.StatusServiceUnavailable, "storage is unavailable")
		return 0, false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad session id")
		return 0, false
	}
	return id, true
}

func storeFail(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "session not found")
		return
	}
	fail(w, http.StatusInternalServerError, err.Error())
}

func (s *Server) sessionList(w http.ResponseWriter, r *http.Request) {
	if s.Sessions == nil {
		fail(w, http.StatusServiceUnavailable, "storage is unavailable")
		return
	}
	list, err := s.Sessions.Sessions(r.Context())
	if err != nil {
		storeFail(w, err)
		return
	}
	size, _ := s.Sessions.Size()
	writeJSON(w, http.StatusOK, map[string]any{"sessions": list, "dbBytes": size, "recording": s.Hub.State().Status.Session})
}

func (s *Server) sessionDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionID(w, r)
	if !ok {
		return
	}
	sess, err := s.Sessions.Session(r.Context(), id)
	if err != nil {
		storeFail(w, err)
		return
	}
	index, err := s.Sessions.Index(r.Context(), id)
	if err != nil {
		storeFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": sess, "index": index})
}

func (s *Server) sessionRename(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionID(w, r)
	if !ok {
		return
	}
	var b struct {
		Name string `json:"name"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || len(b.Name) > 200 {
		fail(w, http.StatusBadRequest, "body must be JSON with a name of at most 200 characters")
		return
	}
	if err := s.Sessions.Rename(r.Context(), id, strings.TrimSpace(b.Name)); err != nil {
		storeFail(w, err)
		return
	}
	sess, err := s.Sessions.Session(r.Context(), id)
	if err != nil {
		storeFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) sessionDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionID(w, r)
	if !ok {
		return
	}
	if err := s.Sessions.Delete(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			storeFail(w, err)
		} else {
			fail(w, http.StatusConflict, err.Error())
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// timeRange reads from, to, and limit. Times are unix milliseconds.
func timeRange(r *http.Request, defLimit, maxLimit int) (from, to int64, limit int, err error) {
	q := r.URL.Query()
	from, to, limit = 0, 1<<62, defLimit
	if v := q.Get("from"); v != "" {
		if from, err = strconv.ParseInt(v, 10, 64); err != nil {
			return 0, 0, 0, errors.New("from must be a time in unix milliseconds")
		}
	}
	if v := q.Get("to"); v != "" {
		if to, err = strconv.ParseInt(v, 10, 64); err != nil {
			return 0, 0, 0, errors.New("to must be a time in unix milliseconds")
		}
	}
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 {
			return 0, 0, 0, errors.New("limit must be a positive whole number")
		}
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	return from, to, limit, nil
}

// sessionSnapshots streams one page of snapshots. The response is
// {"snapshots":[...],"more":bool,"next":time}; when more is true, ask again
// with from=next. Each stored blob is decompressed straight into the
// response, so memory use does not grow with the page.
func (s *Server) sessionSnapshots(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionID(w, r)
	if !ok {
		return
	}
	from, to, limit, err := timeRange(r, 8, 40)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.Sessions.Session(r.Context(), id); err != nil {
		storeFail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	var out io.Writer = w
	if acceptsGzip(r) {
		w.Header().Set("Content-Encoding", "gzip")
		zw, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
		defer zw.Close()
		out = zw
	}
	io.WriteString(out, `{"snapshots":[`)
	n, last := 0, int64(0)
	more, err := s.Sessions.Snapshots(r.Context(), id, from, to, limit, func(row store.SnapshotRow) error {
		zr, err := gzip.NewReader(strings.NewReader(string(row.Data)))
		if err != nil {
			return err
		}
		if n > 0 {
			io.WriteString(out, ",")
		}
		n++
		last = row.Time
		_, err = io.Copy(out, zr)
		return err
	})
	if err != nil {
		s.Log.Error("streaming snapshots", "session", id, "error", err)
		// The body is already under way; end it so the client sees bad JSON.
		return
	}
	fmt.Fprintf(out, `],"more":%t,"next":%d}`, more, last+1)
}

func (s *Server) sessionEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionID(w, r)
	if !ok {
		return
	}
	from, to, limit, err := timeRange(r, 1000, 5000)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if _, err := s.Sessions.Session(r.Context(), id); err != nil {
		storeFail(w, err)
		return
	}
	// types=a,b keeps only those event types; exclude=a,b drops them.
	var types []string
	exclude := false
	if v := r.URL.Query().Get("types"); v != "" {
		types = strings.Split(v, ",")
	} else if v := r.URL.Query().Get("exclude"); v != "" {
		types, exclude = strings.Split(v, ","), true
	}
	events, more, err := s.Sessions.Events(r.Context(), id, from, to, after, limit, types, exclude)
	if err != nil {
		storeFail(w, err)
		return
	}
	next := after
	if len(events) > 0 {
		next = events[len(events)-1].ID
	}
	if acceptsGzip(r) {
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		defer zw.Close()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(zw).Encode(map[string]any{"events": events, "more": more, "after": next})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "more": more, "after": next})
}

// --- embedded frontend ---

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	f, err := s.Static.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	ctype := mime.TypeByExtension(path.Ext(name))
	switch path.Ext(name) {
	case ".js", ".mjs":
		ctype = "text/javascript; charset=utf-8"
	case ".wasm":
		ctype = "application/wasm"
	case ".json", ".geojson", ".map":
		ctype = "application/json"
	case "":
		ctype = "application/octet-stream"
	}
	if ctype != "" {
		h.Set("Content-Type", ctype)
	}
	// Files change only when the binary does, so the start time is the tag.
	etag := `"` + s.started + `"`
	h.Set("ETag", etag)
	if strings.HasPrefix(name, "vendor/") {
		h.Set("Cache-Control", "public, max-age=86400")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	if strings.Contains(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		io.Copy(w, f)
		return
	}
	http.ServeContent(w, r, "", time.Time{}, rs)
}
