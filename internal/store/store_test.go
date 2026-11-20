package store

import (
	"context"
	"io"
	"log/slog"
	"math/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/goodtune/airspace/internal/model"
	"github.com/goodtune/airspace/internal/scope"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))
var ctx = context.Background()

func open(t *testing.T, opt Options) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path, opt, quiet)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func snap(fetched time.Time, n int, global bool) *model.Snapshot {
	r := rand.New(rand.NewSource(fetched.UnixNano()))
	s := &model.Snapshot{Version: 1, Time: fetched.Unix(), Fetched: fetched.UnixMilli(), Fields: model.Fields,
		Scope: scope.Plan{Boxes: []scope.Box{{LaMin: -36, LoMin: 149, LaMax: -31, LoMax: 154}}, Cost: 1}}
	if global {
		s.Scope = scope.GlobalPlan()
	}
	for i := 0; i < n; i++ {
		s.Aircraft = append(s.Aircraft, model.Aircraft{ICAO24: "abc" + string(rune('a'+i%26)), Lon: r.Float64() * 100, Lat: r.Float64() * 50,
			Baro: r.Float64() * 1e4, Geo: r.Float64() * 1e4, Velocity: r.Float64() * 250, Track: r.Float64() * 360, TimePos: fetched.Unix(), LastContact: fetched.Unix()})
	}
	return s
}

// fill records a closed session of n snapshots starting at start.
func fill(s *Store, id int64, start time.Time, n, aircraft int, global bool) {
	s.SessionStart(id, start)
	for i := 0; i < n; i++ {
		s.Snapshot(id, snap(start.Add(time.Duration(i)*10*time.Second), aircraft, global))
	}
	s.SessionEnd(id, start.Add(time.Duration(n)*10*time.Second))
}

