package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goodtune/airspace/internal/anomaly"
	"github.com/goodtune/airspace/internal/model"
	"github.com/goodtune/airspace/internal/scope"
	"github.com/goodtune/airspace/internal/store"
)

// seedDB records one session in which an aircraft jumps away and back.
func seedDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "a.db")
	st, err := store.Open(path, store.Options{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Unix(1_700_000_000, 0)
	st.SessionStart(42, start)
	for i, lon := range []float64{5.00, 5.02, 15.04, 15.06, 5.08, 5.10} {
		at := start.Add(time.Duration(i) * 10 * time.Second)
		st.Snapshot(42, &model.Snapshot{Version: int64(i + 1), Time: at.Unix(), Fetched: at.UnixMilli(), Scope: scope.GlobalPlan(), Fields: model.Fields,
			Aircraft: []model.Aircraft{
				{ICAO24: "486492", Callsign: "KLM83K", Lon: lon, Lat: 52.4, Geo: 3000, Velocity: 140, Track: 90, TimePos: at.Unix(), LastContact: at.Unix()},
				{ICAO24: "aaaaaa", Callsign: "FINE1", Lon: 100 + float64(i)*0.02, Lat: 0, Geo: 9000, Velocity: 220, Track: 90, TimePos: at.Unix(), LastContact: at.Unix()},
			}})
	}
	st.SessionEnd(42, start.Add(time.Minute))
	st.Flush()
	if err := st.Rename(t.Context(), 42, "Test session"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	return path
}

func TestAnomaliesCommand(t *testing.T) {
	db := seedDB(t)
	var out, errs bytes.Buffer

	if code := runAnomalies([]string{"-db", db, "-url", "http://air.test:8080/"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	text := out.String()
	for _, want := range []string{"Session 42  Test session", "position_excursion", "486492", "KLM83K",
		"watch: http://air.test:8080/?replay=42&at=1700000012000&aircraft=486492", "1 anomalies in 6 snapshots across 1 session(s)."} {
		if !strings.Contains(text, want) {
			t.Errorf("text output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "FINE1") {
		t.Error("the well-behaved aircraft was reported")
	}

	out.Reset()
	if code := runAnomalies([]string{"-db", db, "-format", "json", "-session", "latest"}, &out, &errs); code != 0 {
		t.Fatalf("json: exit %d: %s", code, errs.String())
	}
	var found []anomaly.Anomaly
	if err := json.Unmarshal(out.Bytes(), &found); err != nil || len(found) != 1 || found[0].Kind != anomaly.PositionExcursion || found[0].Snapshots != 2 {
		t.Fatalf("json: %v %+v", err, found)
	}

	file := filepath.Join(t.TempDir(), "out.csv")
	out.Reset()
	if code := runAnomalies([]string{"-db", db, "-format", "csv", "-o", file, "-session", "42"}, &out, &errs); code != 0 || out.Len() != 0 {
		t.Fatalf("csv to file: exit %d, stdout %q", code, out.String())
	}
	rows, err := csv.NewReader(strings.NewReader(readFile(t, file))).ReadAll()
	if err != nil || len(rows) != 2 || rows[0][2] != "kind" || rows[1][2] != "position_excursion" || rows[1][1] != "Test session" {
		t.Fatalf("csv: %v %v", err, rows)
	}

	// Filters.
	for _, args := range [][]string{{"-kind", "track_mismatch"}, {"-aircraft", "FINE1"}} {
		out.Reset()
		runAnomalies(append([]string{"-db", db, "-format", "json"}, args...), &out, &errs)
		if strings.TrimSpace(out.String()) != "[]" {
			t.Errorf("%v should match nothing, got %s", args, out.String())
		}
	}
	out.Reset()
	runAnomalies([]string{"-db", db, "-format", "json", "-aircraft", "klm83k"}, &out, &errs)
	if !strings.Contains(out.String(), "486492") {
		t.Error("-aircraft should match a callsign, whatever its case")
	}

	// Mistakes are reported, and nothing is created for a missing database.
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-db", filepath.Join(t.TempDir(), "none.db")}, "no such file"},
		{[]string{"-db", db, "-format", "xml"}, "-format must be"},
		{[]string{"-db", db, "-kind", "ufo"}, `unknown kind "ufo"`},
		{[]string{"-db", db, "-session", "99"}, "no session 99"},
	} {
		errs.Reset()
		if code := runAnomalies(c.args, &out, &errs); code != 1 || !strings.Contains(errs.String(), c.want) {
			t.Errorf("%v: exit %d, %q; want error containing %q", c.args, code, errs.String(), c.want)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
