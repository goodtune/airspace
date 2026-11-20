// Replay of a recorded session. It reads only from the server's database,
// is local to this browser, and never loads a whole session: snapshots are
// fetched a page at a time around the playhead and dropped behind it.

import * as fmt from './fmt.js';
import { pairSnapshots, interpolate, cameraAt, recordedCameraAt, inScope } from './geo.js';
import { applyCamera, readCamera } from './sync.js';

const C = window.Cesium;
const SPEEDS = [1, 2, 5, 10, 30, 60, 120];
const KEEP_BEHIND = 3, KEEP_AHEAD = 30;
const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c]);

const EVENT_LABELS = {
  connect: 'Client connected', disconnect: 'Client disconnected', interval: 'Interval changed',
  selected: 'Selection changed', recording: 'Recording switched', coverage: 'Coverage switched',
  scope: 'Query scope changed', pause: 'Paused (idle)', resume: 'Resumed', state: 'Session start',
};

class Replay {
  constructor(app) {
    this.app = app;
    this.bar = document.getElementById('replay-bar');
    this.active = false;
  }

  // --- opening and closing ---

  async open(id) {
    if (this.active) this.close(false);
    const resp = await fetch(`/api/sessions/${id}`);
    if (!resp.ok) {
      window.alert('That session could not be opened.');
      return;
    }
    const { session, index } = await resp.json();
    if (index.length < 1) {
      window.alert('That session holds no snapshots.');
      return;
    }
    Object.assign(this, {
      id, session, index, times: index.map((i) => i.time),
      start: index[0].time, end: index[index.length - 1].time,
      time: index[0].time, playing: false, speed: 10, buffering: false,
      cache: new Map(), loading: new Set(), pairKey: -1, pairs: [],
      cameraTrack: [], events: [], selections: [], keyframes: [],
      cameraMode: 'recorded', showOutline: true, lastWall: performance.now(), active: true, userSelected: false,
    });
    const app = this.app;
    app.mode = 'replay';
    document.body.classList.add('replay');
    app.sendPresence(); // a replaying client does not count as watching live
    app.layer.clear();
    app.layer.setSelected('');
    app.frameHook = () => this.frame();
    app.viewer.clock.shouldAnimate = false;
    this.buildBar();
    this.ensure(0);
    await this.loadEvents();
    this.updateBar();
  }

  close(rejoin = true) {
    if (!this.active) return;
    this.active = false;
    const app = this.app;
    this.releaseFollow();
    this.bar.hidden = true;
    this.cache.clear();
    app.frameHook = null;
    app.viewer.clock.currentTime = C.JulianDate.now();
    app.viewer.clock.shouldAnimate = true;
    app.mode = 'live';
    document.body.classList.remove('replay');
    app.layer.clear();
    app.live.aircraft.clear();
    app.live.version = 0;
    app.live.etag = '';
    app.resetSeen();
    app.sendPresence();
    app.react();
    if (rejoin) app.camSync.rejoin();
  }

  // --- data ---

  async loadEvents() {
    const base = `/api/sessions/${this.id}/events`;
    const page = async (query, each) => {
      let after = 0;
      for (;;) {
        const resp = await fetch(`${base}?limit=5000&after=${after}&${query}`);
        if (!resp.ok || !this.active) return;
        const data = await resp.json();
        data.events.forEach(each);
        if (!data.more) return;
        after = data.after;
      }
    };
    await page('exclude=camera', (e) => {
      if (e.type === 'state' && e.payload?.camera?.height > 0) {
        this.cameraTrack.push({ time: e.time, ...e.payload.camera });
        this.selections.push({ time: e.time, icao24: e.payload.selected ?? '' });
      }
      if (e.type === 'selected') this.selections.push({ time: e.time, icao24: e.payload?.icao24 ?? '' });
      this.events.push(e);
    });
    // The camera track is small next to the snapshots: five entries a second
    // at most, and only while the camera moved.
    await page('types=camera', (e) => this.cameraTrack.push({ time: e.time, ...e.payload }));
    this.cameraTrack.sort((a, b) => a.time - b.time);
    this.drawMarks();
  }

  // Index of the last snapshot at or before t.
  indexAt(t) {
    let lo = 0, hi = this.times.length - 1;
    while (lo < hi) {
      const mid = (lo + hi + 1) >> 1;
      if (this.times[mid] <= t) lo = mid; else hi = mid - 1;
    }
    return lo;
  }

