// Video export. The replay is rendered offline, one frame at a time: set the
// replay clock, wait for imagery, capture the canvas, encode, advance. The
// result does not depend on how fast the machine is.

import { Muxer, ArrayBufferTarget, FileSystemWritableFileStreamTarget } from '../vendor/mp4-muxer.mjs';
import * as fmt from './fmt.js';

const SIZES = { '720p': [1280, 720], '1080p': [1920, 1080], '1440p': [2560, 1440], '4K': [3840, 2160] };
const ATTRIBUTION = 'Aircraft data: The OpenSky Network · Imagery: NASA GIBS Blue Marble';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// Pick an encoder configuration the browser supports. H.264 is preferred
// because every player handles it; VP9 and AV1 in MP4 are the alternatives.
async function pickCodec(width, height, fps, bitrate) {
  const pixels = width * height;
  const level = pixels > 2560 * 1440 ? (fps > 30 ? '34' : '33') : pixels > 1920 * 1080 ? '32' : fps > 30 ? '2a' : '28';
  const candidates = [
    { mux: 'avc', codec: `avc1.6400${level}`, name: 'H.264', avc: { format: 'avc' } },
    { mux: 'avc', codec: `avc1.4d00${level}`, name: 'H.264', avc: { format: 'avc' } },
    { mux: 'avc', codec: `avc1.4200${level}`, name: 'H.264', avc: { format: 'avc' } },
    { mux: 'vp9', codec: 'vp09.00.51.08', name: 'VP9' },
    { mux: 'av1', codec: 'av01.0.13M.08', name: 'AV1' },
  ];
  for (const c of candidates) {
    const config = { codec: c.codec, width, height, bitrate, framerate: fps, latencyMode: 'quality', ...(c.avc ? { avc: c.avc } : {}) };
    try {
      if ((await VideoEncoder.isConfigSupported(config)).supported) return { ...c, config };
    } catch { /* try the next one */ }
  }
  return null;
}

class Exporter {
  constructor(app) {
    this.app = app;
    this.dialog = document.getElementById('export-dialog');
    this.running = false;
    this.cancelled = false;
    this.last = null; // facts about the last export, for the tests
  }

