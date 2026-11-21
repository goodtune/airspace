import { test, expect, openApp, debug, replayDebug, sleep, moveCamera } from '../helpers.js';

const EUROPE = '44,-2,56,22'; // lamin,lomin,lamax,lomax

async function openReplay(page, index = 0) {
  await page.locator('#btn-sessions').click();
  await page.locator('#sessions .session').nth(index).locator('[data-do=replay]').click();
  await expect(page.locator('#replay-bar')).toBeVisible();
  await page.waitForFunction(() => {
    const d = window.airspace.replay.debug();
    return d.active && !d.buffering && d.aircraft > 0;
  }, null, { timeout: 30_000 });
}

test.describe('recording and replay', () => {
  test('a recorded session replays after a restart with OpenSky unreachable', async ({ stack, page, browser }) => {
    await stack.startFake({ capture: true });
    await stack.startApp();
    await openApp(page, stack);
    await expect.poll(async () => (await debug(page)).aircraft).toBeGreaterThan(20);
    const seen = (await debug(page)).aircraft;
    await sleep(1500); // the session opens with the camera at rest
    await moveCamera(page, 9.5, 50.5, 320000);
    await sleep(6000);
    await moveCamera(page, 11, 49, 350000);
    await sleep(12000);
    const polls = (await stack.stats()).stateRequests;
    expect(polls).toBeGreaterThanOrEqual(3);
    await page.close();

    // Restart the binary, with the upstream gone for good.
    await stack.stopApp();
    await stack.stopFake();
    await stack.startApp();
    const list = await (await fetch(`${stack.url}/api/sessions`)).json();
    expect(list.sessions).toHaveLength(1);
    expect(list.sessions[0].snapshots).toBeGreaterThanOrEqual(3);
    expect(list.sessions[0].coverage).toBe('viewport');
    expect(list.sessions[0].end).toBeGreaterThan(list.sessions[0].start);

    const p2 = await openApp(await browser.newPage({ viewport: { width: 1280, height: 800 } }), stack);
    await openReplay(p2);
    const r = await replayDebug(p2);
    expect(r.snapshots).toBe(list.sessions[0].snapshots);
    // What replay shows matches what was seen live, give or take arrivals.
    expect(Math.abs(r.aircraft - seen)).toBeLessThan(seen * 0.5 + 20);
    await expect(p2.locator('body')).toHaveClass(/replay/);

    // "As recorded" follows the recorded camera: at the end it is where the
    // live camera finished.
    expect(r.cameraMode).toBe('recorded');
    expect(r.cameraTrack).toBeGreaterThanOrEqual(3);
    await p2.evaluate(() => window.airspace.replay.seek(window.airspace.replay.end));
    await expect.poll(async () => {
      const c = (await debug(p2)).camera;
      return Math.abs(c.lon - 11) < 0.01 && Math.abs(c.lat - 49) < 0.01;
    }).toBe(true);
    await p2.evaluate(() => window.airspace.replay.seek(window.airspace.replay.start));
    await expect.poll(async () => Math.abs((await debug(p2)).camera.lon - 8.5) < 0.01).toBe(true);

    // A replaying browser is not a live viewer: the server stays stopped and
    // makes no attempt to reach OpenSky.
    await expect.poll(async () => (await stack.status()).status.state).toBe('stopped');
    const attempts = () => (stack.logs.match(/upstream poll failed/g) ?? []).length;
    const before = attempts();
    await sleep(7000);
    expect(attempts()).toBe(before);
    await p2.close();
  });

  test('controls: play, pause, scrub, speed, jump to event', async ({ stack, page }) => {
    stack.seed({ bbox: EUROPE, name: 'Europe capture' });
    await stack.startApp();
    await openApp(page, stack);
    await openReplay(page);
    let r = await replayDebug(page);
    expect(r.snapshots).toBe(87);
    expect(r.playing).toBe(false);
    expect(r.time).toBe(r.start);

    await page.locator('#rp-speed').selectOption('60');
    await page.locator('#rp-play').click();
    await expect(page.locator('#rp-play')).toHaveText('Pause');
    await sleep(3000);
    await page.locator('#rp-play').click();
    r = await replayDebug(page);
    expect(r.playing).toBe(false);
    // About 3 s at 60x is 180 s of session time.
    expect(r.time - r.start).toBeGreaterThan(90_000);
    expect(r.time - r.start).toBeLessThan(260_000);
    const paused = r.time;
    await sleep(600);
    expect((await replayDebug(page)).time).toBe(paused);

    // Scrub to the middle.
    const mid = Math.round((r.start + r.end) / 2);
    await page.locator('#rp-scrub').fill(String(mid));
    await expect.poll(async () => Math.abs((await replayDebug(page)).time - mid)).toBeLessThan(200);
    await page.waitForFunction(() => !window.airspace.replay.debug().buffering);
    expect((await replayDebug(page)).aircraft).toBeGreaterThan(100);

    // Only a window of snapshots is ever held, never the whole session.
    expect((await replayDebug(page)).cached).toBeLessThan(40);

    // 120x plays to the end and stops.
    await page.locator('#rp-speed').selectOption('120');
    await page.locator('#rp-play').click();
    await expect.poll(async () => (await replayDebug(page)).time, { timeout: 20000 }).toBe(r.end);
    expect((await replayDebug(page)).playing).toBe(false);

    await page.locator('#rp-prev').click(); // the session's last event
    await page.locator('#rp-live').click();
    await expect(page.locator('#replay-bar')).toBeHidden();
    expect((await debug(page)).mode).toBe('live');
  });

  test('aircraft are interpolated between snapshots, not dead-reckoned', async ({ stack, page }) => {
    stack.seed({ bbox: EUROPE });
    await stack.startApp();
    await openApp(page, stack);
    await openReplay(page);
    // Halfway between the first two snapshots, an airborne aircraft present
    // in both sits between its two recorded positions.
    const result = await page.evaluate(async () => {
      const { replay, layer } = window.airspace;
      await replay.prepare(replay.times[0]);
      const [a, b] = [replay.cache.get(0), replay.cache.get(1)];
      const second = new Map(b.aircraft.map((x) => [x[0], x]));
      const first = a.aircraft.find((x) => second.has(x[0]) && !x[7] && x[8] > 150 &&
        Math.hypot(x[3] - second.get(x[0])[3], x[4] - second.get(x[0])[4]) > 0.01);
      const end = second.get(first[0]);
      const at = (t) => {
        replay.drawAt(t, 'free');
        const c = window.Cesium.Cartographic.fromCartesian(layer.items.get(first[0]).bb.position);
        return [window.Cesium.Math.toDegrees(c.longitude), window.Cesium.Math.toDegrees(c.latitude)];
      };
      const t0 = replay.times[0], t1 = replay.times[1];
      return { start: [first[3], first[4]], end: [end[3], end[4]], p0: at(t0), half: at((t0 + t1) / 2), p1: at(t1 - 1) };
    });
    const close = (p, q, eps) => Math.hypot(p[0] - q[0], p[1] - q[1]) < eps;
    expect(close(result.p0, result.start, 1e-6)).toBe(true);
    expect(close(result.p1, result.end, 1e-3)).toBe(true);
    const midpoint = [(result.start[0] + result.end[0]) / 2, (result.start[1] + result.end[1]) / 2];
    expect(close(result.half, midpoint, 1e-3)).toBe(true);
  });

  test('a viewport session holds traffic only inside its outline; a global one is worldwide', async ({ stack, page }) => {
    stack.seed({ bbox: EUROPE, name: 'Europe only' });
    await stack.startApp();
    await openApp(page, stack);
    await openReplay(page);
    const spread = () => page.evaluate(() => {
      const C = window.Cesium;
      let west = 180, east = -180, south = 90, north = -90;
      for (const it of window.airspace.layer.items.values()) {
        const c = C.Cartographic.fromCartesian(it.bb.position);
        const lon = C.Math.toDegrees(c.longitude), lat = C.Math.toDegrees(c.latitude);
        west = Math.min(west, lon); east = Math.max(east, lon); south = Math.min(south, lat); north = Math.max(north, lat);
      }
      return { west, east, south, north, outline: window.airspace.outline.lines.length };
    });
    const v = await spread();
    expect(v.west).toBeGreaterThanOrEqual(-2.01);
    expect(v.east).toBeLessThanOrEqual(22.01);
    expect(v.south).toBeGreaterThanOrEqual(43.99);
    expect(v.north).toBeLessThanOrEqual(56.01);
    expect(v.outline).toBe(1); // the recorded scope is drawn
    await page.locator('#rp-outline').uncheck();
    await expect.poll(async () => (await spread()).outline).toBe(0);
    await page.screenshot({ path: '.tmp/replay-viewport.png' });
    await page.locator('#rp-live').click();

    // Now a global session in the same database.
    await stack.stopApp();
    stack.seed({ name: 'Whole world' });
    await stack.startApp();
    await openApp(page, stack);
    await page.locator('#btn-sessions').click();
    await expect(page.locator('#sessions .session')).toHaveCount(2);
    await expect(page.locator('#sessions')).toContainText('global');
    await expect(page.locator('#sessions')).toContainText('viewport');
    await page.locator('#btn-sessions').click();
    const globalIndex = await page.evaluate(async () => {
      const { sessions } = await (await fetch('/api/sessions')).json();
      return sessions.findIndex((s) => s.coverage === 'global');
    });
    await openReplay(page, globalIndex);
    const g = await spread();
    expect(g.east - g.west).toBeGreaterThan(300);
    expect(g.north - g.south).toBeGreaterThan(100);
    expect(g.outline).toBe(0); // nothing to outline: everything was queried
    expect((await replayDebug(page)).aircraft).toBeGreaterThan(8000);
    await page.evaluate(() => window.airspace.replay.setCameraMode('free'));
    await moveCamera(page, 20, 30, 2.2e7);
    await sleep(1500);
    await page.screenshot({ path: '.tmp/replay-global.png' });
  });

  test('camera modes: free, follow aircraft, keyframes with an outside-area warning', async ({ stack, page }) => {
    stack.seed({ bbox: EUROPE });
    await stack.startApp();
    await openApp(page, stack);
    await openReplay(page);

    // Follow needs a selection first.
    await page.locator('#rp-camera').selectOption('follow');
    await expect(page.locator('#rp-note')).toContainText('Click an aircraft first');
    await expect(page.locator('#rp-camera')).toHaveValue('free');

    // Pick a fast aircraft and follow it while playing.
    const id = await page.evaluate(() => {
      const { replay } = window.airspace;
      const a = replay.cache.get(0).aircraft.find((x) => !x[7] && x[8] > 200 && x[6] > 9000);
      replay.select(a[0]);
      return a[0];
    });
    await expect(page.locator('#aircraft')).toContainText(id);
    await page.locator('#rp-camera').selectOption('follow');
    await expect(page.locator('#rp-camera')).toHaveValue('follow');
    const camDistance = () => page.evaluate(() => {
      const { viewer, layer } = window.airspace;
      const C = window.Cesium, s = layer.selectedPos;
      return C.Cartesian3.distance(viewer.camera.positionWC, C.Cartesian3.fromDegrees(s.lon, s.lat, s.alt));
    });
    await sleep(500);
    const d0 = await camDistance();
    const posBefore = (await debug(page)).camera;
    await page.locator('#rp-speed').selectOption('30');
    const t0 = (await replayDebug(page)).time;
    await page.locator('#rp-play').click();
    // Play until 90 s of the session has gone by. On a slow renderer that
    // takes longer than the 3 s it takes at full frame rate.
    await expect.poll(async () => (await replayDebug(page)).time - t0, { timeout: 60000 }).toBeGreaterThan(90_000);
    await page.locator('#rp-play').click();
    const posAfter = (await debug(page)).camera;
    // The camera travelled with the aircraft and kept its distance.
    expect(Math.hypot(posAfter.lon - posBefore.lon, posAfter.lat - posBefore.lat)).toBeGreaterThan(0.05);
    expect(Math.abs((await camDistance()) - d0)).toBeLessThan(d0 * 0.05);

    // Keyframes: one inside the recorded area, one far outside it.
    await page.locator('#rp-camera').selectOption('free');
    await page.evaluate(() => window.airspace.replay.seek(window.airspace.replay.start + 10_000));
    await moveCamera(page, 10, 50, 900000);
    await page.locator('#rp-key').click();
    await expect(page.locator('#rp-note')).toContainText('Keyframe 1 added');
    await page.evaluate(() => window.airspace.replay.seek(window.airspace.replay.start + 70_000));
    await moveCamera(page, -100, 40, 900000); // North America: not recorded
    await page.locator('#rp-key').click();
    await expect(page.locator('#rp-note')).toContainText('outside the area this session recorded');
    await expect(page.locator('#rp-marks .kf')).toHaveCount(2);
    await expect(page.locator('#rp-marks .kf.outside')).toHaveCount(1);

    // Keyframe mode interpolates the camera between them.
    await page.locator('#rp-camera').selectOption('keyframes');
    await page.evaluate(() => window.airspace.replay.seek(window.airspace.replay.start + 40_000));
    await expect.poll(async () => {
      const c = (await debug(page)).camera;
      return c.lon < 5 && c.lon > -95;
    }).toBe(true);
    await page.locator('#rp-keyclear').click();
    await expect(page.locator('#rp-marks .kf')).toHaveCount(0);
  });

  test('sessions can be renamed and deleted', async ({ stack, page }) => {
    stack.seed({ bbox: EUROPE, name: 'Before' });
    await stack.startApp();
    await openApp(page, stack);
    await page.locator('#btn-sessions').click();
    const row = page.locator('#sessions .session').first();
    await expect(row.locator('.name')).toHaveText('Before');
    await expect(row.locator('.meta')).toContainText('87 snapshots');
    await expect(row.locator('.meta')).toContainText('15 min');
    await expect(row.locator('.meta')).toContainText('MB');
    await expect(row.locator('.tag')).toHaveText('viewport');
    page.once('dialog', (d) => d.accept('After'));
    await row.locator('[data-do=rename]').click();
    await expect(page.locator('#sessions .session .name').first()).toHaveText('After');
    page.once('dialog', (d) => d.accept());
    await page.locator('#sessions .session [data-do=delete]').first().click();
    await expect(page.locator('#sessions .empty')).toBeVisible();
    expect((await (await fetch(`${stack.url}/api/sessions`)).json()).sessions).toHaveLength(0);
  });
});