  // Make sure snapshots from index i onwards are loaded or on their way.
  ensure(i, count = 6) {
    const last = Math.min(this.times.length - 1, i + count - 1);
    let first = -1;
    for (let k = i; k <= last; k++) {
      if (!this.cache.has(k) && !this.loading.has(k)) {
        first = k;
        break;
      }
    }
    if (first < 0) return;
    const n = Math.min(last - first + 1, 12);
    for (let k = first; k < first + n; k++) this.loading.add(k);
    const id = this.id;
    fetch(`/api/sessions/${id}/snapshots?from=${this.times[first]}&to=${this.times[first + n - 1]}&limit=${n}`)
      .then((r) => (r.ok ? r.json() : Promise.reject(new Error(`HTTP ${r.status}`))))
      .then((data) => {
        if (!this.active || this.id !== id) return;
        for (const snap of data.snapshots) {
          const k = this.indexAt(snap.fetched);
          if (this.times[k] === snap.fetched) this.cache.set(k, snap);
        }
      })
      .catch((err) => console.warn('loading snapshots failed', err))
      .finally(() => {
        for (let k = first; k < first + n; k++) this.loading.delete(k);
      });
  }

  evict(k) {
    for (const i of this.cache.keys()) {
      if (i < k - KEEP_BEHIND || i > k + KEEP_AHEAD) this.cache.delete(i);
    }
  }

  // Resolves once the snapshots either side of t are loaded.
  async prepare(t) {
    const k = this.indexAt(t);
    const need = k < this.times.length - 1 ? [k, k + 1] : [k];
    for (let tries = 0; tries < 600; tries++) {
      if (need.every((i) => this.cache.has(i))) return true;
      this.ensure(k, 8);
      await new Promise((r) => setTimeout(r, 25));
      if (!this.active) return false;
    }
    return false;
  }

  // --- drawing ---

  frame() {
    const wall = performance.now();
    const dt = wall - this.lastWall;
    this.lastWall = wall;
    const k = this.indexAt(this.time);
    const ahead = Math.max(6, Math.ceil(this.speed / 4));
    this.ensure(k, ahead);
    this.evict(k);
    const ready = this.cache.has(k) && (k === this.times.length - 1 || this.cache.has(k + 1));
    this.buffering = !ready;
    if (this.playing && ready) {
      this.time = Math.min(this.end, this.time + Math.min(dt, 250) * this.speed);
      if (this.time >= this.end) this.playing = false;
    }
    if (ready) this.drawAt(this.time, this.cameraMode);
    if (wall - (this.lastBar ?? 0) > 200) {
      this.lastBar = wall;
      this.updateBar();
    }
  }

  // Draw the session at time t. Both neighbouring snapshots must be loaded.
  // Returns false when they are not.
  drawAt(t, cameraMode) {
    const { layer, outline } = this.app;
    const k = this.indexAt(t);
    const first = this.cache.get(k), second = this.cache.get(k + 1) ?? null;
    if (!first || (k < this.times.length - 1 && !second)) return false;
    if (this.pairKey !== k) {
      this.pairKey = k;
      this.pairs = pairSnapshots(first, second);
    }
    const f = second ? Math.min(1, Math.max(0, (t - this.times[k]) / (this.times[k + 1] - this.times[k]))) : 0;
    // The sun and stars follow the replay time, not the wall clock, so a
    // frame looks the same whenever it is drawn.
    this.app.viewer.clock.currentTime = C.JulianDate.fromDate(new Date(t));

    if (cameraMode === 'recorded' && !this.userSelected) {
      const sel = this.selectionAt(t);
      if (sel !== layer.selected) layer.setSelected(sel);
    }
    layer.begin();
    for (const pair of this.pairs) {
      const p = interpolate(pair, f);
      if (p.alpha < 0.03) continue;
      layer.place(pair.id, p.lon, p.lat, p.alt, p.track, p.alpha, p.ground, p.data);
    }
    layer.end();
    outline.show = this.showOutline;
    outline.key = ''; // force a redraw when the toggle changed
    outline.set(first.scope);
    this.applyCamera(t, cameraMode);
    return true;
  }

  selectionAt(t) {
    let sel = '';
    for (const s of this.selections) {
      if (s.time > t) break;
      sel = s.icao24;
    }
    return sel;
  }

