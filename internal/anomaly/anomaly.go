// Package anomaly looks through recorded snapshots for data that cannot be
// right: aircraft that jump hundreds of kilometres, velocity vectors that
// disagree with the path flown, and the like. It only reports. It never
// changes or filters the recorded data.
package anomaly

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/goodtune/airspace/internal/model"
)

// Kinds of anomaly.
const (
	// PositionExcursion: the aircraft jumped away and later jumped back to
	// where its earlier path says it should be.
	PositionExcursion = "position_excursion"
	// PositionJump: the aircraft moved faster than anything flies, and did
	// not come back within the look-ahead window.
	PositionJump = "position_jump"
	// TrackMismatch: the reported velocity vector disagrees with the path
	// the positions trace out.
	TrackMismatch = "track_mismatch"
	// AltitudeJump: the altitude changed faster than any aircraft climbs.
	AltitudeJump = "altitude_jump"
	// TimeReversal: a snapshot's data time is earlier than the one before.
	TimeReversal = "time_reversal"
)

// Kinds lists every kind, for help text and validation.
var Kinds = []string{PositionExcursion, PositionJump, TrackMismatch, AltitudeJump, TimeReversal}

// Anomaly is one finding.
type Anomaly struct {
	Session  int64  `json:"session"`
	Kind     string `json:"kind"`
	ICAO24   string `json:"icao24,omitempty"`
	Callsign string `json:"callsign,omitempty"`
	Start    int64  `json:"start"` // unix ms, the snapshot where it begins
	End      int64  `json:"end"`   // unix ms, the snapshot where it ends
	// Snapshots is how many snapshots show the bad data.
	Snapshots  int     `json:"snapshots"`
	DistanceKm float64 `json:"distanceKm,omitempty"`
	Detail     string  `json:"detail"`
	// Replay is the path and query that opens the replay just before the
	// anomaly with the aircraft selected.
	Replay string `json:"replay"`
}

// Options tune the detector. The zero value uses the defaults.
type Options struct {
	// MaxSpeed is the implied ground speed, in m/s, above which a move
	// between two positions counts as a jump. Default 700 (about Mach 2).
	MaxSpeed float64
	// MinJumpKm is the shortest move that can count as a jump. Default 15.
	MinJumpKm float64
	// ReturnWindow is how long after a jump a return still pairs with it.
	// Default 5 minutes.
	ReturnWindow time.Duration
	// TrackDegrees is how far the reported track may differ from the path
	// flown before it is flagged. Default 60.
	TrackDegrees float64
}

func (o Options) withDefaults() Options {
	if o.MaxSpeed <= 0 {
		o.MaxSpeed = 700
	}
	if o.MinJumpKm <= 0 {
		o.MinJumpKm = 15
	}
	if o.ReturnWindow <= 0 {
		o.ReturnWindow = 5 * time.Minute
	}
	if o.TrackDegrees <= 0 {
		o.TrackDegrees = 60
	}
	return o
}

type vector struct {
	velocity, track, vrate float64
	at                     int64 // snapshot time, unix ms
}

type state struct {
	a    model.Aircraft
	at   int64 // snapshot time, unix ms
	prev []vector
	// pending is a jump waiting to see whether the aircraft comes back.
	pending *pendingJump
}

type pendingJump struct {
	index  int            // position in Detector.found
	from   model.Aircraft // last good state before the jump
	fromAt int64
	count  int // snapshots spent displaced
}

// Detector consumes one session's snapshots in order.
type Detector struct {
	opt      Options
	session  int64
	seen     map[string]*state
	lastTime int64 // data time of the previous snapshot, unix s
	lastAt   int64
	found    []Anomaly
}

// New makes a detector for one session.
func New(session int64, opt Options) *Detector {
	return &Detector{opt: opt.withDefaults(), session: session, seen: map[string]*state{}}
}

const earthRadius = 6371008.8

func rad(d float64) float64 { return d * math.Pi / 180 }

// distance is the great-circle distance in metres.
func distance(lon1, lat1, lon2, lat2 float64) float64 {
	p1, p2 := rad(lat1), rad(lat2)
	h := math.Pow(math.Sin((p2-p1)/2), 2) + math.Cos(p1)*math.Cos(p2)*math.Pow(math.Sin(rad(lon2-lon1)/2), 2)
	return 2 * earthRadius * math.Asin(math.Min(1, math.Sqrt(h)))
}

