// Package scope turns what clients are looking at into the cheapest set of
// OpenSky bounding-box queries.
package scope

import (
	"math"
	"sort"
)

// MaxCost is the most a single poll may cost, and the cost of a global query.
const MaxCost = 4

// MaxPad is the largest fraction a box is padded by on each side.
const MaxPad = 0.2

// hysteresis is how far below a tier boundary a view must shrink before the
// plan drops to the cheaper tier.
const hysteresis = 0.9

// Box is a latitude and longitude bounding box in degrees. It never crosses
// the antimeridian.
type Box struct {
	LaMin float64 `json:"lamin"`
	LoMin float64 `json:"lomin"`
	LaMax float64 `json:"lamax"`
	LoMax float64 `json:"lomax"`
}

// Area is the box area in square degrees, as OpenSky measures it.
func (b Box) Area() float64 { return (b.LaMax - b.LaMin) * (b.LoMax - b.LoMin) }

// Contains reports whether o lies inside b.
func (b Box) Contains(o Box) bool {
	return o.LaMin >= b.LaMin && o.LaMax <= b.LaMax && o.LoMin >= b.LoMin && o.LoMax <= b.LoMax
}

// View is the rectangle one client sees. West greater than East means the
// view crosses the antimeridian. Global means the whole globe is in view.
type View struct {
	West   float64 `json:"west"`
	South  float64 `json:"south"`
	East   float64 `json:"east"`
	North  float64 `json:"north"`
	Global bool    `json:"global,omitempty"`
}

// Plan is the set of queries for one poll.
type Plan struct {
	Global bool  `json:"global"`
	Boxes  []Box `json:"boxes,omitempty"`
	Cost   int   `json:"cost"`
}

// GlobalPlan is the whole-world query.
func GlobalPlan() Plan { return Plan{Global: true, Cost: MaxCost} }

// Equal reports whether two plans make the same queries.
func (p Plan) Equal(o Plan) bool {
	if p.Global != o.Global || len(p.Boxes) != len(o.Boxes) {
		return false
	}
	for i := range p.Boxes {
		if p.Boxes[i] != o.Boxes[i] {
			return false
		}
	}
	return true
}

// Covers reports whether every box in boxes lies inside one of the plan's.
func (p Plan) Covers(boxes []Box) bool {
	if p.Global {
		return true
	}
	for _, b := range boxes {
		ok := false
		for _, pb := range p.Boxes {
			if pb.Contains(b) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

var tierLimits = [3]float64{25, 100, 400}

// Cost is the credit cost of a query for an area in square degrees.
func Cost(area float64) int { return costScaled(area, 1) }

func costScaled(area, scale float64) int {
	for i, lim := range tierLimits {
		if area <= lim*scale {
			return i + 1
		}
	}
	return MaxCost
}

// Boxes converts a view to one or two unpadded boxes. ok is false when the
// view needs a global query.
func (v View) Boxes() (boxes []Box, ok bool) {
	if v.Global || anyNaN(v.West, v.South, v.East, v.North) {
		return nil, false
	}
	s, n := clamp(v.South, -90, 90), clamp(v.North, -90, 90)
	if n <= s {
		return nil, false
	}
	w, e := clamp(v.West, -180, 180), clamp(v.East, -180, 180)
	if w == e {
		return nil, false
	}
	if w < e {
		return []Box{{s, w, n, e}}, true
	}
	// Crosses the antimeridian: split in two.
	var out []Box
	if w < 180 {
		out = append(out, Box{s, w, n, 180})
	}
	if e > -180 {
		out = append(out, Box{s, -180, n, e})
	}
	return out, len(out) > 0
}

// Pad grows a box by up to MaxPad on each side without raising its tier.
func Pad(b Box) Box {
	a := b.Area()
	if a <= 0 {
		return b
	}
	f := MaxPad
	if t := Cost(a); t < MaxCost {
		// Padded area is a*(1+2f)^2 and must stay at or under the tier limit.
		if lim := (math.Sqrt(tierLimits[t-1]/a) - 1) / 2; lim < f {
			f = lim
		}
	}
	// Step back a hair so rounding cannot push the area over the limit.
	f = math.Max(0, f-1e-9)
	dh, dw := (b.LaMax-b.LaMin)*f, (b.LoMax-b.LoMin)*f
	return Box{
		LaMin: clamp(b.LaMin-dh, -90, 90), LoMin: clamp(b.LoMin-dw, -180, 180),
		LaMax: clamp(b.LaMax+dh, -90, 90), LoMax: clamp(b.LoMax+dw, -180, 180),
	}
}

// Build makes the cheapest plan that covers every view. prev is the plan in
// use; it is kept when the views still fit inside it and have not shrunk far
// enough below a tier boundary to justify a change.
func Build(views []View, prev *Plan) Plan {
	if len(views) == 0 {
		return Plan{}
	}
	var raw []Box
	for _, v := range views {
		bs, ok := v.Boxes()
		if !ok {
			return GlobalPlan()
		}
		raw = append(raw, bs...)
	}
	plan := cheapest(raw, 1)
	if prev != nil && !prev.Global && len(prev.Boxes) > 0 && plan.Cost < prev.Cost &&
		prev.Covers(raw) && cheapest(raw, hysteresis).Cost >= prev.Cost {
		return *prev
	}
	return plan
}

// cheapest picks between separate boxes, one enclosing box, and global.
// scale shrinks the tier limits, for the hysteresis test.
func cheapest(raw []Box, scale float64) Plan {
	sep := dedupe(raw)
	sepCost := 0
	for _, b := range sep {
		sepCost += costScaled(b.Area(), scale)
	}
	enc := sep[0]
	for _, b := range sep[1:] {
		enc.LaMin, enc.LoMin = math.Min(enc.LaMin, b.LaMin), math.Min(enc.LoMin, b.LoMin)
		enc.LaMax, enc.LoMax = math.Max(enc.LaMax, b.LaMax), math.Max(enc.LoMax, b.LoMax)
	}
	encCost := costScaled(enc.Area(), scale)

	// On a tie, one request beats several.
	boxes, cost := sep, sepCost
	if encCost <= sepCost {
		boxes, cost = []Box{enc}, encCost
	}
	if cost >= MaxCost {
		return GlobalPlan()
	}
	out := make([]Box, len(boxes))
	for i, b := range boxes {
		out[i] = Pad(b)
	}
	return Plan{Boxes: out, Cost: cost}
}

// dedupe drops boxes that lie inside another and sorts the rest.
func dedupe(in []Box) []Box {
	var out []Box
	for i, b := range in {
		inside := false
		for j, o := range in {
			if i != j && o.Contains(b) && (o != b || j < i) {
				inside = true
				break
			}
		}
		if !inside {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.LoMin != b.LoMin {
			return a.LoMin < b.LoMin
		}
		return a.LaMin < b.LaMin
	})
	return out
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

func anyNaN(vs ...float64) bool {
	for _, v := range vs {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return true
		}
	}
	return false
}
