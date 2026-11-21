// Airspace frontend entry point.

import * as fmt from './fmt.js';
import { F } from './geo.js';
import { AircraftLayer, ScopeOutline } from './aircraft.js';
import { Live } from './live.js';
import { CameraSync, act, setClientId, getClientId, readCamera, readView } from './sync.js';

const C = window.Cesium;
const params = new URLSearchParams(location.search);

// --- state mirror -----------------------------------------------------------
// Datastar holds the signals. This is a plain copy of them for the Cesium
// code, kept up to date from every signal patch.
const state = {};
let clockOffset = 0; // server time minus this machine's time, ms
const serverNow = () => Date.now() + clockOffset;

function merge(into, patch) {
  for (const [k, v] of Object.entries(patch)) {
    if (v && typeof v === 'object' && !Array.isArray(v)) {
      if (!into[k] || typeof into[k] !== 'object' || Array.isArray(into[k])) into[k] = {};
      merge(into[k], v);
    } else {
      into[k] = v;
    }
  }
}

const app = (window.airspace = {
  state,
  mode: 'live', // or 'replay'
  ready: false,
  onPatch(patch) {
    if (!patch || typeof patch !== 'object') return;
    merge(state, patch);
    if (this.ready) react();
  },
  scopeArea() {
    const boxes = state.scope?.boxes ?? [];
    if (!boxes.length) return null;
    return { boxes: boxes.length, sqdeg: boxes.reduce((s, b) => s + (b.lamax - b.lamin) * (b.lomax - b.lomin), 0) };
  },
});
window.fmt = fmt;
window.act = act;

// --- globe ------------------------------------------------------------------

function imagery() {
  if (params.get('imagery') === 'off') {
    // For tests and machines with no internet: a plain grid.
    return new C.ImageryLayer(new C.GridImageryProvider({
      cells: 4, color: C.Color.fromCssColorString('#1d3a5c'), glowColor: C.Color.fromCssColorString('#1d3a5c'),
      glowWidth: 0, backgroundColor: C.Color.fromCssColorString('#0b1a2b'),
    }));
  }
  // NASA GIBS Blue Marble over WMTS. Layer and matrix set are from the
  // GetCapabilities document. GIBS geographic tiles start as a 2 x 1 grid of
  // 288 degree tiles from the top-left corner, which this scheme describes.
  return new C.ImageryLayer(new C.WebMapTileServiceImageryProvider({
    url: 'https://gibs.earthdata.nasa.gov/wmts/epsg4326/best/BlueMarble_ShadedRelief_Bathymetry/default/{TileMatrixSet}/{TileMatrix}/{TileRow}/{TileCol}.jpeg',
    layer: 'BlueMarble_ShadedRelief_Bathymetry', style: 'default', format: 'image/jpeg',
    tileMatrixSetID: '500m', maximumLevel: 7, tileWidth: 512, tileHeight: 512,
    tilingScheme: new C.GeographicTilingScheme({
      rectangle: C.Rectangle.fromDegrees(-180, -198, 396, 90),
      numberOfLevelZeroTilesX: 2, numberOfLevelZeroTilesY: 1,
    }),
    rectangle: C.Rectangle.fromDegrees(-180, -90, 180, 90),
    credit: 'NASA Global Imagery Browse Services (GIBS)',
  }));
}

C.Ion.defaultAccessToken = ''; // Cesium ion is never used
const viewer = new C.Viewer('globe', {
  baseLayer: imagery(), baseLayerPicker: false, geocoder: false, homeButton: false, sceneModePicker: false,
  navigationHelpButton: false, animation: false, timeline: false, fullscreenButton: false, infoBox: false,
  selectionIndicator: false, scene3DOnly: true,
  contextOptions: { webgl: { preserveDrawingBuffer: params.has('capture') } },
});
viewer.scene.globe.baseColor = C.Color.fromCssColorString('#0b1a2b');
viewer.scene.screenSpaceCameraController.minimumZoomDistance = 2000;
viewer.scene.screenSpaceCameraController.maximumZoomDistance = 4e7;
viewer.cesiumWidget.screenSpaceEventHandler.removeInputAction(C.ScreenSpaceEventType.LEFT_DOUBLE_CLICK);

const layer = new AircraftLayer(viewer);
const outline = new ScopeOutline(viewer);
const live = new Live(layer, serverNow);
const camSync = new CameraSync(viewer, {
  isLive: () => app.mode === 'live',
  isDetached: () => !!state._detached,
});
Object.assign(app, { viewer, layer, outline, live, camSync, serverNow, act });

// --- reacting to signals ----------------------------------------------------

const seen = { version: -1, selected: null, detached: false, exag: 1 };