  open() {
    const r = this.app.replay;
    const supported = typeof VideoEncoder === 'function' && typeof VideoFrame === 'function';
    const total = (r.end - r.start) / 1000;
    const from = Math.round((r.time - r.start) / 1000);
    const opt = (v, label, sel) => `<option value="${v}"${sel ? ' selected' : ''}>${label ?? v}</option>`;
    this.dialog.innerHTML = `
      <h2>Export video</h2>
      ${supported ? '' : `<p class="msg warn-text" id="ex-unsupported">This browser has no WebCodecs video encoder, so it cannot export video.
        Use a current Chrome, Edge, or Safari. Airspace does not fall back to a lower-quality screen recording.</p>`}
      <div class="grid">
        <label for="ex-from">Start (seconds in)</label><input id="ex-from" type="number" min="0" max="${Math.floor(total)}" value="${Math.min(from, Math.max(0, Math.floor(total) - 1))}">
        <label for="ex-to">End (seconds in)</label><input id="ex-to" type="number" min="1" max="${Math.ceil(total)}" value="${Math.ceil(total)}">
        <label for="ex-speed">Playback speed</label><select id="ex-speed">${[1, 2, 5, 10, 30, 60, 120].map((v) => opt(v, `${v}×`, v === r.speed)).join('')}</select>
        <label for="ex-size">Resolution</label><select id="ex-size">${Object.keys(SIZES).map((k) => opt(k, `${k} (${SIZES[k].join(' × ')})`, k === '1080p')).join('')}</select>
        <label for="ex-fps">Frame rate</label><select id="ex-fps">${[24, 25, 30, 60].map((v) => opt(v, `${v} fps`, v === 30)).join('')}</select>
        <label for="ex-camera">Camera</label><select id="ex-camera">
          ${opt('recorded', 'As recorded', r.cameraMode === 'recorded')}
          ${opt('keyframes', `Keyframes (${r.keyframes.length} placed)`, r.cameraMode === 'keyframes')}
          ${opt('free', 'Fixed, as it is now', r.cameraMode === 'free')}
          ${opt('follow', 'Follow selected aircraft', r.cameraMode === 'follow')}</select>
        <label for="ex-outline">Scope outline</label><input id="ex-outline" type="checkbox" ${r.showOutline ? 'checked' : ''}>
        <label for="ex-clock">Burn in clock</label><input id="ex-clock" type="checkbox" checked>
        <label for="ex-attrib">Burn in attribution</label><input id="ex-attrib" type="checkbox" checked>
        <label for="ex-target">Save</label><select id="ex-target">
          ${opt('download', 'Download when finished')}
          ${'showSaveFilePicker' in window ? opt('file', 'Choose a file, write as it goes') : ''}</select>
      </div>
      <div class="msg" id="ex-msg"></div>
      <progress id="ex-progress" max="1" value="0" hidden></progress>
      <div class="actions">
        <button class="btn" id="ex-cancel">Close</button>
        <button class="btn primary" id="ex-start" ${supported ? '' : 'disabled'}>Export</button>
      </div>`;
    const $ = (id) => this.dialog.querySelector(id);
    const describe = () => {
      const o = this.options();
      const secs = (o.to - o.from) / 1000 / o.speed;
      const warn = o.camera === 'keyframes' && r.keyframes.some((k) => k.outside);
      $('#ex-msg').textContent = secs > 0
        ? `${fmt.dur(secs)} of video, ${Math.ceil(secs * o.fps)} frames.` + (warn ? ' A keyframe looks outside the recorded area.' : '')
        : 'The end must be after the start.';
      $('#ex-msg').classList.toggle('warn-text', warn || secs <= 0);
    };
    this.dialog.querySelectorAll('input, select').forEach((el) => el.addEventListener('input', describe));
    describe();
    $('#ex-cancel').onclick = () => {
      if (this.running) this.cancelled = true;
      else this.dialog.close();
    };
    $('#ex-start').onclick = () => this.start();
    this.dialog.oncancel = (e) => {
      if (this.running) {
        e.preventDefault();
        this.cancelled = true;
      }
    };
    if (!this.dialog.open) this.dialog.showModal();
  }

  options() {
    const r = this.app.replay;
    const v = (id) => this.dialog.querySelector(id);
    const [width, height] = SIZES[v('#ex-size').value];
    return {
      from: r.start + Number(v('#ex-from').value) * 1000,
      to: Math.min(r.end, r.start + Number(v('#ex-to').value) * 1000),
      speed: Number(v('#ex-speed').value), fps: Number(v('#ex-fps').value), width, height,
      camera: v('#ex-camera').value, outline: v('#ex-outline').checked,
      clock: v('#ex-clock').checked, attribution: v('#ex-attrib').checked, target: v('#ex-target').value,
    };
  }

  async start() {
    const o = this.options();
    const r = this.app.replay;
    const $ = (id) => this.dialog.querySelector(id);
    const fail = (msg) => {
      $('#ex-msg').textContent = msg;
      $('#ex-msg').classList.add('warn-text');
    };
    if (o.to <= o.from) return fail('The end must be after the start.');
    if (o.camera === 'keyframes' && r.keyframes.length < 2) return fail('Place at least two keyframes in Free camera mode first.');
    if (o.camera === 'follow' && !this.app.layer.selected) return fail('Click an aircraft to follow first.');

    let writable = null;
    if (o.target === 'file') {
      try {
        const handle = await window.showSaveFilePicker({ suggestedName: this.fileName(), types: [{ description: 'MP4 video', accept: { 'video/mp4': ['.mp4'] } }] });
        writable = await handle.createWritable();
      } catch {
        return; // the user closed the file picker
      }
    }
    $('#ex-start').disabled = true;
    $('#ex-cancel').textContent = 'Cancel';
    $('#ex-progress').hidden = false;
    this.dialog.querySelectorAll('.grid input, .grid select').forEach((el) => { el.disabled = true; });
    try {
      const result = await this.run(o, writable, (done, total, text) => {
        $('#ex-progress').value = done / total;
        $('#ex-msg').textContent = text;
        $('#ex-msg').classList.remove('warn-text');
      });
      $('#ex-msg').textContent = result.cancelled ? 'Export cancelled.'
        : `Done: ${result.frames} frames, ${fmt.bytes(result.bytes)}, ${result.codec}.` +
          (result.codec === 'H.264' ? '' : ` This browser cannot encode H.264, so the file uses ${result.codec}; some players may not open it.`) +
          (result.slowTiles ? ` Imagery was still loading on ${result.slowTiles} frames.` : '');
    } catch (err) {
      console.error(err);
      fail(`Export failed: ${err.message}`);
    } finally {
      $('#ex-cancel').textContent = 'Close';
      $('#ex-start').disabled = false;
      this.dialog.querySelectorAll('.grid input, .grid select').forEach((el) => { el.disabled = false; });
    }
  }