// bearing is the initial bearing from the first point to the second, in
// degrees clockwise from north.
func bearing(lon1, lat1, lon2, lat2 float64) float64 {
	p1, p2, dl := rad(lat1), rad(lat2), rad(lon2-lon1)
	b := math.Atan2(math.Sin(dl)*math.Cos(p2), math.Cos(p1)*math.Sin(p2)-math.Sin(p1)*math.Cos(p2)*math.Cos(dl))
	return math.Mod(b*180/math.Pi+360, 360)
}

func angleDiff(a, b float64) float64 {
	return math.Abs(math.Mod(b-a+540, 360) - 180)
}

// advance moves a point along a track for a distance in metres.
func advance(lon, lat, track, dist float64) (float64, float64) {
	d, t, p1, l1 := dist/earthRadius, rad(track), rad(lat), rad(lon)
	p2 := math.Asin(math.Sin(p1)*math.Cos(d) + math.Cos(p1)*math.Sin(d)*math.Cos(t))
	l2 := l1 + math.Atan2(math.Sin(t)*math.Sin(d)*math.Cos(p1), math.Cos(d)-math.Sin(p1)*math.Sin(p2))
	return l2 * 180 / math.Pi, p2 * 180 / math.Pi
}

func clock(ms int64) string { return time.UnixMilli(ms).Format("15:04:05") }

func (d *Detector) add(a Anomaly) int {
	a.Session = d.session
	if a.End == 0 {
		a.End = a.Start
	}
	if a.Snapshots == 0 {
		a.Snapshots = 1
	}
	// Open a few seconds early so the anomaly is seen happening.
	a.Replay = fmt.Sprintf("/?replay=%d&at=%d", d.session, a.Start-8000)
	if a.ICAO24 != "" {
		a.Replay += "&aircraft=" + a.ICAO24
	}
	d.found = append(d.found, a)
	return len(d.found) - 1
}

// Add examines the next snapshot. at is when it was recorded, unix ms.
func (d *Detector) Add(at int64, snap *model.Snapshot) {
	if d.lastTime != 0 && snap.Time < d.lastTime {
		d.add(Anomaly{Kind: TimeReversal, Start: at,
			Detail: fmt.Sprintf("snapshot data time went back %d s (to %s from %s)", d.lastTime-snap.Time,
				time.Unix(snap.Time, 0).Format("15:04:05"), time.Unix(d.lastTime, 0).Format("15:04:05"))})
	}
	d.lastTime, d.lastAt = snap.Time, at

	for _, a := range snap.Aircraft {
		st := d.seen[a.ICAO24]
		if st == nil {
			d.seen[a.ICAO24] = &state{a: a, at: at}
			continue
		}
		d.compare(st, a, at, snap.Time)
		st.remember(a, at)
		st.a, st.at = a, at
	}
}

// remember keeps the distinct velocity vectors seen lately, to recognise one
// that is replayed later.
func (s *state) remember(a model.Aircraft, at int64) {
	if math.IsNaN(a.Velocity) || math.IsNaN(a.Track) {
		return
	}
	v := vector{a.Velocity, a.Track, a.VRate, at}
	if n := len(s.prev); n > 0 && s.prev[n-1].velocity == v.velocity && s.prev[n-1].track == v.track {
		return
	}
	s.prev = append(s.prev, v)
	if len(s.prev) > 64 {
		s.prev = s.prev[1:]
	}
}

