package scope

import (
	"math/rand"
	"testing"
)

func view(w, s, e, n float64) View { return View{West: w, South: s, East: e, North: n} }

func TestCostTiers(t *testing.T) {
	for _, c := range []struct {
		area float64
		want int
	}{{1, 1}, {25, 1}, {25.01, 2}, {100, 2}, {100.01, 3}, {400, 3}, {400.01, 4}, {64800, 4}} {
		if got := Cost(c.area); got != c.want {
			t.Errorf("Cost(%v) = %d, want %d", c.area, got, c.want)
		}
	}
}

func TestPaddingNeverRaisesTier(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		la, lo := r.Float64()*170-85, r.Float64()*340-170
		h, w := r.Float64()*25+0.01, r.Float64()*25+0.01
		b := Box{la, lo, min(la+h, 90), min(lo+w, 180)}
		p := Pad(b)
		if Cost(p.Area()) != Cost(b.Area()) {
			t.Fatalf("padding raised tier: %v (%d) -> %v (%d)", b, Cost(b.Area()), p, Cost(p.Area()))
		}
		if !p.Contains(b) {
			t.Fatalf("padded box %v does not contain %v", p, b)
		}
	}
}

func TestPaddingFullWhenRoom(t *testing.T) {
	p := Pad(Box{0, 0, 2, 2}) // 4 sq deg; 20% a side gives 2.8 x 2.8
	if p.LaMin > -0.39 || p.LoMax < 2.39 {
		t.Errorf("expected about 20%% padding, got %v", p)
	}
	// Right at the boundary there is no room.
	p = Pad(Box{0, 0, 5, 5})
	if p.Area() > 25 {
		t.Errorf("padded area %v exceeds tier", p.Area())
	}
}

func TestSingleViewTiers(t *testing.T) {
	for _, c := range []struct {
		v    View
		cost int
	}{
		{view(0, 0, 4, 4), 1},
		{view(0, 0, 9, 9), 2},
		{view(0, 0, 19, 19), 3},
	} {
		p := Build([]View{c.v}, nil)
		if p.Global || p.Cost != c.cost || len(p.Boxes) != 1 {
			t.Errorf("%v: got %+v, want cost %d", c.v, p, c.cost)
		}
	}
	if p := Build([]View{view(0, 0, 21, 21)}, nil); !p.Global || p.Cost != 4 {
		t.Errorf("large view should be global, got %+v", p)
	}
	if p := Build([]View{{Global: true}}, nil); !p.Global {
		t.Errorf("global view should be global, got %+v", p)
	}
	if p := Build(nil, nil); p.Cost != 0 || p.Global {
		t.Errorf("no views should be an empty plan, got %+v", p)
	}
}

func TestHysteresis(t *testing.T) {
	big := Build([]View{view(0, 0, 5.2, 5.2)}, nil) // 27 sq deg, tier 2
	if big.Cost != 2 {
		t.Fatalf("want tier 2, got %+v", big)
	}
	// Shrinks just under the boundary: plan is kept.
	p := Build([]View{view(0.1, 0.1, 5.05, 5.05)}, &big) // 24.5 sq deg
	if !p.Equal(big) || p.Cost != 2 {
		t.Errorf("wobble under boundary flipped tier: %+v", p)
	}
	// Shrinks well under: tier drops.
	p = Build([]View{view(0.5, 0.5, 4.5, 4.5)}, &big) // 16 sq deg
	if p.Cost != 1 {
		t.Errorf("want tier 1 after real zoom in, got %+v", p)
	}
	// Moves outside the old plan: no stickiness.
	p = Build([]View{view(40, 40, 44.9, 44.9)}, &big)
	if p.Cost != 1 {
		t.Errorf("moved view should be re-planned, got %+v", p)
	}
	// Growing always applies at once.
	small := Build([]View{view(0, 0, 4.9, 4.9)}, nil)
	p = Build([]View{view(0, 0, 5.1, 5.1)}, &small)
	if p.Cost != 2 {
		t.Errorf("growth must raise tier, got %+v", p)
	}
}

func TestAntimeridianSplit(t *testing.T) {
	p := Build([]View{view(178, -2, -178, 2)}, nil)
	if p.Global || len(p.Boxes) != 2 {
		t.Fatalf("want two boxes, got %+v", p)
	}
	if p.Boxes[0].LoMin != -180 || p.Boxes[1].LoMax != 180 {
		t.Errorf("boxes must stop at the antimeridian: %+v", p.Boxes)
	}
	if p.Cost != 2 {
		t.Errorf("want cost 2 (1+1), got %d", p.Cost)
	}
	for _, b := range p.Boxes {
		if b.LoMin < -180 || b.LoMax > 180 || b.LoMin >= b.LoMax {
			t.Errorf("bad box %v", b)
		}
	}
}

func TestPole(t *testing.T) {
	p := Build([]View{view(10, 88, 20, 95)}, nil)
	if p.Global || len(p.Boxes) != 1 || p.Boxes[0].LaMax != 90 {
		t.Fatalf("want one box up to the pole, got %+v", p)
	}
	p = Build([]View{view(-30, -99, -20, -89)}, nil)
	if p.Global || p.Boxes[0].LaMin != -90 {
		t.Fatalf("want one box down to the pole, got %+v", p)
	}
}

func TestPlanSelection(t *testing.T) {
	// Far apart: separate boxes (1+1) beat the enclosing box (tier 4).
	p := Build([]View{view(0, 0, 4, 4), view(100, 30, 104, 34)}, nil)
	if p.Global || len(p.Boxes) != 2 || p.Cost != 2 {
		t.Errorf("want separate boxes cost 2, got %+v", p)
	}
	// Overlapping: one enclosing box (tier 1) beats two (1+1).
	p = Build([]View{view(0, 0, 3, 3), view(1, 1, 4, 4)}, nil)
	if p.Global || len(p.Boxes) != 1 || p.Cost != 1 {
		t.Errorf("want one enclosing box cost 1, got %+v", p)
	}
	// Three far-apart tier-2 views: 6 credits, so global.
	p = Build([]View{view(0, 0, 9, 9), view(50, 0, 59, 9), view(100, 0, 109, 9)}, nil)
	if !p.Global || p.Cost != 4 {
		t.Errorf("want global, got %+v", p)
	}
	// A detached client inside the shared view adds nothing.
	p = Build([]View{view(0, 0, 4, 4), view(1, 1, 2, 2)}, nil)
	if len(p.Boxes) != 1 || p.Cost != 1 {
		t.Errorf("contained view should not add a box, got %+v", p)
	}
}

func TestCostNeverExceedsFour(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	var prev *Plan
	for i := 0; i < 20000; i++ {
		n := r.Intn(5) + 1
		vs := make([]View, n)
		for j := range vs {
			w, s := r.Float64()*360-180, r.Float64()*180-90
			vs[j] = view(w, s, w+r.Float64()*40-5, s+r.Float64()*30)
			if vs[j].East > 180 {
				vs[j].East -= 360
			}
		}
		p := Build(vs, prev)
		sum := 0
		for _, b := range p.Boxes {
			sum += Cost(b.Area())
		}
		if p.Global {
			sum = 4
		}
		if p.Cost > MaxCost || sum > MaxCost || sum > p.Cost {
			t.Fatalf("views %v: plan %+v real cost %d", vs, p, sum)
		}
		prev = &p
	}
}
