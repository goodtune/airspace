import test from 'node:test';
import assert from 'node:assert/strict';
import * as geo from '../static/js/geo.js';

const near = (a, b, eps, msg) => assert.ok(Math.abs(a - b) <= eps, `${msg ?? ''} ${a} vs ${b}`);
// icao, callsign, country, lon, lat, baro, geo, ground, velocity, track, vrate, squawk, timePos, lastContact
const ac = (id, lon, lat, o = {}) => [id, 'CS', 'X', lon, lat, o.alt ?? 10000, o.alt ?? 10000, o.ground ?? false,
  o.v ?? 250, o.track ?? 90, o.vrate ?? 0, '', o.t ?? 1000, o.t ?? 1000];

test('advance moves east along the equator', () => {
  const [lon, lat] = geo.advance(0, 0, 250, 90, 60); // 15 km
  near(lon, (15000 / geo.EARTH_RADIUS) * 180 / Math.PI, 1e-6);
  near(lat, 0, 1e-9);
});

test('advance agrees between the flat and exact forms', () => {
  const flat = geo.advance(10, 79.9, 250, 37, 30);
  const exact = geo.advance(10, 80.1, 250, 37, 30);
  near(flat[1] - 79.9, exact[1] - 80.1, 1e-4, 'latitude step');
  // Crossing the antimeridian wraps.
  const [lon] = geo.advance(179.99, 0, 250, 90, 60);
  assert.ok(lon < -179.8 && lon > -180, `wrapped to ${lon}`);
});

test('dead reckoning extrapolates, then holds, then removes', () => {
  const a = ac('a', 0, 0, { t: 1000, vrate: 5 });
  const p10 = geo.deadReckon(a, 1010);
  near(p10.lon, geo.advance(0, 0, 250, 90, 10)[0], 1e-9);
  near(p10.alt, 10050, 1e-9);
  assert.equal(p10.stale, false);
  // Past 60 s it stops moving.
  const p60 = geo.deadReckon(a, 1060), p200 = geo.deadReckon(a, 1200);
  assert.equal(p200.stale, true);
  near(p200.lon, p60.lon, 1e-12, 'held position');
  // Past 5 minutes it is gone.
  assert.equal(geo.deadReckon(a, 1301), null);
});

test('aircraft on the ground sit at zero altitude', () => {
  assert.equal(geo.deadReckon(ac('g', 0, 0, { ground: true, v: 5 }), 1010).alt, 0);
});

test('great circle midpoint', () => {
  const [lon, lat] = geo.greatCircle(0, 0, 90, 0, 0.5);
  near(lon, 45, 1e-9); near(lat, 0, 1e-9);
  // Over the pole: the midpoint of two points at 80N on opposite meridians.
  const [, lat2] = geo.greatCircle(0, 80, 180, 80, 0.5);
  near(lat2, 90, 1e-6);
  // Across the antimeridian the short way.
  const [lon3] = geo.greatCircle(170, 0, -170, 0, 0.5);
  near(Math.abs(lon3), 180, 1e-9);
});

test('interpolation: aircraft in both snapshots follows the great circle', () => {
  const first = { aircraft: [ac('both', 0, 0, { alt: 1000, track: 350 })] };
  const second = { aircraft: [ac('both', 10, 0, { alt: 2000, track: 10 })] };
  const [pair] = geo.pairSnapshots(first, second);
  const p = geo.interpolate(pair, 0.25);
  near(p.lon, 2.5, 1e-9); near(p.lat, 0, 1e-9); near(p.alt, 1250, 1e-9);
  near(p.track, 355, 1e-9, 'track goes the short way through north');
  assert.equal(p.alpha, 1);
  near(geo.interpolate(pair, 0).lon, 0, 1e-9);
  near(geo.interpolate(pair, 1).lon, 10, 1e-9);
});

