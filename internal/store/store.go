// Package store records sessions in SQLite. Writes are queued so they never
// block the poller, and a failure degrades to live-only instead of stopping
// the app.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/goodtune/airspace/internal/model"
)

// migrations run in order at startup. PRAGMA user_version counts how many
// have been applied. Never edit an entry; add a new one.
var migrations = []string{
	`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
	 CREATE TABLE sessions (
		id               INTEGER PRIMARY KEY,
		start            INTEGER NOT NULL,           -- unix ms
		end              INTEGER,                    -- unix ms, NULL while open
		snapshots        INTEGER NOT NULL DEFAULT 0,
		global_snapshots INTEGER NOT NULL DEFAULT 0,
		bytes            INTEGER NOT NULL DEFAULT 0,
		name             TEXT
	 );
	 CREATE TABLE snapshots (
		session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
		time       INTEGER NOT NULL,                 -- when fetched, unix ms
		data_time  INTEGER NOT NULL,                 -- OpenSky time, unix s
		scope      TEXT NOT NULL,                    -- JSON: boxes or global
		aircraft   INTEGER NOT NULL,
		data       BLOB NOT NULL                     -- gzip JSON snapshot
	 );
	 CREATE INDEX snapshots_session_time ON snapshots(session_id, time);
	 CREATE TABLE events (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
		time       INTEGER NOT NULL,                 -- unix ms
		type       TEXT NOT NULL,
		payload    TEXT NOT NULL                     -- JSON
	 );
	 CREATE INDEX events_session_time ON events(session_id, time, id);`,
}

// Session is one recorded session.
type Session struct {
	ID        int64  `json:"id"`
	Start     int64  `json:"start"`
	End       int64  `json:"end"` // 0 while open
	Snapshots int    `json:"snapshots"`
	Bytes     int64  `json:"bytes"`
	Name      string `json:"name"`
	Coverage  string `json:"coverage"` // viewport, global, or mixed
}

// SnapshotInfo describes a stored snapshot without its data.
type SnapshotInfo struct {
	Time     int64           `json:"time"`
	Aircraft int             `json:"aircraft"`
	Scope    json.RawMessage `json:"scope"`
}

// SnapshotRow is a stored snapshot with its gzip JSON blob.
type SnapshotRow struct {
	Time int64
	Data []byte
}

// Event is one entry in a session's event track.
type Event struct {
	ID      int64           `json:"id"`
	Time    int64           `json:"time"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// ErrNotFound means there is no such session.
var ErrNotFound = errors.New("session not found")

// Options control retention.
type Options struct {
	MaxSize       int64 // bytes, 0 for no limit
	RetentionDays int   // 0 for no limit
}

// Store is the SQLite recorder.
type Store struct {
	w, r *sql.DB
	log  *slog.Logger
	opt  Options

	queue chan func(*sql.DB) error
	done  chan struct{}

	// Observe, when set, feeds metrics: "snapshot" with its size in bytes,
	// "error" for a failed write, "dropped" for a write lost to a full
	// queue, and "pruned" with the number of sessions deleted.
	Observe func(event string, n int)

	mu      sync.Mutex
	health  string
	current int64
	writes  int
}

// Open opens or creates the database and applies migrations.
func Open(path string, opt Options, log *slog.Logger) (*Store, error) {
	dsn := "file:" + url.PathEscape(path) + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	// auto_vacuum must be set before the first table exists.
	if _, err := w.Exec(`PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
		w.Close()
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	if err := migrate(w); err != nil {
		w.Close()
		return nil, fmt.Errorf("migrating %s: %w", path, err)
	}
	r, err := sql.Open("sqlite", dsn+"&mode=ro")
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)
	s := &Store{w: w, r: r, log: log, opt: opt, queue: make(chan func(*sql.DB) error, 2048), done: make(chan struct{})}
	// Close any session left open by a crash.
	if _, err := w.Exec(`UPDATE sessions SET end = COALESCE(
		(SELECT MAX(time) FROM snapshots WHERE session_id = sessions.id), start) WHERE end IS NULL`); err != nil {
		s.Close()
		return nil, err
	}
	go s.writer()
	s.enqueue(func(db *sql.DB) error { return s.prune(db) })
	return s, nil
}

func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d)", v, len(migrations))
	}
	for ; v < len(migrations); v++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", v+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Close drains the write queue and closes the database.
func (s *Store) Close() error {
	close(s.queue)
	<-s.done
	s.r.Close()
	return s.w.Close()
}

// Flush waits until everything queued so far is written.
func (s *Store) Flush() {
	ch := make(chan struct{})
	s.queue <- func(*sql.DB) error { close(ch); return nil }
	<-ch
}

