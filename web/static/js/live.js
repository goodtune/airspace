// The live view: fetches each new snapshot and dead-reckons aircraft
// between them.

import { F, deadReckon, angleDiff, wrapLon } from './geo.js';

const EASE_SECONDS = 1.2;

export class Live {
  constructor(layer, clock) {
    this.layer = layer;
    this.clock = clock; // () => server time in ms
    this.aircraft = new Map(); // icao24 -> {a, err, easeStart}
    this.version = 0;
    this.etag = '';
    this.fetching = false;
    this.wanted = 0;
    this.snapshot = null;
  }

  // Called when the snapshot.version signal changes.
  async onVersion(version) {
    this.wanted = Math.max(this.wanted, version);
    if (this.fetching) return;
    this.fetching = true;
    try {
      while (this.version < this.wanted) {
        const resp = await fetch('/api/states', { headers: this.etag ? { 'If-None-Match': this.etag } : {} });
        if (resp.status === 304) break;
        if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
        this.etag = resp.headers.get('ETag') ?? '';
        const snap = await resp.json();
        if (snap.version <= this.version) break;
        this.ingest(snap);
      }
    } catch (err) {
      console.warn('fetching /api/states failed', err);
    } finally {
      this.fetching = false;
    }
  }

  ingest(snap) {
    const now = this.clock() / 1000;
    for (const a of snap.aircraft) {
      const id = a[F.icao];
      const old = this.aircraft.get(id);
      let err = null;
      if (old) {
        // Keep the aircraft where it is drawn now, and ease out the
        // difference from where the new data says it should be.
        const shown = this.position(old, now), fresh = deadReckon(a, now);
        if (shown && fresh) {
          err = { lon: angleDiff(fresh.lon, shown.lon), lat: shown.lat - fresh.lat, alt: shown.alt - fresh.alt };
          if (Math.abs(err.lon) > 1 || Math.abs(err.lat) > 1) err = null; // a jump, not a correction
        }
      }
      this.aircraft.set(id, { a, err, easeStart: now });
    }
    this.version = snap.version;
    this.snapshot = snap;
  }

  position(rec, now) {
    const p = deadReckon(rec.a, now);
    if (!p || !rec.err) return p;
    const t = (now - rec.easeStart) / EASE_SECONDS;
    if (t >= 1) {
      rec.err = null;
      return p;
    }
    const k = (1 - t) ** 3; // ease-out cubic
    p.lon = wrapLon(p.lon + rec.err.lon * k);
    p.lat += rec.err.lat * k;
    p.alt += rec.err.alt * k;
    return p;
  }

  frame() {
    const now = this.clock() / 1000;
    this.layer.begin();
    for (const [id, rec] of this.aircraft) {
      const p = this.position(rec, now);
      if (!p) {
        this.aircraft.delete(id); // not heard from for five minutes
        continue;
      }
      this.layer.place(id, p.lon, p.lat, p.alt, p.track, p.stale ? 0.4 : 1, p.ground, rec.a);
    }
    this.layer.end();
  }

  get(id) {
    return this.aircraft.get(id)?.a ?? null;
  }
}