  fileName() {
    const d = new Date(this.app.replay.start);
    const p = (n) => String(n).padStart(2, '0');
    return `airspace-${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}.mp4`;
  }

  // run renders and encodes. Encoded chunks go to the muxer as they are
  // produced and frames are closed at once, so memory use stays flat.
  async run(o, writable, progress) {
    const { viewer, replay, layer } = this.app;
    const bitrate = Math.round(o.width * o.height * o.fps * 0.12);
    const codec = await pickCodec(o.width, o.height, o.fps, bitrate);
    if (!codec) throw new Error('this browser cannot encode video at that size');

    const target = writable ? new FileSystemWritableFileStreamTarget(writable) : new ArrayBufferTarget();
    const muxer = new Muxer({
      target, video: { codec: codec.mux, width: o.width, height: o.height, frameRate: o.fps },
      fastStart: writable ? false : 'in-memory', firstTimestampBehavior: 'offset',
    });
    let encodeError = null;
    const encoder = new VideoEncoder({
      output: (chunk, meta) => muxer.addVideoChunk(chunk, meta),
      error: (e) => { encodeError = e; },
    });
    encoder.configure(codec.config);

    // Take over the frame: fixed-size canvas, no automatic rendering.
    const globe = document.getElementById('globe');
    const saved = {
      loop: viewer.useDefaultRenderLoop, hook: this.app.frameHook, style: globe.getAttribute('style') ?? '',
      recommended: viewer.useBrowserRecommendedResolution, scale: viewer.resolutionScale,
      playing: replay.playing, time: replay.time, outline: replay.showOutline,
    };
    this.running = true;
    this.cancelled = false;
    replay.playing = false;
    replay.showOutline = o.outline;
    viewer.useDefaultRenderLoop = false;
    this.app.frameHook = () => {};
    document.body.classList.add('exporting');
    const fit = Math.min(window.innerWidth / o.width, window.innerHeight / o.height);
    globe.style.cssText = `width:${o.width}px;height:${o.height}px;transform:scale(${fit})`;
    viewer.useBrowserRecommendedResolution = true; // one canvas pixel for each CSS pixel
    viewer.resolutionScale = 1;
    viewer.resize();

    // hashFrames is a test aid: it records a checksum of every frame's
    // pixels, to prove two runs draw the same thing.
    const hashes = this.hashFrames ? [] : null;
    const overlay = o.clock || o.attribution || hashes ? new OffscreenCanvas(o.width, o.height) : null;
    const pen = overlay?.getContext('2d', { willReadFrequently: !!hashes });
    const total = Math.max(1, Math.ceil(((o.to - o.from) / 1000 / o.speed) * o.fps));
    const frameUs = 1e6 / o.fps;
    let frames = 0, slowTiles = 0, lastIndex = -1;
    try {
      for (let i = 0; i < total && !this.cancelled; i++) {
        if (encodeError) throw encodeError;
        const t = Math.min(o.to, o.from + (i / o.fps) * o.speed * 1000);
        if (!(await replay.prepare(t))) throw new Error('recorded data could not be loaded');
        replay.time = t;
        // Icons are drawn in the order they were created, and overlapping
        // icons blend in that order. Rebuild the layer at each snapshot
        // boundary so the order depends on the data alone, not on what was
        // on screen before the export began.
        const k = replay.indexAt(t);
        if (k !== lastIndex) {
          lastIndex = k;
          layer.clear();
        }
        replay.drawAt(t, o.camera);
        // Render until every imagery tile for this view has arrived and
        // every aircraft icon is ready to draw.
        viewer.render();
        const waitStart = performance.now();
        while (!viewer.scene.globe.tilesLoaded || !layer.ready()) {
          if (this.cancelled) break;
          if (performance.now() - waitStart > 20000) {
            slowTiles++;
            break;
          }
          await sleep(15);
          viewer.render();
        }
        if (this.cancelled) break;
        viewer.render();

        let source = viewer.canvas;
        if (pen) {
          pen.drawImage(viewer.canvas, 0, 0, o.width, o.height);
          this.burnIn(pen, o, t);
          source = overlay;
          if (hashes) {
            const px = pen.getImageData(0, 0, o.width, o.height).data;
            let h = 0x811c9dc5;
            for (let j = 0; j < px.length; j++) h = Math.imul(h ^ px[j], 0x01000193);
            hashes.push(h >>> 0);
          }
        }
        const frame = new VideoFrame(source, { timestamp: Math.round(i * frameUs), duration: Math.round(frameUs) });
        encoder.encode(frame, { keyFrame: i % (o.fps * 2) === 0 });
        frame.close();
        frames++;
        // Do not let frames pile up waiting for the encoder.
        while (encoder.encodeQueueSize > 6 && !this.cancelled) await sleep(5);
        if (i % 3 === 0) {
          progress(i + 1, total, `Frame ${i + 1} of ${total}`);
          await sleep(0); // let the page paint and take a cancel click
        }
      }
      if (encodeError) throw encodeError;
      // A cancelled export is thrown away, so there is no reason to wait for
      // the frames still queued in the encoder. Closing it drops them.
      if (!this.cancelled) {
        await encoder.flush();
        muxer.finalize();
      }
    } finally {
      if (encoder.state !== 'closed') encoder.close();
      document.body.classList.remove('exporting');
      globe.setAttribute('style', saved.style);
      viewer.useBrowserRecommendedResolution = saved.recommended;
      viewer.resolutionScale = saved.scale;
      viewer.resize();
      replay.time = saved.time;
      replay.showOutline = saved.outline;
      replay.lastWall = performance.now();
      this.app.frameHook = saved.hook;
      viewer.useDefaultRenderLoop = saved.loop;
      this.running = false;
    }

    let bytes = 0;
    if (writable) {
      if (this.cancelled) await writable.abort();
      else await writable.close();
    } else if (!this.cancelled) {
      const blob = new Blob([target.buffer], { type: 'video/mp4' });
      bytes = blob.size;
      const a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = this.fileName();
      a.click();
      setTimeout(() => URL.revokeObjectURL(a.href), 60000);
    }
    this.last = { frames, total, bytes, codec: codec.name, cancelled: this.cancelled, slowTiles, width: o.width, height: o.height, fps: o.fps, hashes };
    return this.last;
  }

  burnIn(pen, o, t) {
    const size = Math.round(o.height / 45);
    pen.font = `600 ${size}px ui-monospace, Menlo, Consolas, monospace`;
    pen.textBaseline = 'bottom';
    pen.lineWidth = Math.max(2, size / 6);
    pen.strokeStyle = 'rgba(0,0,0,0.8)';
    pen.fillStyle = '#fff';
    const pad = size;
    const put = (text, x, align) => {
      pen.textAlign = align;
      pen.strokeText(text, x, o.height - pad);
      pen.fillText(text, x, o.height - pad);
    };
    if (o.clock) put(new Date(t).toLocaleString([], { dateStyle: 'medium', timeStyle: 'medium' }), pad, 'left');
    if (o.attribution) {
      pen.font = `${Math.round(size * 0.75)}px system-ui, sans-serif`;
      put(ATTRIBUTION, o.width - pad, 'right');
    }
  }
}

export function initExport(app) {
  app.exporter = new Exporter(app);
}