func (s *Store) writer() {
	defer close(s.done)
	for op := range s.queue {
		err := op(s.w)
		if err != nil {
			s.observe("error", 1)
		}
		s.mu.Lock()
		if err != nil {
			if s.health == "" {
				s.log.Error("recording failed; live view continues", "error", err)
			}
			s.health = "recording failed: " + err.Error()
		} else if s.health != "" && s.health != queueFull {
			s.log.Info("recording recovered")
			s.health = ""
		}
		s.mu.Unlock()
	}
}

const queueFull = "recording is falling behind; some data was not stored"

// enqueue never blocks. When the queue is full the write is dropped.
func (s *Store) enqueue(op func(*sql.DB) error) {
	select {
	case s.queue <- op:
	default:
		s.observe("dropped", 1)
		s.mu.Lock()
		s.health = queueFull
		s.mu.Unlock()
	}
}

func (s *Store) observe(event string, n int) {
	if s.Observe != nil {
		s.Observe(event, n)
	}
}

// Health is empty when recording works.
func (s *Store) Health() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.health
}

// --- hub.Recorder ---

func (s *Store) SessionStart(id int64, start time.Time) {
	s.mu.Lock()
	s.current = id
	s.mu.Unlock()
	s.enqueue(func(db *sql.DB) error {
		_, err := db.Exec(`INSERT OR IGNORE INTO sessions (id, start) VALUES (?, ?)`, id, start.UnixMilli())
		return err
	})
}

func (s *Store) SessionEnd(id int64, end time.Time) {
	s.mu.Lock()
	if s.current == id {
		s.current = 0
	}
	s.mu.Unlock()
	s.enqueue(func(db *sql.DB) error {
		if _, err := db.Exec(`UPDATE sessions SET end = ? WHERE id = ?`, end.UnixMilli(), id); err != nil {
			return err
		}
		// A session that stored nothing is not worth listing.
		if _, err := db.Exec(`DELETE FROM sessions WHERE id = ? AND snapshots = 0`, id); err != nil {
			return err
		}
		return s.prune(db)
	})
}

