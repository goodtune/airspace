// Command devtool holds development helpers. It is not part of the shipped
// binary.
//
//	devtool fake -addr :9090 -capture file.jsonl.gz
//	    Serve a fake OpenSky API that replays a capture file, so the app can
//	    be run and tested without spending credits.
//
//	devtool seed -db airspace.db -capture file.jsonl.gz
//	    Write a capture file into a database as one recorded global session,
//	    so replay and export can be worked on with no upstream at all.
package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/goodtune/airspace/internal/fakeopensky"
	"github.com/goodtune/airspace/internal/hub"
	"github.com/goodtune/airspace/internal/model"
	"github.com/goodtune/airspace/internal/scope"
	"github.com/goodtune/airspace/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: devtool fake|seed [flags]")
	}
	switch os.Args[1] {
	case "fake":
		fake(os.Args[2:])
	case "seed":
		seed(os.Args[2:])
	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}

func fake(args []string) {
	fs := flag.NewFlagSet("fake", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:9090", "listen address")
	capture := fs.String("capture", "", "capture file to replay; synthetic traffic when empty")
	credits := fs.Int("credits", 4000, "starting credit balance")
	fs.Parse(args)

	f := fakeopensky.New()
	f.SetRemaining(*credits)
	if *capture != "" {
		c, err := fakeopensky.LoadCapture(*capture)
		if err != nil {
			log.Fatal(err)
		}
		f.SetSource(c.Source)
		log.Printf("replaying %d frames from %s", c.Frames(), *capture)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.Handler())
	// Counters, so end-to-end tests can assert on upstream traffic.
	mux.HandleFunc("/_stats", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tokenRequests":%d,"stateRequests":%d,"remaining":%d}`+"\n",
			f.TokenRequests(), f.StateRequests(), f.Remaining())
	})
	mux.HandleFunc("/_failing", func(w http.ResponseWriter, r *http.Request) {
		f.SetFailing(r.URL.Query().Get("on") == "1")
	})
	mux.HandleFunc("/_remaining", func(w http.ResponseWriter, r *http.Request) {
		var n int
		fmt.Sscan(r.URL.Query().Get("n"), &n)
		f.SetRemaining(n)
	})
	log.Printf("fake OpenSky on http://%s (token at /token, API at /api)", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func seed(args []string) {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	db := fs.String("db", "airspace.db", "database to write")
	capture := fs.String("capture", "", "capture file")
	name := fs.String("name", "Captured global traffic", "session name")
	bbox := fs.String("bbox", "", "keep only lamin,lomin,lamax,lomax, recorded as a viewport session")
	fs.Parse(args)
	if *capture == "" {
		log.Fatal("-capture is required")
	}
	plan := scope.GlobalPlan()
	var box *scope.Box
	if *bbox != "" {
		box = &scope.Box{}
		if _, err := fmt.Sscanf(*bbox, "%f,%f,%f,%f", &box.LaMin, &box.LoMin, &box.LaMax, &box.LoMax); err != nil {
			log.Fatalf("-bbox: %v", err)
		}
		plan = scope.Plan{Boxes: []scope.Box{*box}, Cost: scope.Cost(box.Area())}
	}

	fh, err := os.Open(*capture)
	if err != nil {
		log.Fatal(err)
	}
	defer fh.Close()
	zr, err := gzip.NewReader(fh)
	if err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(*db, store.Options{}, slog.Default())
	if err != nil {
		log.Fatal(err)
	}

	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 1<<20), 256<<20)
	var id int64
	var first, last time.Time
	n := 0
	for sc.Scan() {
		t, aircraft, err := model.ParseStates(sc.Bytes())
		if err != nil {
			log.Printf("skipping a bad frame: %v", err)
			continue
		}
		if box != nil {
			kept := aircraft[:0]
			for _, a := range aircraft {
				if a.Lat >= box.LaMin && a.Lat <= box.LaMax && a.Lon >= box.LoMin && a.Lon <= box.LoMax {
					kept = append(kept, a)
				}
			}
			aircraft = kept
		}
		last = time.Unix(t, 0)
		if n == 0 {
			first, id = last, last.UnixMilli()
			// Seeding the same capture twice must make two sessions.
			existing, _ := st.Sessions(context.Background())
			for taken := true; taken; {
				taken = false
				for _, e := range existing {
					if e.ID == id {
						id, taken = id+1, true
					}
				}
			}
			st.SessionStart(id, first)
			cam := hub.Camera{Lon: 10, Lat: 48, Height: 6e6, Pitch: -90, Seq: 1}
			if box != nil {
				cam = hub.Camera{Lon: (box.LoMin + box.LoMax) / 2, Lat: (box.LaMin + box.LaMax) / 2, Height: 9e5, Pitch: -90, Seq: 1}
			}
			st.Event(id, first, "state", hub.State{Camera: cam, Recording: "on", Coverage: map[bool]string{true: "viewport", false: "global"}[box != nil], Scope: plan})
		}
		n++
		st.Snapshot(id, &model.Snapshot{Version: int64(n), Time: t, Fetched: last.UnixMilli(), Scope: plan, Fields: model.Fields, Aircraft: aircraft})
		st.Flush()
	}
	if err := sc.Err(); err != nil {
		log.Fatal(err)
	}
	if n == 0 {
		log.Fatal("no usable frames")
	}
	st.SessionEnd(id, last)
	st.Flush()
	if err := st.Rename(context.Background(), id, *name); err != nil {
		log.Fatal(err)
	}
	size, _ := st.Size()
	st.Close()
	log.Printf("seeded session %d: %d snapshots over %s, database %.1f MB", id, n, last.Sub(first), float64(size)/1e6)
}
