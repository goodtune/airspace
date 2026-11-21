// The aircraft layer: one billboard collection shared by live view and
// replay. Each frame the owner calls begin(), place() for every aircraft,
// then end(), which removes any that were not placed.

const C = window.Cesium;

function makeIcon() {
  const s = 64, c = document.createElement('canvas');
  c.width = c.height = s;
  const g = c.getContext('2d');
  g.translate(s / 2, s / 2);
  g.beginPath(); // a simple airliner silhouette, nose up
  g.moveTo(0, -28); g.quadraticCurveTo(5, -22, 5, -10); g.lineTo(28, 8); g.lineTo(28, 14); g.lineTo(5, 6);
  g.lineTo(4, 20); g.lineTo(12, 26); g.lineTo(12, 30); g.lineTo(0, 27);
  g.lineTo(-12, 30); g.lineTo(-12, 26); g.lineTo(-4, 20); g.lineTo(-5, 6); g.lineTo(-28, 14); g.lineTo(-28, 8);
  g.lineTo(-5, -10); g.quadraticCurveTo(-5, -22, 0, -28); g.closePath();
  g.fillStyle = '#fff'; g.fill();
  g.lineWidth = 3; g.strokeStyle = 'rgba(0,0,0,0.75)'; g.stroke();
  g.fill();
  return c;
}

// Colour ramp by altitude: warm near the ground, cool at cruise.
const STOPS = [[0, [255, 196, 92]], [3000, [255, 236, 130]], [7000, [140, 240, 170]], [11000, [95, 208, 255]], [14000, [190, 150, 255]]];
const BUCKETS = 24, ALPHAS = 8;

function ramp(alt) {
  for (let i = 1; i < STOPS.length; i++) {
    if (alt <= STOPS[i][0]) {
      const [a0, c0] = STOPS[i - 1], [a1, c1] = STOPS[i], f = (alt - a0) / (a1 - a0);
      return c0.map((v, k) => (v + (c1[k] - v) * f) / 255);
    }
  }
  return STOPS[STOPS.length - 1][1].map((v) => v / 255);
}

export class AircraftLayer {
  constructor(viewer) {
    this.viewer = viewer;
    this.billboards = viewer.scene.primitives.add(new C.BillboardCollection({ scene: viewer.scene }));
    this.labels = viewer.scene.primitives.add(new C.LabelCollection({ scene: viewer.scene }));
    this.icon = makeIcon();
    this.items = new Map(); // icao24 -> {bb, frame, colorKey}
    this.frame = 0;
    this.exaggeration = 1;
    this.selected = '';
    this.selectedPos = null; // {lon, lat, alt, track, data} of the selected aircraft this frame
    this.scratch = new C.Cartesian3();
    this.scale = new C.NearFarScalar(3e5, 0.42, 1.2e7, 0.16);
    this.colors = [];
    for (let b = 0; b <= BUCKETS; b++) {
      const [r, g, bl] = ramp((b / BUCKETS) * 14000);
      this.colors.push(Array.from({ length: ALPHAS + 1 }, (_, a) => new C.Color(r, g, bl, a / ALPHAS)));
    }
    this.groundColors = Array.from({ length: ALPHAS + 1 }, (_, a) => new C.Color(0.62, 0.66, 0.72, (a / ALPHAS) * 0.9));
    this.selectedColor = C.Color.fromCssColorString('#ff5d8f');
    this.label = this.labels.add({
      show: false, font: '600 13px ui-monospace, Menlo, monospace', fillColor: C.Color.WHITE,
      outlineColor: C.Color.BLACK, outlineWidth: 3, style: C.LabelStyle.FILL_AND_OUTLINE,
      pixelOffset: new C.Cartesian2(14, -14), disableDepthTestDistance: Number.POSITIVE_INFINITY,
    });
  }

  get count() { return this.items.size; }

  begin() {
    this.frame++;
    this.selectedPos = null;
  }

