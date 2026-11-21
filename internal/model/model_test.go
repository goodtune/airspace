package model

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"github.com/goodtune/airspace/internal/scope"
)

const sample = `{"time":1700000000,"states":[
["abc123","QFA1    ","Australia",1699999999,1700000000,151.2,-33.9,10000.5,false,250.1,45.5,-3.2,null,10100,"1234",false,0],
["nopos1","X","Y",null,1700000000,null,null,null,true,null,null,null,null,null,null,false,0],
["def456",null,"France",1699999990,1699999995,2.5,48.8,null,true,0,null,null,null,null,null,false,0]]}`

// eq compares aircraft treating NaN as equal to NaN.
func eq(a, b Aircraft) bool {
	fa, fb := []float64{a.Lon, a.Lat, a.Baro, a.Geo, a.Velocity, a.Track, a.VRate}, []float64{b.Lon, b.Lat, b.Baro, b.Geo, b.Velocity, b.Track, b.VRate}
	for i := range fa {
		if fa[i] != fb[i] && !(math.IsNaN(fa[i]) && math.IsNaN(fb[i])) {
			return false
		}
	}
	return a.ICAO24 == b.ICAO24 && a.Callsign == b.Callsign && a.Country == b.Country && a.OnGround == b.OnGround &&
		a.Squawk == b.Squawk && a.TimePos == b.TimePos && a.LastContact == b.LastContact
}

func TestParseStates(t *testing.T) {
	tm, ac, err := ParseStates([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if tm != 1700000000 || len(ac) != 2 {
		t.Fatalf("time %d, %d aircraft; want 2 (null position dropped)", tm, len(ac))
	}
	a := ac[0]
	if a.ICAO24 != "abc123" || a.Callsign != "QFA1" || a.Lon != 151.2 || a.Baro != 10000.5 || a.Geo != 10100 ||
		a.Velocity != 250.1 || a.Track != 45.5 || a.VRate != -3.2 || a.Squawk != "1234" || a.TimePos != 1699999999 {
		t.Errorf("bad parse: %+v", a)
	}
	if b := ac[1]; !math.IsNaN(b.Baro) || !b.OnGround || b.Callsign != "" {
		t.Errorf("bad parse of nulls: %+v", b)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	_, ac, _ := ParseStates([]byte(sample))
	s := &Snapshot{Version: 7, Time: 1700000000, Fetched: 1700000001234, Fields: Fields,
		Scope: scope.Plan{Boxes: []scope.Box{{LaMin: -36, LoMin: 149, LaMax: -31, LoMax: 154}}, Cost: 1}, Aircraft: ac}
	blob, err := s.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(blob)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != s.Version || got.Time != s.Time || got.Fetched != s.Fetched || !reflect.DeepEqual(got.Scope, s.Scope) || len(got.Aircraft) != len(s.Aircraft) {
		t.Fatalf("round trip changed header: %+v", got)
	}
	for i := range ac {
		if !eq(ac[i], got.Aircraft[i]) {
			t.Errorf("aircraft %d changed: %+v -> %+v", i, ac[i], got.Aircraft[i])
		}
	}
	// The wire form is an array with nulls for unknowns.
	j, _ := json.Marshal(ac[1])
	if string(j) != `["def456","","France",2.5,48.8,null,null,true,0,null,null,"",1699999990,1699999995]` {
		t.Errorf("unexpected JSON: %s", j)
	}
}
