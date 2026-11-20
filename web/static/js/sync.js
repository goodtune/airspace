// Talking to the server: actions, and keeping the camera in step with the
// shared one without ever echoing a move back.

const C = window.Cesium;
const DEG = 180 / Math.PI;

let clientId = '';
export function setClientId(id) { clientId = id; }
export function getClientId() { return clientId; }

// act posts a change to shared state. It resolves to true when accepted.
export async function act(name, body = {}) {
  if (!clientId) return false;
  try {
    const resp = await fetch(`/api/action/${name}`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ...body, clientId }), keepalive: true,
    });
    return resp.ok;
  } catch {
    return false;
  }
}

export function readCamera(camera) {
  const p = camera.positionCartographic;
  return {
    lon: p.longitude * DEG, lat: p.latitude * DEG, height: p.height,
    heading: camera.heading * DEG, pitch: camera.pitch * DEG,
  };
}

export function applyCamera(camera, cam) {
  camera.setView({
    destination: C.Cartesian3.fromDegrees(cam.lon, cam.lat, cam.height),
    orientation: { heading: cam.heading / DEG, pitch: cam.pitch / DEG, roll: 0 },
  });
}

// What the camera can see, as the server's View: a rectangle in degrees, or
// global when the whole globe or the horizon is in view.
export function readView(viewer) {
  const r = viewer.camera.computeViewRectangle(viewer.scene.globe.ellipsoid);
  if (!r) return { global: true };
  const v = { west: r.west * DEG, south: r.south * DEG, east: r.east * DEG, north: r.north * DEG };
  const width = v.east >= v.west ? v.east - v.west : v.east + 360 - v.west;
  if (width >= 359 || v.north - v.south >= 179) return { global: true };
  return v;
}

function differs(a, b) {
  if (!a || !b) return true;
  const eps = Math.max(1e-7, (a.height / 111000) * 2e-4); // degrees, scaled to zoom
  const dLon = Math.abs(((a.lon - b.lon + 540) % 360) - 180);
  return dLon > eps || Math.abs(a.lat - b.lat) > eps || Math.abs(a.height - b.height) > a.height * 1e-4 ||
    Math.abs(((a.heading - b.heading + 540) % 360) - 180) > 0.02 || Math.abs(a.pitch - b.pitch) > 0.02;
}

export class CameraSync {
  // isLive() says whether this client is in the live view; isDetached()
  // whether it has stopped following the shared camera.
  constructor(viewer, { isLive, isDetached }) {
    this.viewer = viewer;
    this.isLive = isLive;
    this.isDetached = isDetached;
    this.applying = false; // a remote camera is being eased in
    this.lastSent = null;
    this.seq = -1; // sequence number of the last shared camera seen
    this.shared = null;
    this.sent = 0; // outbound camera updates, for tests
    this.applied = 0; // remote cameras applied, for tests

    // About five updates a second while moving, and one when movement ends.
    setInterval(() => this.maybeSend(), 200);
    viewer.camera.moveEnd.addEventListener(() => this.maybeSend());
    // The user taking over cancels a remote move.
    const stop = () => {
      if (this.applying) {
        this.flight = (this.flight ?? 0) + 1; // the flight's own callback must not run
        viewer.camera.cancelFlight();
        this.applying = false;
      }
    };
    viewer.canvas.addEventListener('pointerdown', stop);
    viewer.canvas.addEventListener('wheel', stop, { passive: true });
  }

  maybeSend() {
    if (!this.isLive() || this.applying || this.lastSent === null) return;
    const cam = readCamera(this.viewer.camera);
    if (!differs(cam, this.lastSent)) return;
    this.lastSent = cam;
    const view = readView(this.viewer);
    if (this.isDetached()) {
      act('view', { view }); // scope only; the shared camera stays put
    } else {
      this.sent++;
      act('camera', { camera: cam, view });
    }
  }

  // Called with the shared camera whenever its seq changes.
  onShared(cam) {
    this.shared = cam;
    const first = this.seq < 0;
    if (cam.seq === this.seq) return;
    this.seq = cam.seq;
    if (!this.isLive() || this.isDetached()) return;
    if (first) {
      // Opening the page: jump to the shared camera, and tell the server
      // what it shows from here so the query scope is exact.
      applyCamera(this.viewer.camera, cam);
      this.lastSent = readCamera(this.viewer.camera);
      act('view', { view: readView(this.viewer), seq: cam.seq });
      return;
    }
    if (cam.by === clientId) return; // our own move coming back
    this.ease(cam);
  }

  ease(cam, duration = 0.45) {
    this.applying = true;
    this.applied++;
    // Starting a flight cancels the one before it. Only the newest flight
    // may end the "applying" state, or the rest of its motion would look
    // like a local move and be sent back out.
    const flight = (this.flight = (this.flight ?? 0) + 1);
    const done = () => {
      if (flight !== this.flight) return;
      this.applying = false;
      // Mark the result as sent, so the applied move is never re-broadcast.
      this.lastSent = readCamera(this.viewer.camera);
    };
    this.viewer.camera.flyTo({
      destination: C.Cartesian3.fromDegrees(cam.lon, cam.lat, cam.height),
      orientation: { heading: cam.heading / DEG, pitch: cam.pitch / DEG, roll: 0 },
      duration, easingFunction: C.EasingFunction.QUADRATIC_OUT,
      complete: done, cancel: done,
    });
  }

  // Tell the server what the shared camera shows from here, if this client
  // is still sitting on it. Used once the client id is known.
  reportSharedView() {
    if (this.seq < 0 || !this.isLive() || this.isDetached() || this.applying) return;
    if (this.lastSent && !differs(readCamera(this.viewer.camera), this.lastSent)) {
      act('view', { view: readView(this.viewer), seq: this.seq });
    }
  }

  // Go back to the shared camera, after re-attaching or leaving replay.
  rejoin() {
    if (this.shared) this.ease(this.shared, 0.8);
    else this.lastSent = readCamera(this.viewer.camera);
  }

  // Report the current view after becoming detached.
  reportView() {
    this.lastSent = readCamera(this.viewer.camera);
    act('view', { view: readView(this.viewer) });
  }
}