  applyCamera(t, mode) {
    const camera = this.app.viewer.camera;
    if (mode !== 'follow') this.releaseFollow();
    if (mode === 'recorded') {
      const cam = recordedCameraAt(this.cameraTrack, t);
      if (cam) applyCamera(camera, cam);
    } else if (mode === 'keyframes') {
      const cam = cameraAt(this.keyframes, t, true);
      if (cam) applyCamera(camera, cam);
    } else if (mode === 'follow') {
      const s = this.app.layer.selectedPos;
      if (!s) return;
      const target = C.Cartesian3.fromDegrees(s.lon, s.lat, s.alt * this.app.layer.exaggeration);
      if (!this.following) {
        camera.lookAt(target, new C.HeadingPitchRange(0, -0.6, 60000));
        this.following = true;
      } else {
        // Move the reference frame with the aircraft. camera.position is
        // the camera's offset inside that frame; carrying it over keeps
        // whatever angle and distance the user has orbited to.
        const offset = C.Cartesian3.clone(camera.position);
        camera.lookAtTransform(C.Transforms.eastNorthUpToFixedFrame(target), offset);
      }
    }
  }

  releaseFollow() {
    if (this.following) {
      this.app.viewer.camera.lookAtTransform(C.Matrix4.IDENTITY);
      this.following = false;
    }
  }

  // --- controls ---

  select(id) {
    this.userSelected = true;
    this.app.layer.setSelected(id);
    this.app.renderAircraftPanel();
  }

  seek(t) {
    this.time = Math.min(this.end, Math.max(this.start, t));
    this.ensure(this.indexAt(this.time), 6);
    this.updateBar();
  }

  jump(direction) {
    const list = this.events.filter((e) => e.type !== 'state');
    const t = this.time;
    const hit = direction > 0 ? list.find((e) => e.time > t + 50) : [...list].reverse().find((e) => e.time < t - 1000);
    if (!hit) return;
    this.seek(hit.time);
    this.note(`${EVENT_LABELS[hit.type] ?? hit.type} at ${new Date(hit.time).toLocaleTimeString()}`);
  }

  // The scope recorded at time t, for the keyframe warning.
  scopeAt(t) {
    return this.index[this.indexAt(t)].scope;
  }

  addKeyframe() {
    const viewer = this.app.viewer;
    const cam = readCamera(viewer.camera);
    // What the camera is looking at: the globe under the middle of the view.
    const centre = viewer.camera.pickEllipsoid(new C.Cartesian2(viewer.canvas.clientWidth / 2, viewer.canvas.clientHeight / 2));
    let outside = false;
    if (centre) {
      const c = C.Cartographic.fromCartesian(centre);
      outside = !inScope(this.scopeAt(this.time), C.Math.toDegrees(c.longitude), C.Math.toDegrees(c.latitude));
    } else {
      outside = !this.scopeAt(this.time).global;
    }
    this.keyframes = this.keyframes.filter((k) => Math.abs(k.time - this.time) > 250);
    this.keyframes.push({ time: this.time, ...cam, outside });
    this.keyframes.sort((a, b) => a.time - b.time);
    this.drawMarks();
    this.updateBar();
    this.note(outside
      ? 'Keyframe added. It looks outside the area this session recorded, so there is no traffic there.'
      : `Keyframe ${this.keyframes.length} added.`, outside);
  }

  note(text, warn = false) {
    this.noteText = text;
    this.noteWarn = warn;
    this.noteUntil = performance.now() + 6000;
    this.updateBar();
  }

