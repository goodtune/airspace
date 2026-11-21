package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/goodtune/airspace/internal/anomaly"
	"github.com/goodtune/airspace/internal/model"
	"github.com/goodtune/airspace/internal/store"
)

const anomaliesUsage = `usage: airspace anomalies [flags]

Look through recorded sessions for data that cannot be right, such as an
aircraft that jumps across a continent, and list where to watch it in replay.
It reads the database only. It makes no request to OpenSky and changes no data,
and it is safe to run while the server is running.

Kinds:
  position_excursion  jumped away, then back onto its earlier path
  position_jump       moved faster than anything flies, and stayed there
  track_mismatch      reported heading disagrees with the path flown
  altitude_jump       altitude changed faster than any aircraft climbs
  time_reversal       a snapshot's data time is earlier than the one before

flags:
`

// runAnomalies is the "airspace anomalies" command.
func runAnomalies(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("anomalies", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, anomaliesUsage); fs.PrintDefaults() }
	dbDefault := os.Getenv("DB_PATH")
	if dbDefault == "" {
		dbDefault = "./airspace.db"
	}
	db := fs.String("db", dbDefault, "database file (env DB_PATH)")
	session := fs.String("session", "all", "which sessions: all, latest, or a session id")
	format := fs.String("format", "text", "output format: text, json, or csv")
	out := fs.String("o", "", "write to this file instead of standard output")
	kinds := fs.String("kind", "", "only these kinds, comma separated")
	aircraft := fs.String("aircraft", "", "only this aircraft, by icao24 or callsign")
	base := fs.String("url", "", "address of a running Airspace, to print full replay links, for example http://localhost:8080")
	maxSpeed := fs.Float64("max-speed", 0, "implied speed in km/h above which a move is a jump (default 2520)")
	minJump := fs.Float64("min-jump-km", 0, "shortest move that can be a jump (default 15)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "airspace anomalies: "+format+"\n", a...)
		return 1
	}
	if !slices.Contains([]string{"text", "json", "csv"}, *format) {
		return fail("-format must be text, json, or csv")
	}
	var want []string
	for _, k := range strings.Split(*kinds, ",") {
		if k = strings.TrimSpace(k); k == "" {
			continue
		}
		if !slices.Contains(anomaly.Kinds, k) {
			return fail("unknown kind %q; kinds are %s", k, strings.Join(anomaly.Kinds, ", "))
		}
		want = append(want, k)
	}

	st, err := store.OpenReadOnly(*db)
	if err != nil {
		return fail("%v", err)
	}
	defer st.Close()
	ctx := context.Background()
	sessions, err := st.Sessions(ctx)
	if err != nil {
		return fail("%v", err)
	}
	switch *session {
	case "all":
	case "latest":
		if len(sessions) > 1 {
			sessions = sessions[:1] // newest first
		}
	default:
		id, err := strconv.ParseInt(*session, 10, 64)
		if err != nil {
			return fail("-session must be all, latest, or a session id")
		}
		sessions = slices.DeleteFunc(sessions, func(s store.Session) bool { return s.ID != id })
		if len(sessions) == 0 {
			return fail("no session %d in %s", id, *db)
		}
	}

	opt := anomaly.Options{MaxSpeed: *maxSpeed / 3.6, MinJumpKm: *minJump}
	var found []anomaly.Anomaly
	names := map[int64]string{}
	snapshots := 0
	// Oldest session first, so the report reads in time order.
	for i := len(sessions) - 1; i >= 0; i-- {
		s := sessions[i]
		names[s.ID] = s.Name
		det := anomaly.New(s.ID, opt)
		from := int64(0)
		for {
			var last int64
			// A page at a time: a session is never held in memory whole.
			more, err := st.Snapshots(ctx, s.ID, from, 1<<62, 20, func(row store.SnapshotRow) error {
				snap, err := model.Decode(row.Data)
				if err != nil {
					return err
				}
				det.Add(row.Time, snap)
				last = row.Time
				snapshots++
				return nil
			})
			if err != nil {
				return fail("reading session %d: %v", s.ID, err)
			}
			if !more {
				break
			}
			from = last + 1
		}
		found = append(found, det.Result()...)
	}

	found = slices.DeleteFunc(found, func(a anomaly.Anomaly) bool {
		if len(want) > 0 && !slices.Contains(want, a.Kind) {
			return true
		}
		if q := strings.TrimSpace(*aircraft); q != "" && !strings.EqualFold(a.ICAO24, q) && !strings.EqualFold(a.Callsign, q) {
			return true
		}
		return false
	})
	for i := range found {
		found[i].Replay = strings.TrimRight(*base, "/") + found[i].Replay
	}

	w := stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return fail("%v", err)
		}
		defer f.Close()
		w = f
	}
	stamp := func(ms int64) string { return time.UnixMilli(ms).Format("2006-01-02 15:04:05") }
	switch *format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if found == nil {
			found = []anomaly.Anomaly{}
		}
		if err := enc.Encode(found); err != nil {
			return fail("%v", err)
		}
	case "csv":
		cw := csv.NewWriter(w)
		cw.Write([]string{"session", "session_name", "kind", "icao24", "callsign", "start", "end", "snapshots", "distance_km", "detail", "replay"})
		for _, a := range found {
			cw.Write([]string{strconv.FormatInt(a.Session, 10), names[a.Session], a.Kind, a.ICAO24, a.Callsign,
				time.UnixMilli(a.Start).Format(time.RFC3339), time.UnixMilli(a.End).Format(time.RFC3339),
				strconv.Itoa(a.Snapshots), strconv.FormatFloat(a.DistanceKm, 'f', -1, 64), a.Detail, a.Replay})
		}
		cw.Flush()
		if err := cw.Error(); err != nil {
			return fail("%v", err)
		}
	default:
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "TIME\tKIND\tAIRCRAFT\tCALLSIGN\tDETAIL")
		current := int64(-1)
		for _, a := range found {
			if a.Session != current {
				current = a.Session
				tw.Flush()
				fmt.Fprintf(w, "\nSession %d  %s\n", a.Session, names[a.Session])
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", stamp(a.Start), a.Kind, a.ICAO24, a.Callsign, a.Detail)
			fmt.Fprintf(tw, "\t\t\t\twatch: %s\n", a.Replay)
		}
		tw.Flush()
		fmt.Fprintf(w, "\n%d anomalies in %d snapshots across %d session(s).\n", len(found), snapshots, len(sessions))
	}
	return 0
}
