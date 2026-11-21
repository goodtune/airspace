package anomaly

import (
	"bufio"
	"compress/gzip"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/goodtune/airspace/internal/model"
)

const t0 = int64(1_700_000_000) // unix s

// ac is an aircraft at a position, moving east at 230 m/s unless told otherwise.
func ac(id string, lon, lat float64, sec int64, mod ...func(*model.Aircraft)) model.Aircraft {
	a := model.Aircraft{ICAO24: id, Callsign: strings.ToUpper(id), Lon: lon, Lat: lat, Geo: 10000, Baro: 10000,
		Velocity: 230, Track: 90, VRate: 0, TimePos: t0 + sec, LastContact: t0 + sec}
	for _, m := range mod {
		m(&a)
	}
	return a
}

// run feeds one aircraft's states, ten seconds apart, through a detector.
func run(states ...model.Aircraft) []Anomaly {
	d := New(7, Options{})
	for _, a := range states {
		d.Add(a.TimePos*1000, &model.Snapshot{Time: a.TimePos, Aircraft: []model.Aircraft{a}})
	}
	return d.Result()
}

// east is the longitude reached flying east along the equator at 230 m/s.
func east(sec int64) float64 { return float64(sec) * 230 / 111195 }

func TestCleanFlightHasNoAnomalies(t *testing.T) {
	var s []model.Aircraft
	for i := int64(0); i < 30; i++ {
		s = append(s, ac("ok", east(i*10), 0, i*10))
	}
	if got := run(s...); len(got) != 0 {
		t.Fatalf("clean flight flagged: %+v", got)
	}
}

func TestExcursionAndReturnIsOneFinding(t *testing.T) {
	got := run(
		ac("klm", east(0), 52, 0), ac("klm", east(10), 52, 10),
		ac("klm", east(20)+10, 52, 20), // 10 degrees of longitude off
		ac("klm", east(30)+10, 52, 30),
		ac("klm", east(40), 52, 40), // back
		ac("klm", east(50), 52, 50),
	)
	if len(got) != 1 {
		t.Fatalf("want one finding, got %d: %+v", len(got), got)
	}
	a := got[0]
	if a.Kind != PositionExcursion || a.Snapshots != 2 || a.Start != (t0+20)*1000 || a.End != (t0+40)*1000 {
		t.Errorf("excursion: %+v", a)
	}
	if a.DistanceKm < 600 || a.DistanceKm > 760 || !strings.Contains(a.Detail, "back on its earlier path") {
		t.Errorf("detail: %v km, %q", a.DistanceKm, a.Detail)
	}
	if a.Replay != "/?replay=7&at=1700000012000&aircraft=klm" || a.Session != 7 || a.Callsign != "KLM" {
		t.Errorf("replay link %q", a.Replay)
	}
}

func TestJumpThatStaysIsAJump(t *testing.T) {
	s := []model.Aircraft{ac("gone", east(0), 0, 0), ac("gone", east(10), 0, 10)}
	for i := int64(2); i < 8; i++ {
		s = append(s, ac("gone", east(i*10)+30, 0, i*10)) // carries on from the new place
	}
	got := run(s...)
	if len(got) != 1 || got[0].Kind != PositionJump || got[0].Snapshots != 1 {
		t.Fatalf("want one position_jump, got %+v", got)
	}
}

func TestFastButPossibleIsNotAJump(t *testing.T) {
	// 600 m/s over 20 s is 12 km: under the speed limit and the distance floor.
	if got := run(ac("fast", 0, 0, 0), ac("fast", 12000.0/111195, 0, 20)); len(got) != 0 {
		t.Fatalf("flagged: %+v", got)
	}
	// A long gap between sightings explains a long move.
	if got := run(ac("gap", 0, 0, 0), ac("gap", east(600), 0, 600)); len(got) != 0 {
		t.Fatalf("flagged after a gap: %+v", got)
	}
}

func TestTrackMismatchAndReplayedVector(t *testing.T) {
	climb := func(a *model.Aircraft) { a.Velocity, a.Track, a.VRate = 119.34, 20.05, 19.51 }
	var s []model.Aircraft
	// Climb out heading north-north-east, then turn east and fly on.
	for i := int64(0); i < 4; i++ {
		s = append(s, ac("v", float64(i)*0.004, float64(i)*0.01, i*10, climb))
	}
	lon, lat := 0.012, 0.03
	for i := int64(4); i < 16; i++ {
		lon += 157.0 * 10 / 111195
		s = append(s, ac("v", lon, lat, i*10, func(a *model.Aircraft) { a.Velocity, a.Track = 157+float64(i)/10, 90 }))
	}
	// Still flying east, but the old climb-out vector comes back.
	lon += 157.0 * 10 / 111195
	s = append(s, ac("v", lon, lat, 160, climb))
	got := run(s...)
	if len(got) != 1 || got[0].Kind != TrackMismatch {
		t.Fatalf("want one track_mismatch, got %+v", got)
	}
	if d := got[0].Detail; !strings.Contains(d, "reports track 20°") || !strings.Contains(d, "flew 90°") || !strings.Contains(d, "the one it sent at") {
		t.Errorf("detail %q", d)
	}
}