func (s *Store) Snapshot(session int64, snap *model.Snapshot) {
	s.enqueue(func(db *sql.DB) error {
		blob, err := snap.Encode()
		if err != nil {
			return err
		}
		scopeJSON, _ := json.Marshal(snap.Scope)
		global := 0
		if snap.Scope.Global {
			global = 1
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`INSERT INTO snapshots (session_id, time, data_time, scope, aircraft, data) VALUES (?, ?, ?, ?, ?, ?)`,
			session, snap.Fetched, snap.Time, string(scopeJSON), len(snap.Aircraft), blob); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE sessions SET snapshots = snapshots + 1, global_snapshots = global_snapshots + ?,
			bytes = bytes + ? WHERE id = ?`, global, len(blob), session); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		s.observe("snapshot", len(blob))
		s.writes++
		if s.writes%200 == 0 {
			return s.prune(db)
		}
		return nil
	})
}

func (s *Store) Event(session int64, t time.Time, typ string, payload any) {
	p, err := json.Marshal(payload)
	if err != nil {
		p = []byte("null")
	}
	s.enqueue(func(db *sql.DB) error {
		_, err := db.Exec(`INSERT INTO events (session_id, time, type, payload) VALUES (?, ?, ?, ?)`,
			session, t.UnixMilli(), typ, string(p))
		return err
	})
}

func (s *Store) SaveMeta(key string, v any) {
	p, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.enqueue(func(db *sql.DB) error {
		_, err := db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, string(p))
		return err
	})
}

// LoadMeta reads a value saved by SaveMeta. ok is false when there is none.
func (s *Store) LoadMeta(key string, into any) (ok bool, err error) {
	var v string
	switch err := s.r.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, json.Unmarshal([]byte(v), into)
}

// --- retention ---

// Size is the database size in bytes, not counting free pages.
func size(db *sql.DB) (int64, error) {
	var pages, free, pageSize int64
	if err := db.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, err
	}
	if err := db.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil {
		return 0, err
	}
	if err := db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, err
	}
	return (pages - free) * pageSize, nil
}

// Size is the database size in bytes, not counting free pages.
func (s *Store) Size() (int64, error) { return size(s.r) }

// prune deletes the oldest closed sessions until the limits are met.
func (s *Store) prune(db *sql.DB) error {
	deleted := 0
	if s.opt.RetentionDays > 0 {
		cutoff := time.Now().Add(-time.Duration(s.opt.RetentionDays) * 24 * time.Hour).UnixMilli()
		res, err := db.Exec(`DELETE FROM sessions WHERE end IS NOT NULL AND end < ?`, cutoff)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		deleted += int(n)
	}
	for s.opt.MaxSize > 0 {
		sz, err := size(db)
		if err != nil {
			return err
		}
		if sz <= s.opt.MaxSize {
			break
		}
		var id int64
		err = db.QueryRow(`SELECT id FROM sessions WHERE end IS NOT NULL ORDER BY start LIMIT 1`).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			break // only the open session is left
		} else if err != nil {
			return err
		}
		if _, err := db.Exec(`DELETE FROM sessions WHERE id = ?`, id); err != nil {
			return err
		}
		deleted++
	}
	if deleted > 0 {
		s.observe("pruned", deleted)
		s.log.Info("pruned sessions", "count", deleted)
		return reclaim(db)
	}
	return nil
}

func reclaim(db *sql.DB) error {
	if _, err := db.Exec(`PRAGMA incremental_vacuum`); err != nil {
		return err
	}
	_, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// --- reads ---

const sessionCols = `id, start, COALESCE(end, 0), snapshots, bytes, COALESCE(name, ''), global_snapshots`

func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var s Session
	var global int
	if err := row.Scan(&s.ID, &s.Start, &s.End, &s.Snapshots, &s.Bytes, &s.Name, &global); err != nil {
		return s, err
	}
	switch {
	case global == 0:
		s.Coverage = "viewport"
	case global == s.Snapshots:
		s.Coverage = "global"
	default:
		s.Coverage = "mixed"
	}
	return s, nil
}

// Sessions lists sessions that hold at least one snapshot, newest first.
func (s *Store) Sessions(ctx context.Context) ([]Session, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE snapshots > 0 ORDER BY start DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// Session returns one session.
func (s *Store) Session(ctx context.Context, id int64) (Session, error) {
	sess, err := scanSession(s.r.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return sess, ErrNotFound
	}
	return sess, err
}

// Index lists every snapshot in a session without its data.
func (s *Store) Index(ctx context.Context, id int64) ([]SnapshotInfo, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT time, aircraft, scope FROM snapshots WHERE session_id = ? ORDER BY time`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SnapshotInfo{}
	for rows.Next() {
		var i SnapshotInfo
		var sc string
		if err := rows.Scan(&i.Time, &i.Aircraft, &sc); err != nil {
			return nil, err
		}
		i.Scope = json.RawMessage(sc)
		out = append(out, i)
	}
	return out, rows.Err()
}

// Rename sets a session's name.
func (s *Store) Rename(ctx context.Context, id int64, name string) error {
	res, err := s.w.ExecContext(ctx, `UPDATE sessions SET name = NULLIF(?, '') WHERE id = ?`, name, id)
	return affected(res, err)
}

// Delete removes a session and everything in it. The open session cannot be
// deleted.
func (s *Store) Delete(ctx context.Context, id int64) error {
	s.mu.Lock()
	current := s.current
	s.mu.Unlock()
	if id == current {
		return errors.New("the session is still being recorded")
	}
	res, err := s.w.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	if err := affected(res, err); err != nil {
		return err
	}
	s.enqueue(reclaim)
	return nil
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Snapshots calls fn for each snapshot with from <= time <= to, oldest
// first, up to limit rows. It reports whether more rows remain. Rows are
// streamed; the session is never loaded whole.
func (s *Store) Snapshots(ctx context.Context, id, from, to int64, limit int, fn func(SnapshotRow) error) (more bool, err error) {
	rows, err := s.r.QueryContext(ctx, `SELECT time, data FROM snapshots
		WHERE session_id = ? AND time >= ? AND time <= ? ORDER BY time LIMIT ?`, id, from, to, limit+1)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		if n == limit {
			return true, nil
		}
		var r SnapshotRow
		if err := rows.Scan(&r.Time, &r.Data); err != nil {
			return false, err
		}
		if err := fn(r); err != nil {
			return false, err
		}
		n++
	}
	return false, rows.Err()
}

// Events returns events with from <= time <= to and id > after, oldest
// first, up to limit. types narrows the result to those event types, or to
// every other type when exclude is set. It reports whether more remain.
func (s *Store) Events(ctx context.Context, id, from, to, after int64, limit int, types []string, exclude bool) (events []Event, more bool, err error) {
	q := `SELECT id, time, type, payload FROM events WHERE session_id = ? AND time >= ? AND time <= ? AND id > ?`
	args := []any{id, from, to, after}
	if len(types) > 0 {
		q += ` AND type`
		if exclude {
			q += ` NOT`
		}
		q += ` IN (?` + strings.Repeat(`, ?`, len(types)-1) + `)`
		for _, t := range types {
			args = append(args, t)
		}
	}
	rows, err := s.r.QueryContext(ctx, q+` ORDER BY time, id LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	events = []Event{}
	for rows.Next() {
		if len(events) == limit {
			return events, true, nil
		}
		var e Event
		var p string
		if err := rows.Scan(&e.ID, &e.Time, &e.Type, &p); err != nil {
			return nil, false, err
		}
		e.Payload = json.RawMessage(p)
		events = append(events, e)
	}
	return events, false, rows.Err()
}
