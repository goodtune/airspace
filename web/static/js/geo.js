// Pure geometry and aircraft motion. No browser or Cesium dependency, so it
// runs under `node --test` as well.

export const EARTH_RADIUS = 6371008.8; // metres
const RAD = Math.PI / 180;
const DEG = 180 / Math.PI;

// Positions of the fields in an aircraft array. Must match model.Fields.
export const F = {
  icao: 0, callsign: 1, country: 2, lon: 3, lat: 4, baro: 5, geo: 6, ground: 7,
  velocity: 8, track: 9, vrate: 10, squawk: 11, timePos: 12, lastContact: 13,
};

export const STOP_EXTRAPOLATING_AFTER = 60; // seconds since last contact
export const REMOVE_AFTER = 300;

export function wrapLon(lon) {
  return ((((lon + 180) % 360) + 360) % 360) - 180;
}

// Shortest signed difference b - a between two angles in degrees.
export function angleDiff(a, b) {
  return ((((b - a) % 360) + 540) % 360) - 180;
}

export function lerpAngle(a, b, f) {
  return (((a + angleDiff(a, b) * f) % 360) + 360) % 360;
}

// Best available altitude in metres: geometric, then barometric, then 0.
export function altitudeOf(a) {
  if (a[F.ground]) return 0;
  const alt = a[F.geo] ?? a[F.baro] ?? 0;
  return alt > 0 ? alt : 0;
}

// Move from a point at a speed (m/s) along a track (degrees) for dt seconds.
// Returns [lon, lat] in degrees.
export function advance(lon, lat, speed, track, dt) {
  const dist = speed * dt;
  if (!(dist > 0) || track == null) return [lon, lat];
  if (Math.abs(lat) < 80 && dist < 200000) {
    // Flat approximation: accurate to metres over the distances flown
    // between snapshots, and much cheaper than the exact form.
    const t = track * RAD;
    const dLat = (dist * Math.cos(t)) / EARTH_RADIUS;
    const dLon = (dist * Math.sin(t)) / (EARTH_RADIUS * Math.cos(lat * RAD));
    return [wrapLon(lon + dLon * DEG), lat + dLat * DEG];
  }
  const d = dist / EARTH_RADIUS, t = track * RAD, p1 = lat * RAD, l1 = lon * RAD;
  const p2 = Math.asin(Math.sin(p1) * Math.cos(d) + Math.cos(p1) * Math.sin(d) * Math.cos(t));
  const l2 = l1 + Math.atan2(Math.sin(t) * Math.sin(d) * Math.cos(p1), Math.cos(d) - Math.sin(p1) * Math.sin(p2));
  return [wrapLon(l2 * DEG), p2 * DEG];
}

// Where a live aircraft is at time now (unix seconds), by dead reckoning from
// its last reported position. Returns null once it is due for removal.
export function deadReckon(a, now) {
  const contact = a[F.lastContact] || a[F.timePos] || now;
  const age = now - contact;
  if (age > REMOVE_AFTER) return null;
  const from = a[F.timePos] || contact;
  // Past the cut-off the aircraft holds the last extrapolated position.
  const until = Math.min(now, contact + STOP_EXTRAPOLATING_AFTER);
  const dt = Math.max(0, until - from);
  const stale = age > STOP_EXTRAPOLATING_AFTER;
  const ground = a[F.ground];
  const [lon, lat] = advance(a[F.lon], a[F.lat], a[F.velocity] ?? 0, a[F.track], dt);
  let alt = altitudeOf(a);
  if (!ground && a[F.vrate]) alt = Math.max(0, alt + a[F.vrate] * dt);
  return { lon, lat, alt, track: a[F.track] ?? 0, stale, ground };
}

function toVec(lon, lat) {
  const l = lon * RAD, p = lat * RAD, c = Math.cos(p);
  return [c * Math.cos(l), c * Math.sin(l), Math.sin(p)];
}

// Point a fraction f of the way along the great circle between two points.
export function greatCircle(lon1, lat1, lon2, lat2, f) {
  const a = toVec(lon1, lat1), b = toVec(lon2, lat2);
  const dot = Math.min(1, Math.max(-1, a[0] * b[0] + a[1] * b[1] + a[2] * b[2]));
  const omega = Math.acos(dot);
  let x, y, z;
  if (omega < 1e-9) {
    [x, y, z] = a;
  } else {
    const s = Math.sin(omega), ka = Math.sin((1 - f) * omega) / s, kb = Math.sin(f * omega) / s;
    x = ka * a[0] + kb * b[0]; y = ka * a[1] + kb * b[1]; z = ka * a[2] + kb * b[2];
  }
  return [Math.atan2(y, x) * DEG, Math.atan2(z, Math.hypot(x, y)) * DEG];
}