test('interpolation: aircraft only in the first snapshot fades out in place', () => {
  const pairs = geo.pairSnapshots({ aircraft: [ac('gone', 5, 5)] }, { aircraft: [] });
  assert.equal(pairs.length, 1);
  assert.equal(pairs[0].b, null);
  const p = geo.interpolate(pairs[0], 0.25);
  assert.deepEqual([p.lon, p.lat], [5, 5]);
  near(p.alpha, 0.75, 1e-12);
  near(geo.interpolate(pairs[0], 1).alpha, 0, 1e-12);
});

test('interpolation: aircraft only in the second snapshot fades in in place', () => {
  const pairs = geo.pairSnapshots({ aircraft: [] }, { aircraft: [ac('new', 7, 8)] });
  assert.equal(pairs[0].a, null);
  const p = geo.interpolate(pairs[0], 0.25);
  assert.deepEqual([p.lon, p.lat], [7, 8]);
  near(p.alpha, 0.25, 1e-12);
});

test('pairing keeps every aircraft exactly once', () => {
  const first = { aircraft: [ac('a', 0, 0), ac('b', 1, 1)] };
  const second = { aircraft: [ac('b', 2, 2), ac('c', 3, 3)] };
  const ids = geo.pairSnapshots(first, second).map((p) => p.id).sort();
  assert.deepEqual(ids, ['a', 'b', 'c']);
  // The last snapshot of a session has no neighbour.
  assert.equal(geo.pairSnapshots(first, null).length, 2);
});

test('camera track: replay follows the recorded cameras in order', () => {
  const track = [
    { time: 0, lon: 0, lat: 0, height: 1e6, heading: 0, pitch: -90 },
    { time: 10, lon: 10, lat: 0, height: 1e6, heading: 90, pitch: -45 },
    { time: 20, lon: 10, lat: 10, height: 4e6, heading: 90, pitch: -45 },
  ];
  assert.equal(geo.cameraAt(track, -5), track[0]);
  assert.equal(geo.cameraAt(track, 99), track[2]);
  const mid = geo.cameraAt(track, 5, false);
  near(mid.lon, 5, 1e-9); near(mid.heading, 45, 1e-9); near(mid.pitch, -67.5, 1e-9);
  const later = geo.cameraAt(track, 15, false);
  near(later.lat, 5, 1e-6); near(later.height, 2.5e6, 1e-6);
  assert.equal(geo.cameraAt([], 5), null);
  // Smooth keyframe interpolation still hits the keyframes exactly.
  near(geo.lerpCamera(track[0], track[1], 1).lon, 10, 1e-9);
  near(geo.lerpCamera(track[0], track[1], 0).heading, 0, 1e-9);
});

test('recorded camera holds between sparse updates, then eases', () => {
  const track = [
    { time: 0, lon: 0, lat: 0, height: 1e6, heading: 0, pitch: -90 },
    { time: 60000, lon: 10, lat: 0, height: 1e6, heading: 0, pitch: -90 }, // a move a minute later
    { time: 60200, lon: 11, lat: 0, height: 1e6, heading: 0, pitch: -90 }, // and a quick follow-up
  ];
  // For most of the minute the camera has not moved at all.
  assert.equal(geo.recordedCameraAt(track, 30000), track[0]);
  assert.equal(geo.recordedCameraAt(track, 59700), track[0]);
  // It eases in over the last 300 ms.
  near(geo.recordedCameraAt(track, 59850).lon, 5, 1e-6);
  near(geo.recordedCameraAt(track, 60000).lon, 10, 1e-9);
  // Updates closer together than the ease are joined directly.
  near(geo.recordedCameraAt(track, 60100).lon, 10.5, 1e-6);
  assert.equal(geo.recordedCameraAt(track, 99999), track[2]);
  assert.equal(geo.recordedCameraAt([], 1), null);
});

test('scope containment', () => {
  const plan = { global: false, boxes: [{ lamin: 0, lomin: 0, lamax: 5, lomax: 5 }] };
  assert.equal(geo.inScope(plan, 2, 2), true);
  assert.equal(geo.inScope(plan, 6, 2), false);
  assert.equal(geo.inScope({ global: true }, 100, 80), true);
});