  buildBar() {
    const s = this.session;
    this.bar.hidden = false;
    this.bar.innerHTML = `
      <div class="line">
        <button class="btn" id="rp-live">Back to live</button>
        <button class="btn" id="rp-prev" title="Jump to the previous event">⏮ Event</button>
        <button class="btn primary" id="rp-play">Play</button>
        <button class="btn" id="rp-next" title="Jump to the next event">Event ⏭</button>
        <span class="time" id="rp-time"></span>
        <span class="track"><input type="range" id="rp-scrub" min="${this.start}" max="${this.end}" step="100" value="${this.start}">
          <span class="marks" id="rp-marks"></span></span>
        <label>Speed <select id="rp-speed">${SPEEDS.map((v) => `<option value="${v}"${v === this.speed ? ' selected' : ''}>${v}×</option>`).join('')}</select></label>
      </div>
      <div class="line">
        <b>${esc(s.name || fmt.dateTime(s.start))}</b>
        <span class="note" style="color:var(--dim)">${esc(s.coverage)} coverage</span>
        <label>Camera <select id="rp-camera">
          <option value="recorded">As recorded</option><option value="free">Free</option>
          <option value="follow">Follow aircraft</option><option value="keyframes">Keyframes</option></select></label>
        <button class="btn" id="rp-key" title="Save the current camera at this moment">Add keyframe</button>
        <button class="btn" id="rp-keyclear">Clear keyframes</button>
        <label><input type="checkbox" id="rp-outline" checked> Scope outline</label>
        <button class="btn" id="rp-export">Export video…</button>
        <span class="spacer"></span>
        <span class="note" id="rp-note"></span>
      </div>`;
    const $ = (id) => this.bar.querySelector(id);
    $('#rp-live').onclick = () => this.close();
    $('#rp-play').onclick = () => {
      if (!this.playing && this.time >= this.end) this.time = this.start;
      this.playing = !this.playing;
      this.updateBar();
    };
    $('#rp-prev').onclick = () => this.jump(-1);
    $('#rp-next').onclick = () => this.jump(1);
    $('#rp-scrub').oninput = (e) => this.seek(Number(e.target.value));
    $('#rp-speed').onchange = (e) => { this.speed = Number(e.target.value); };
    $('#rp-camera').onchange = (e) => this.setCameraMode(e.target.value);
    $('#rp-key').onclick = () => this.addKeyframe();
    $('#rp-keyclear').onclick = () => {
      this.keyframes = [];
      this.drawMarks();
      this.updateBar();
    };
    $('#rp-outline').onchange = (e) => { this.showOutline = e.target.checked; };
    $('#rp-export').onclick = () => this.app.exporter.open();
  }

  setCameraMode(mode) {
    if (mode === 'follow' && !this.app.layer.selected) {
      this.note('Click an aircraft first, then choose Follow aircraft.', true);
      mode = 'free';
    }
    if (mode === 'keyframes' && this.keyframes.length < 2) {
      this.note('Add at least two keyframes in Free mode first.', true);
      mode = 'free';
    }
    this.cameraMode = mode;
    this.bar.querySelector('#rp-camera').value = mode;
  }

  drawMarks() {
    const span = Math.max(1, this.end - this.start);
    const pos = (t) => `${(((t - this.start) / span) * 100).toFixed(3)}%`;
    const marks = this.events.filter((e) => e.type !== 'state' && e.type !== 'scope')
      .map((e) => `<i style="left:${pos(e.time)}" title="${esc(EVENT_LABELS[e.type] ?? e.type)}"></i>`);
    const keys = this.keyframes.map((k) => `<i class="kf${k.outside ? ' outside' : ''}" style="left:${pos(k.time)}"></i>`);
    const el = this.bar.querySelector('#rp-marks');
    if (el) el.innerHTML = marks.join('') + keys.join('');
  }

  updateBar() {
    if (!this.active || this.bar.hidden) return;
    const $ = (id) => this.bar.querySelector(id);
    const clock = (ms) => {
      const s = Math.round(ms / 1000);
      return `${String(Math.floor(s / 60)).padStart(2, '0')}:${String(s % 60).padStart(2, '0')}`;
    };
    $('#rp-time').textContent = `${new Date(this.time).toLocaleTimeString()}  ${clock(this.time - this.start)} / ${clock(this.end - this.start)}`;
    $('#rp-play').textContent = this.playing ? 'Pause' : 'Play';
    const scrub = $('#rp-scrub');
    if (document.activeElement !== scrub) scrub.value = this.time;
    $('#rp-keyclear').textContent = `Clear keyframes (${this.keyframes.length})`;
    $('#rp-keyclear').disabled = this.keyframes.length === 0;
    const note = $('#rp-note');
    let text = '', warn = false;
    if (this.noteUntil && performance.now() < this.noteUntil) {
      [text, warn] = [this.noteText, this.noteWarn];
    } else if (this.buffering) {
      text = 'Loading…';
    } else if (this.keyframes.some((k) => k.outside)) {
      [text, warn] = ['A keyframe looks outside the recorded area.', true];
    }
    note.textContent = text;
    note.classList.toggle('warn-text', warn);
  }

  // For the browser tests.
  debug() {
    return {
      active: this.active, time: this.time, start: this.start, end: this.end, playing: this.playing,
      speed: this.speed, buffering: this.buffering, cached: this.cache?.size ?? 0, snapshots: this.times?.length ?? 0,
      cameraMode: this.cameraMode, cameraTrack: this.cameraTrack?.length ?? 0, events: this.events?.length ?? 0,
      keyframes: this.keyframes ?? [], aircraft: this.app.layer.count, selected: this.app.layer.selected,
    };
  }
}

export function initReplay(app) {
  app.replay = new Replay(app);
  import('./export.js').then(({ initExport }) => initExport(app));
}