// Index a snapshot's aircraft by icao24.
export function indexSnapshot(snap) {
  const m = new Map();
  for (const a of snap.aircraft) m.set(a[F.icao], a);
  return m;
}

// Pair up the aircraft of two neighbouring snapshots for replay. Each entry
// is {id, a, b}: a is null when the aircraft appears only in the second
// snapshot, b is null when only in the first.
export function pairSnapshots(first, second) {
  const out = [];
  const bIndex = second ? indexSnapshot(second) : new Map();
  for (const a of first.aircraft) {
    const id = a[F.icao];
    out.push({ id, a, b: bIndex.get(id) ?? null });
    bIndex.delete(id);
  }
  for (const [id, b] of bIndex) out.push({ id, a: null, b });
  return out;
}

// Where a paired aircraft is a fraction f of the way from the first snapshot
// to the second. Aircraft in both are moved along a great circle. Aircraft in
// only one hold their position and fade: out when leaving, in when arriving.
export function interpolate(pair, f) {
  const { a, b } = pair;
  if (a && b) {
    const [lon, lat] = greatCircle(a[F.lon], a[F.lat], b[F.lon], b[F.lat], f);
    const altA = altitudeOf(a), altB = altitudeOf(b);
    return {
      lon, lat, alt: altA + (altB - altA) * f,
      track: lerpAngle(a[F.track] ?? b[F.track] ?? 0, b[F.track] ?? a[F.track] ?? 0, f),
      alpha: 1, ground: f < 0.5 ? a[F.ground] : b[F.ground], data: f < 0.5 ? a : b,
    };
  }
  const only = a ?? b;
  return {
    lon: only[F.lon], lat: only[F.lat], alt: altitudeOf(only), track: only[F.track] ?? 0,
    alpha: a ? 1 - f : f, ground: only[F.ground], data: only,
  };
}

// Camera between two keyframes {lon, lat, height, heading, pitch}.
export function lerpCamera(a, b, f) {
  const s = f * f * (3 - 2 * f); // smoothstep
  const [lon, lat] = greatCircle(a.lon, a.lat, b.lon, b.lat, s);
  return {
    lon, lat,
    // Height is interpolated in log space so zooms feel even.
    height: Math.exp(Math.log(a.height) + (Math.log(b.height) - Math.log(a.height)) * s),
    heading: lerpAngle(a.heading, b.heading, s),
    pitch: a.pitch + (b.pitch - a.pitch) * s,
  };
}

// The camera at time t from a time-ordered list of {time, ...camera}. Before
// the first entry it is the first, after the last it is the last.
export function cameraAt(track, t, smooth = true) {
  if (!track.length) return null;
  if (t <= track[0].time) return track[0];
  const last = track[track.length - 1];
  if (t >= last.time) return last;
  let lo = 0, hi = track.length - 1;
  while (hi - lo > 1) {
    const mid = (lo + hi) >> 1;
    if (track[mid].time <= t) lo = mid; else hi = mid;
  }
  const a = track[lo], b = track[hi];
  const f = (t - a.time) / (b.time - a.time);
  if (smooth) return lerpCamera(a, b, f);
  const [lon, lat] = greatCircle(a.lon, a.lat, b.lon, b.lat, f);
  return {
    lon, lat, height: a.height + (b.height - a.height) * f,
    heading: lerpAngle(a.heading, b.heading, f), pitch: a.pitch + (b.pitch - a.pitch) * f,
  };
}

// The camera at time t on a recorded track. Live, the camera stays where it
// is until the next update arrives and then eases to it. So this holds each
// entry until shortly before the next one, instead of drifting between two
// entries that may be minutes apart.
export function recordedCameraAt(track, t, ease = 300) {
  if (!track.length) return null;
  if (t <= track[0].time) return track[0];
  const last = track[track.length - 1];
  if (t >= last.time) return last;
  let lo = 0, hi = track.length - 1;
  while (hi - lo > 1) {
    const mid = (lo + hi) >> 1;
    if (track[mid].time <= t) lo = mid; else hi = mid;
  }
  const a = track[lo], b = track[hi];
  const span = Math.min(ease, b.time - a.time);
  const f = (t - (b.time - span)) / span;
  if (f <= 0) return a;
  const [lon, lat] = greatCircle(a.lon, a.lat, b.lon, b.lat, f);
  return {
    lon, lat, height: a.height + (b.height - a.height) * f,
    heading: lerpAngle(a.heading, b.heading, f), pitch: a.pitch + (b.pitch - a.pitch) * f,
  };
}

// Whether a point is inside a scope plan ({global, boxes}).
export function inScope(plan, lon, lat) {
  if (!plan || plan.global) return true;
  return (plan.boxes ?? []).some((b) => lat >= b.lamin && lat <= b.lamax && lon >= b.lomin && lon <= b.lomax);
}