  place(id, lon, lat, alt, track, alpha, ground, data) {
    let it = this.items.get(id);
    if (!it) {
      const bb = this.billboards.add({ id, alignedAxis: C.Cartesian3.UNIT_Z, scaleByDistance: this.scale });
      // One shared image id: every aircraft uses the same entry in the
      // texture atlas, not a copy each.
      bb.setImage('airspace-aircraft', this.icon);
      it = { bb, frame: 0, colorKey: -1, track: NaN };
      this.items.set(id, it);
    }
    it.frame = this.frame;
    const bb = it.bb;
    bb.position = C.Cartesian3.fromDegrees(lon, lat, alt * this.exaggeration, undefined, this.scratch);
    if (track !== it.track) {
      it.track = track;
      bb.rotation = -track * (Math.PI / 180);
    }
    const isSel = id === this.selected;
    const a = Math.max(0, Math.min(ALPHAS, Math.round(alpha * ALPHAS)));
    const bucket = ground ? -1 : Math.max(0, Math.min(BUCKETS, Math.round((alt / 14000) * BUCKETS)));
    const key = isSel ? 1e6 + a : (bucket + 2) * 16 + a;
    if (key !== it.colorKey) {
      it.colorKey = key;
      bb.color = isSel ? this.selectedColor.withAlpha(Math.max(0.35, alpha)) : ground ? this.groundColors[a] : this.colors[bucket][a];
      bb.scale = isSel ? 1.8 : 1;
    }
    if (isSel) this.selectedPos = { lon, lat, alt, track, data };
  }

  end() {
    for (const [id, it] of this.items) {
      if (it.frame !== this.frame) {
        this.billboards.remove(it.bb);
        this.items.delete(id);
      }
    }
    const s = this.selectedPos;
    this.label.show = !!s;
    if (s) {
      this.label.position = C.Cartesian3.fromDegrees(s.lon, s.lat, s.alt * this.exaggeration);
      this.label.text = (s.data && (s.data[1] || s.data[0])) || this.selected;
    }
  }

  // Whether every aircraft is ready to draw. A new billboard takes its
  // image asynchronously, so it can miss the first frame after it is made.
  ready() {
    for (const it of this.items.values()) if (!it.bb.ready) return false;
    return true;
  }

  clear() {
    this.billboards.removeAll();
    this.items.clear();
    this.label.show = false;
    this.selectedPos = null;
  }

  setSelected(id) {
    this.selected = id || '';
    for (const it of this.items.values()) it.colorKey = -1; // repaint
  }

  // The icao24 of the aircraft under a screen position, or ''.
  pick(position) {
    const hit = this.viewer.scene.pick(position, 9, 9);
    return hit && hit.collection === this.billboards && typeof hit.id === 'string' ? hit.id : '';
  }
}

// ScopeOutline draws the edges of the queried boxes on the ground, so an
// empty area reads as "not queried".
export class ScopeOutline {
  constructor(viewer) {
    this.lines = viewer.scene.primitives.add(new C.PolylineCollection());
    this.key = '';
    this.material = C.Material.fromType('PolylineDash', { color: C.Color.fromCssColorString('#5fd0ff').withAlpha(0.9), dashLength: 14 });
    this.show = true;
  }

  set(plan) {
    const boxes = plan && !plan.global && this.show ? plan.boxes ?? [] : [];
    const key = JSON.stringify(boxes);
    if (key === this.key) return;
    this.key = key;
    this.lines.removeAll();
    for (const b of boxes) {
      const pts = [];
      const edge = (lon0, lat0, lon1, lat1) => {
        // Sample each edge so it follows the parallel or meridian.
        const n = Math.max(2, Math.ceil(Math.max(Math.abs(lon1 - lon0), Math.abs(lat1 - lat0)) * 2));
        for (let i = 0; i < n; i++) pts.push(lon0 + ((lon1 - lon0) * i) / n, lat0 + ((lat1 - lat0) * i) / n);
      };
      edge(b.lomin, b.lamin, b.lomax, b.lamin);
      edge(b.lomax, b.lamin, b.lomax, b.lamax);
      edge(b.lomax, b.lamax, b.lomin, b.lamax);
      edge(b.lomin, b.lamax, b.lomin, b.lamin);
      pts.push(b.lomin, b.lamin);
      this.lines.add({ positions: C.Cartesian3.fromDegreesArray(pts), width: 2, material: this.material });
    }
  }
}