func TestRoundTripAndSessionMaths(t *testing.T) {
	s, _ := open(t, Options{})
	start := time.UnixMilli(1_700_000_000_000)
	want := snap(start, 50, false)
	s.SessionStart(1, start)
	s.Snapshot(1, want)
	s.Snapshot(1, snap(start.Add(10*time.Second), 50, true))
	s.SessionEnd(1, start.Add(20*time.Second))
	s.Flush()

	sess, err := s.Session(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Snapshots != 2 || sess.Bytes == 0 || sess.Coverage != "mixed" || sess.End-sess.Start != 20000 {
		t.Errorf("session %+v", sess)
	}
	var got []*model.Snapshot
	more, err := s.Snapshots(ctx, 1, 0, 1<<62, 10, func(r SnapshotRow) error {
		d, err := model.Decode(r.Data)
		got = append(got, d)
		return err
	})
	if err != nil || more || len(got) != 2 {
		t.Fatalf("read back: %v more=%v n=%d", err, more, len(got))
	}
	if len(got[0].Aircraft) != 50 || got[0].Aircraft[7] != want.Aircraft[7] || got[0].Scope.Boxes[0] != want.Scope.Boxes[0] {
		t.Errorf("stored blob did not decode to the same data")
	}
	idx, _ := s.Index(ctx, 1)
	if len(idx) != 2 || idx[0].Aircraft != 50 || idx[0].Time != start.UnixMilli() {
		t.Errorf("index %+v", idx)
	}
	if _, err := s.Session(ctx, 99); err != ErrNotFound {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestSnapshotPaging(t *testing.T) {
	s, _ := open(t, Options{})
	start := time.UnixMilli(1_700_000_000_000)
	fill(s, 1, start, 25, 5, false)
	s.Flush()
	var times []int64
	from := int64(0)
	pages := 0
	for {
		var last int64
		more, err := s.Snapshots(ctx, 1, from, 1<<62, 10, func(r SnapshotRow) error {
			times = append(times, r.Time)
			last = r.Time
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if !more {
			break
		}
		from = last + 1
	}
	if pages != 3 || len(times) != 25 {
		t.Fatalf("%d pages, %d rows; want 3 and 25", pages, len(times))
	}
	for i := 1; i < len(times); i++ {
		if times[i] <= times[i-1] {
			t.Fatal("rows out of order or repeated")
		}
	}
	// A time range selects only what is inside it.
	n := 0
	s.Snapshots(ctx, 1, start.UnixMilli()+20000, start.UnixMilli()+50000, 100, func(SnapshotRow) error { n++; return nil })
	if n != 4 {
		t.Errorf("range returned %d rows, want 4", n)
	}
}

func TestEventsOrderedAndPaged(t *testing.T) {
	s, _ := open(t, Options{})
	start := time.UnixMilli(1_700_000_000_000)
	s.SessionStart(1, start)
	for i := 0; i < 12; i++ {
		// Two events share each timestamp; insertion order must hold.
		s.Event(1, start.Add(time.Duration(i/2)*time.Second), "camera", map[string]int{"n": i})
	}
	s.Flush()
	var all []Event
	after := int64(0)
	for {
		ev, more, err := s.Events(ctx, 1, 0, 1<<62, after, 5)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, ev...)
		if !more {
			break
		}
		after = ev[len(ev)-1].ID
	}
	if len(all) != 12 {
		t.Fatalf("%d events, want 12", len(all))
	}
	for i, e := range all {
		if string(e.Payload) != `{"n":`+string(rune('0'+i%10))+`}` && i < 10 {
			t.Errorf("event %d out of order: %s", i, e.Payload)
		}
		if i > 0 && (e.Time < all[i-1].Time || e.ID <= all[i-1].ID) {
			t.Errorf("event %d not in time order", i)
		}
	}
}

func TestRenameDeleteAndEmptySession(t *testing.T) {
	s, _ := open(t, Options{})
	start := time.Now().Add(-time.Hour)
	fill(s, 1, start, 3, 5, true)
	s.SessionStart(2, start.Add(time.Minute)) // stores nothing
	s.SessionEnd(2, start.Add(2*time.Minute))
	s.Flush()
	list, _ := s.Sessions(ctx)
	if len(list) != 1 || list[0].Coverage != "global" {
		t.Fatalf("want one global session (empty one dropped), got %+v", list)
	}
	if err := s.Rename(ctx, 1, "Sydney evening"); err != nil {
		t.Fatal(err)
	}
	if sess, _ := s.Session(ctx, 1); sess.Name != "Sydney evening" {
		t.Errorf("name %q", sess.Name)
	}
	if err := s.Rename(ctx, 42, "x"); err != ErrNotFound {
		t.Errorf("rename missing: %v", err)
	}
	if err := s.Delete(ctx, 1); err != nil {
		t.Fatal(err)
	}
	n := 0
	s.Snapshots(ctx, 1, 0, 1<<62, 10, func(SnapshotRow) error { n++; return nil })
	if list, _ := s.Sessions(ctx); len(list) != 0 || n != 0 {
		t.Errorf("delete left data behind: %d sessions, %d snapshots", len(list), n)
	}
	// The session being recorded cannot be deleted.
	s.SessionStart(3, time.Now())
	s.Flush()
	if err := s.Delete(ctx, 3); err == nil {
		t.Error("deleted the open session")
	}
}

func TestPruneByAge(t *testing.T) {
	s, _ := open(t, Options{RetentionDays: 7})
	fill(s, 1, time.Now().Add(-10*24*time.Hour), 3, 5, false)
	fill(s, 2, time.Now().Add(-time.Hour), 3, 5, false)
	s.Flush()
	list, _ := s.Sessions(ctx)
	if len(list) != 1 || list[0].ID != 2 {
		t.Fatalf("want only the recent session, got %+v", list)
	}
}

func TestPruneBySize(t *testing.T) {
	s, _ := open(t, Options{MaxSize: 600 << 10})
	base := time.Now().Add(-5 * time.Hour)
	for i := int64(1); i <= 5; i++ {
		fill(s, i, base.Add(time.Duration(i)*time.Hour/2), 10, 600, false) // about 200 kB each
	}
	s.Flush()
	list, _ := s.Sessions(ctx)
	if len(list) == 0 || len(list) >= 5 {
		t.Fatalf("%d sessions left, want some pruned and some kept", len(list))
	}
	if list[0].ID != 5 {
		t.Errorf("newest session should survive, got %+v", list)
	}
	for _, sess := range list {
		if sess.ID == 1 {
			t.Error("oldest session should be pruned first")
		}
	}
	if sz, _ := s.Size(); sz > 600<<10 {
		t.Errorf("size %d still above the limit", sz)
	}
}

func TestReopenKeepsDataAndClosesCrashedSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path, Options{}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	start := time.UnixMilli(1_700_000_000_000)
	s.SessionStart(1, start)
	s.Snapshot(1, snap(start.Add(30*time.Second), 5, false))
	s.SaveMeta("camera", map[string]float64{"lon": 151})
	s.Close() // no SessionEnd: as if the process died

	s, err = Open(path, Options{}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sess, err := s.Session(ctx, 1)
	if err != nil || sess.End != start.Add(30*time.Second).UnixMilli() || sess.Snapshots != 1 {
		t.Errorf("after reopen: %+v %v", sess, err)
	}
	var cam map[string]float64
	if ok, err := s.LoadMeta("camera", &cam); !ok || err != nil || cam["lon"] != 151 {
		t.Errorf("meta lost: %v %v %v", ok, err, cam)
	}
	if ok, _ := s.LoadMeta("nothing", &cam); ok {
		t.Error("missing key reported as present")
	}
}

func TestOpenFailureIsAnError(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "no", "such", "dir", "x.db"), Options{}, quiet); err == nil {
		t.Fatal("want an error for an unusable path")
	}
}