func TestTurningAircraftIsNotAMismatch(t *testing.T) {
	// A tight turn: 9 degrees a second, three times a standard-rate turn.
	// The path bearing lags the reported track by half of each 90 degree
	// step, well inside the limit.
	var s []model.Aircraft
	lon, lat, track := 0.0, 0.0, 0.0
	for i := int64(0); i < 20; i++ {
		s = append(s, ac("turn", lon, lat, i*10, func(a *model.Aircraft) { a.Track = track }))
		mid := (track + 45) * math.Pi / 180
		lon += 2300 * math.Sin(mid) / 111195
		lat += 2300 * math.Cos(mid) / 111195
		track = math.Mod(track+90, 360)
	}
	if got := run(s...); len(got) != 0 {
		t.Fatalf("a turn was flagged: %+v", got)
	}
}

func TestAltitudeJump(t *testing.T) {
	got := run(ac("alt", east(0), 0, 0), ac("alt", east(10), 0, 10, func(a *model.Aircraft) { a.Geo = 1387 }))
	if len(got) != 1 || got[0].Kind != AltitudeJump {
		t.Fatalf("want altitude_jump, got %+v", got)
	}
	// A steep but real descent is fine: 60 m/s for 10 s.
	if got := run(ac("dive", east(0), 0, 0), ac("dive", east(10), 0, 10, func(a *model.Aircraft) { a.Geo = 9400 })); len(got) != 0 {
		t.Fatalf("flagged: %+v", got)
	}
}

func TestTimeReversal(t *testing.T) {
	d := New(1, Options{})
	for _, sec := range []int64{0, 10, 3, 20} {
		d.Add((t0+sec)*1000, &model.Snapshot{Time: t0 + sec})
	}
	got := d.Result()
	if len(got) != 1 || got[0].Kind != TimeReversal || got[0].ICAO24 != "" || !strings.Contains(got[0].Detail, "went back 7 s") {
		t.Fatalf("got %+v", got)
	}
	if got[0].Replay != "/?replay=1&at=1699999995000" {
		t.Errorf("replay %q", got[0].Replay)
	}
}

func TestOptionsChangeTheThresholds(t *testing.T) {
	states := []model.Aircraft{ac("x", 0, 0, 0), ac("x", 0.09, 0, 10)} // 10 km in 10 s
	if got := run(states...); len(got) != 0 {
		t.Fatalf("default floor is 15 km: %+v", got)
	}
	d := New(1, Options{MinJumpKm: 5})
	for _, a := range states {
		d.Add(a.TimePos*1000, &model.Snapshot{Time: a.TimePos, Aircraft: []model.Aircraft{a}})
	}
	if got := d.Result(); len(got) != 1 {
		t.Fatalf("with a 5 km floor: %+v", got)
	}
}

// The captured traffic holds the glitch that prompted this tool: KLM83K
// shown 10 degrees of longitude east of where it was, for two snapshots,
// and later an old velocity vector on a current position.
func TestFindsTheKLM83KGlitchInTheCapture(t *testing.T) {
	fh, err := os.Open("../../testdata/capture/global-2026-11-21.jsonl.gz")
	if err != nil {
		t.Skip("capture not present")
	}
	defer fh.Close()
	zr, err := gzip.NewReader(fh)
	if err != nil {
		t.Skip("capture is a Git LFS pointer; run git lfs pull")
	}
	d := New(1, Options{})
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 1<<20), 256<<20)
	for sc.Scan() {
		tm, aircraft, err := model.ParseStates(sc.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		d.Add(tm*1000, &model.Snapshot{Time: tm, Aircraft: aircraft})
	}
	var excursion, mismatch *Anomaly
	total := 0
	for _, a := range d.Result() {
		total++
		if a.ICAO24 != "486492" {
			continue
		}
		a := a
		switch a.Kind {
		case PositionExcursion:
			excursion = &a
		case TrackMismatch:
			mismatch = &a
		}
	}
	if excursion == nil || excursion.Snapshots != 2 || excursion.DistanceKm < 670 || excursion.DistanceKm > 690 || excursion.Callsign != "KLM83K" {
		t.Errorf("KLM83K excursion: %+v", excursion)
	}
	if mismatch == nil || !strings.Contains(mismatch.Detail, "reports track 46°") {
		t.Errorf("KLM83K track mismatch: %+v", mismatch)
	}
	// About 1.1 million aircraft states: findings must stay a short list.
	if total < 50 || total > 600 {
		t.Errorf("%d findings in the capture; expected a few hundred at most", total)
	}
	t.Logf("%d findings in the capture", total)
}