function react() {
  if (state.clientId && state.clientId !== getClientId()) {
    setClientId(state.clientId);
    sendPresence();
    if (state.camera) camSync.onShared(state.camera);
    camSync.reportSharedView();
  }
  if (state.serverTime) clockOffset = state.serverTime - Date.now();
  if (state.camera) camSync.onShared(state.camera);

  const detached = !!state._detached;
  if (detached !== seen.detached) {
    seen.detached = detached;
    sendPresence();
    if (app.mode === 'live') detached ? camSync.reportView() : camSync.rejoin();
  }
  const exag = Number(state._exag) || 1;
  if (exag !== seen.exag) {
    seen.exag = exag;
    layer.exaggeration = exag;
  }
  if (app.mode !== 'live') return;

  const version = state.snapshot?.version ?? 0;
  if (version !== seen.version) {
    seen.version = version;
    live.onVersion(version);
  }
  if ((state.selected ?? '') !== seen.selected) {
    seen.selected = state.selected ?? '';
    layer.setSelected(seen.selected);
    renderAircraftPanel();
  }
  outline.set(state.snapshot?.scope?.cost ? state.snapshot.scope : state.scope);
}
app.react = react;
// Leaving replay makes the live view catch up from scratch.
app.resetSeen = () => { seen.version = -1; seen.selected = null; };

// --- presence and input -----------------------------------------------------

function sendPresence() {
  act('presence', { visible: !document.hidden, replay: app.mode === 'replay', detached: !!state._detached });
}
app.sendPresence = sendPresence;
document.addEventListener('visibilitychange', sendPresence);
setInterval(sendPresence, 30000); // also restores presence after a reconnect

let lastInput = 0;
function onInput() {
  const now = Date.now();
  if (app.mode !== 'live') return;
  if (now - lastInput > 15000 || state.status?.state === 'paused') {
    lastInput = now;
    act('input');
  }
}
for (const ev of ['pointerdown', 'keydown', 'wheel']) window.addEventListener(ev, onInput, { passive: true, capture: true });

// --- selection --------------------------------------------------------------

const clicks = new C.ScreenSpaceEventHandler(viewer.canvas);
clicks.setInputAction((e) => {
  const id = layer.pick(e.position);
  if (app.mode === 'replay') app.replay?.select(id);
  else if (id !== (state.selected ?? '')) act('select', { icao24: id });
}, C.ScreenSpaceEventType.LEFT_CLICK);

const panel = document.getElementById('aircraft');
const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c]);

// Draw the selected aircraft panel from an aircraft array, or hide it.
export function renderAircraftPanel() {
  const id = layer.selected;
  if (!id || document.body.classList.contains('exporting')) {
    panel.hidden = true;
    return;
  }
  const a = layer.selectedPos?.data ?? (app.mode === 'live' ? live.get(id) : null);
  const nowS = (app.mode === 'replay' ? app.replay?.time ?? 0 : serverNow()) / 1000;
  const v = (x, unit, digits = 0) => (x == null ? '–' : `${Number(x).toFixed(digits)} ${unit}`);
  const rows = a ? [
    ['ICAO24', a[F.icao]],
    ['Country', a[F.country] || '–'],
    ['Altitude', a[F.ground] ? 'on the ground' : v(a[F.geo] ?? a[F.baro], 'm')],
    ['Speed', a[F.velocity] == null ? '–' : `${Math.round(a[F.velocity] * 3.6)} km/h`],
    ['Heading', v(a[F.track], '°')],
    ['Vertical rate', v(a[F.vrate], 'm/s', 1)],
    ['Squawk', a[F.squawk] || '–'],
    ['Age of data', `${Math.max(0, Math.round(nowS - (a[F.lastContact] || a[F.timePos])))} s`],
  ] : [['ICAO24', id], ['', 'not in the current data']];
  panel.hidden = false;
  panel.innerHTML = `<h2><span id="ac-callsign">${esc((a && a[F.callsign]) || id)}</span><button class="btn" id="ac-close">Clear</button></h2>` +
    rows.map(([k, val]) => `<div class="row"><span>${esc(k)}</span><b>${esc(val)}</b></div>`).join('');
  panel.querySelector('#ac-close').onclick = () => (app.mode === 'replay' ? app.replay?.select('') : act('select', { icao24: '' }));
}
app.renderAircraftPanel = renderAircraftPanel;
setInterval(renderAircraftPanel, 1000);

// --- frame loop -------------------------------------------------------------

viewer.scene.preUpdate.addEventListener(() => {
  if (app.frameHook) app.frameHook(); // replay or export owns the frame
  else live.frame();
});

// Counters for the browser tests.
app.debug = () => ({
  mode: app.mode, aircraft: layer.count, version: live.version, camera: readCamera(viewer.camera),
  view: readView(viewer), cameraSent: camSync.sent, cameraApplied: camSync.applied, applying: camSync.applying,
  tilesLoaded: viewer.scene.globe.tilesLoaded, clientId: getClientId(),
});

// --- start ------------------------------------------------------------------
// Read the current state once so the globe opens on the shared camera even
// before the stream connects. This endpoint does not count as activity.
try {
  const resp = await fetch('/api/status');
  if (resp.ok) merge(state, await resp.json());
} catch { /* the stream will supply it */ }
app.ready = true;
react();

const [{ initReplay }, { initSessions }] = await Promise.all([import('./replay.js'), import('./sessions.js')]);
initReplay(app);
initSessions(app);

// ?replay=latest, or ?replay=<session id>, opens straight into replay.
if (params.has('replay')) {
  let id = params.get('replay');
  if (id === 'latest' || id === '') {
    const { sessions } = await (await fetch('/api/sessions')).json();
    id = sessions.find((s) => s.end !== 0)?.id ?? sessions[0]?.id;
  }
  if (id) app.replay.open(id);
}