func (d *Detector) compare(st *state, a model.Aircraft, at, dataTime int64) {
	prev := st.a
	dist := distance(prev.Lon, prev.Lat, a.Lon, a.Lat)
	dt := float64(a.TimePos - prev.TimePos)
	if a.TimePos == 0 || prev.TimePos == 0 || dt <= 0 {
		// No usable position times: fall back to when the snapshots were taken.
		dt = float64(at-st.at) / 1000
	}
	if dt < 1 {
		dt = 1
	}
	speed := dist / dt
	jumped := dist >= d.opt.MinJumpKm*1000 && speed > d.opt.MaxSpeed

	if p := st.pending; p != nil {
		switch {
		case jumped && d.returned(p, a, at):
			// It came back: one excursion, not two jumps.
			f := &d.found[p.index]
			f.Kind, f.End, f.Snapshots = PositionExcursion, at, p.count
			f.Detail = fmt.Sprintf("away for %d snapshot(s) over %s, then back on its earlier path: %s",
				p.count, time.Duration(at-f.Start)*time.Millisecond, f.Detail)
			st.pending = nil
			return
		case time.Duration(at-p.fromAt)*time.Millisecond > d.opt.ReturnWindow:
			st.pending = nil // it stayed away: the jump stands as reported
		default:
			p.count++
		}
	}

	if jumped {
		lag := ""
		if l := a.LastContact - a.TimePos; a.TimePos != 0 && l >= 5 {
			lag = fmt.Sprintf("; its position was %d s older than its last contact", l)
		}
		idx := d.add(Anomaly{Kind: PositionJump, ICAO24: a.ICAO24, Callsign: a.Callsign, Start: at, DistanceKm: math.Round(dist/100) / 10,
			Detail: fmt.Sprintf("moved %.0f km in %.0f s (%.0f km/h implied, %.0f km/h reported)%s",
				dist/1000, dt, speed*3.6, zeroNaN(a.Velocity)*3.6, lag)})
		if st.pending == nil {
			st.pending = &pendingJump{index: idx, from: prev, fromAt: st.at, count: 1}
		}
		return // the vector checks mean nothing across a jump
	}

	if !a.OnGround && !prev.OnGround && !math.IsNaN(a.Geo) && !math.IsNaN(prev.Geo) {
		if dz := math.Abs(a.Geo - prev.Geo); dz > 1500 && dz/dt > 150 {
			d.add(Anomaly{Kind: AltitudeJump, ICAO24: a.ICAO24, Callsign: a.Callsign, Start: at,
				Detail: fmt.Sprintf("altitude went from %.0f m to %.0f m in %.0f s", prev.Geo, a.Geo, dt)})
		}
	}

	// The reported track should agree with the direction actually flown.
	// Needs a fast aircraft and a real displacement, or noise dominates.
	if !a.OnGround && dist > 500 && dt >= 3 && !math.IsNaN(a.Track) && !math.IsNaN(a.Velocity) && a.Velocity > 60 &&
		a.TimePos != prev.TimePos {
		flown := bearing(prev.Lon, prev.Lat, a.Lon, a.Lat)
		// A turning aircraft's path bearing sits between its old and new
		// tracks, so only flag when the new track is far from both.
		if off := angleDiff(a.Track, flown); off > d.opt.TrackDegrees && (math.IsNaN(prev.Track) || angleDiff(prev.Track, flown) < off/2) {
			detail := fmt.Sprintf("reports track %.0f° at %.0f km/h, but flew %.0f° at %.0f km/h", a.Track, a.Velocity*3.6, flown, speed*3.6)
			if old, ok := st.replayed(a, at); ok {
				detail += fmt.Sprintf("; the reported vector is the one it sent at %s", clock(old))
			}
			d.add(Anomaly{Kind: TrackMismatch, ICAO24: a.ICAO24, Callsign: a.Callsign, Start: at, Detail: detail})
		}
	}
}

// replayed reports whether the aircraft's velocity vector exactly repeats one
// it sent at least a minute ago, with different values in between.
func (s *state) replayed(a model.Aircraft, at int64) (int64, bool) {
	for i := len(s.prev) - 2; i >= 0; i-- {
		v := s.prev[i]
		if at-v.at >= 60000 && v.velocity == a.Velocity && v.track == a.Track && (v.vrate == a.VRate || (math.IsNaN(v.vrate) && math.IsNaN(a.VRate))) {
			return v.at, true
		}
	}
	return 0, false
}

// returned reports whether a has come back to where the aircraft would be
// had it kept flying from its last good state.
func (d *Detector) returned(p *pendingJump, a model.Aircraft, at int64) bool {
	elapsed := float64(at-p.fromAt) / 1000
	v := zeroNaN(p.from.Velocity)
	lon, lat := p.from.Lon, p.from.Lat
	if !math.IsNaN(p.from.Track) {
		lon, lat = advance(lon, lat, p.from.Track, v*elapsed)
	}
	// Allow for turns and speed changes since: it may be anywhere it could
	// have flown to, plus a margin.
	allowed := 10000 + math.Max(v, 100)*elapsed*1.5
	return distance(lon, lat, a.Lon, a.Lat) <= allowed
}

func zeroNaN(f float64) float64 {
	if math.IsNaN(f) {
		return 0
	}
	return f
}

// Result returns the findings in time order.
func (d *Detector) Result() []Anomaly {
	out := append([]Anomaly(nil), d.found...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}
