import { test, expect, openApp, debug, sleep, moveCamera, Stack } from '../helpers.js';

test.describe('nobody watching', () => {
  test('makes no upstream request with no browser, or after the last tab closes', async ({ stack, page }) => {
    await stack.startFake();
    await stack.startApp();
    await sleep(6000); // longer than a poll interval
    for (const path of ['/api/states', '/api/status', '/healthz', '/api/sessions']) {
      expect((await fetch(stack.url + path)).status).toBe(200);
    }
    expect(await stack.upstream()).toBe(0);

    await openApp(page, stack);
    await expect.poll(() => stack.upstream()).toBeGreaterThanOrEqual(2); // a token and a poll
    await page.close();
    await sleep(500); // an in-flight request may finish
    const after = await stack.upstream();
    await sleep(12000); // more than two intervals
    expect(await stack.upstream()).toBe(after);
    expect((await stack.status()).status.state).toBe('stopped');
    expect(stack.logs).toContain('upstream poll');
  });
});

test.describe('live view', () => {
  test.beforeEach(async ({ stack }) => {
    await stack.startFake({ capture: true });
    await stack.startApp();
  });

  test('shows moving aircraft within one poll interval', async ({ stack, page }) => {
    await openApp(page, stack);
    await expect(page).toHaveTitle('Airspace');
    await expect(page.locator('header h1')).toHaveText('Airspace');
    await expect(page.locator('#credits')).toContainText('OpenSky Network');
    await expect(page.locator('#credits')).toContainText('NASA Global Imagery Browse Services');
    await expect.poll(async () => (await debug(page)).aircraft, { timeout: 5000 }).toBeGreaterThan(20);
    await expect(page.locator('#mode-chip')).toHaveText('LIVE');
    await expect(page.locator('#ro-clients')).toHaveText('1 active of 1');

    // Aircraft move between snapshots by dead reckoning.
    const positions = () => page.evaluate(() => {
      const out = {};
      for (const [id, it] of window.airspace.layer.items) out[id] = [it.bb.position.x, it.bb.position.y, it.bb.position.z];
      return out;
    });
    // Hold off the next snapshot, so any movement seen is dead reckoning.
    await page.evaluate(() => window.act('interval', { seconds: 120 }));
    await expect.poll(async () => (await stack.status()).interval.seconds).toBe(120);
    await sleep(500);
    const before = await positions();
    const version = (await debug(page)).version;
    const movedCount = async () => {
      const after = await positions();
      return Object.keys(before).filter((id) => after[id] && Math.hypot(...before[id].map((v, i) => v - after[id][i])) > 50).length;
    };
    // Aircraft move every frame. A slow renderer draws few frames, so wait
    // for the movement, not for a fixed time.
    await expect.poll(movedCount, { timeout: 30000 }).toBeGreaterThan(Object.keys(before).length / 3);
    expect((await debug(page)).version).toBe(version); // no new snapshot in between
  });

  test('draws NASA GIBS imagery', async ({ stack, page }) => {
    const tiles = [];
    page.on('response', (r) => { if (r.url().includes('gibs.earthdata.nasa.gov')) tiles.push(r.status()); });
    await openApp(page, stack, '');
    await expect.poll(async () => (await debug(page)).tilesLoaded && tiles.length > 0, { timeout: 30000 }).toBe(true);
    expect(tiles.filter((s) => s === 200).length).toBeGreaterThan(0);
    // Nothing else leaves the machine: no CDN, no Cesium ion.
    const external = [];
    page.on('request', (r) => {
      const u = new URL(r.url());
      if (!u.protocol.startsWith('http')) return; // blob: and data: stay in the page
      if (u.hostname !== '127.0.0.1' && u.hostname !== 'gibs.earthdata.nasa.gov') external.push(r.url());
    });
    await page.reload();
    await page.waitForFunction(() => window.airspace?.ready);
    await sleep(3000);
    expect(external).toEqual([]);
    await page.screenshot({ path: '.tmp/live-gibs.png' });
  });

  test('selecting an aircraft opens the panel', async ({ stack, page }) => {
    await openApp(page, stack);
    await expect.poll(async () => (await debug(page)).aircraft).toBeGreaterThan(20);
    // Click the aircraft nearest the middle of the screen.
    const target = await page.evaluate(() => {
      const { layer, viewer } = window.airspace;
      const w = viewer.canvas.clientWidth, h = viewer.canvas.clientHeight;
      let best = null;
      for (const [id, it] of layer.items) {
        const p = viewer.scene.cartesianToCanvasCoordinates(it.bb.position);
        if (!p || p.x < 340 || p.x > w - 40 || p.y < 80 || p.y > h - 80) continue;
        const d = Math.hypot(p.x - w / 2, p.y - h / 2);
        if (!best || d < best.d) best = { id, x: p.x, y: p.y, d };
      }
      return best;
    });
    expect(target).not.toBeNull();
    await page.mouse.click(target.x, target.y);
    await expect(page.locator('#aircraft')).toBeVisible();
    await expect(page.locator('#aircraft')).toContainText('ICAO24');
    for (const label of ['Country', 'Altitude', 'Speed', 'Heading', 'Vertical rate', 'Squawk', 'Age of data']) {
      await expect(page.locator('#aircraft')).toContainText(label);
    }
    expect((await stack.status()).selected).not.toBe('');
    await page.locator('#ac-close').click();
    await expect(page.locator('#aircraft')).toBeHidden();
  });

  test('survives an upstream outage and recovers', async ({ stack, page }) => {
    await openApp(page, stack);
    await expect.poll(async () => (await debug(page)).aircraft).toBeGreaterThan(20);
    await stack.setFailing(true);
    await expect(page.locator('#ro-error')).toContainText('showing the last good data', { timeout: 20000 });
    expect((await debug(page)).aircraft).toBeGreaterThan(20); // the last good data stays up
    await expect(page.locator('#mode-chip')).toHaveText('STALE DATA', { timeout: 40000 });
    await stack.setFailing(false);
    await expect(page.locator('#mode-chip')).toHaveText('LIVE', { timeout: 60000 });
    await expect(page.locator('#ro-error')).toBeHidden();
  });

  test('never serves the secret or a token', async ({ stack, page }) => {
    const bodies = [];
    page.on('response', async (r) => {
      if (!r.url().startsWith(stack.url) || r.url().includes('/vendor/cesium/')) return;
      try { bodies.push(await r.text()); } catch { /* streams */ }
    });
    await openApp(page, stack);
    await expect.poll(async () => (await debug(page)).aircraft).toBeGreaterThan(0);
    const all = bodies.join('\n') + JSON.stringify(await stack.status()) + stack.logs;
    expect(all).not.toContain(Stack.SECRET);
    expect(all).not.toContain('fake-token-');
  });
});

test.describe('shared session', () => {
  test.beforeEach(async ({ stack }) => {
    await stack.startFake();
    await stack.startApp();
  });

  test('interval, camera and selection reach the other browser within a second', async ({ stack, page, context }) => {
    const a = await openApp(page, stack);
    const b = await openApp(await context.newPage(), stack);
    await expect(a.locator('#ro-clients')).toHaveText('2 active of 2');

    await a.locator('#interval').fill('17');
    await a.locator('#interval').press('Enter');
    await expect(b.locator('#interval')).toHaveValue('17', { timeout: 1000 });

    await moveCamera(a, 12, 47, 250000);
    // The new camera reaches the other browser within a second...
    await expect.poll(() => b.evaluate(() => window.airspace.state.camera.lon), { timeout: 1000 }).toBeCloseTo(12, 2);
    // ...and its globe eases there. How long the ease takes to draw depends
    // on the machine: a CI runner renders WebGL in software.
    await expect.poll(async () => {
      const c = (await debug(b)).camera;
      return Math.abs(c.lon - 12) < 0.01 && Math.abs(c.lat - 47) < 0.01;
    }, { timeout: 20000 }).toBe(true);

    await a.evaluate(() => window.act('select', { icao24: 'a00010' }));
    await expect.poll(() => b.evaluate(() => window.airspace.layer.selected), { timeout: 1000 }).toBe('a00010');
    await expect(b.locator('#aircraft')).toBeVisible();
  });

  test('camera sync has no feedback loop', async ({ stack, page, context }) => {
    const a = await openApp(page, stack);
    const b = await openApp(await context.newPage(), stack);
    const seqBefore = (await stack.status()).camera.seq;
    for (let i = 0; i < 5; i++) {
      await moveCamera(a, 10 + i, 48, 250000);
      await sleep(250);
    }
    // Wait for B to finish easing to A's last position, however slowly this
    // machine draws, then leave time for any echo to bounce around.
    await expect.poll(async () => Math.abs((await debug(b)).camera.lon - 14) < 0.01, { timeout: 20000 }).toBe(true);
    await sleep(1500);
    const da = await debug(a), db = await debug(b);
    // B applied A's moves and sent none of its own.
    expect(db.cameraApplied).toBeGreaterThan(0);
    expect(db.cameraSent).toBe(0);
    // A never applied its own moves coming back.
    expect(da.cameraApplied).toBe(0);
    expect(da.cameraSent).toBeLessThanOrEqual(6);
    // The server saw only A's moves, and the camera has stopped changing.
    const status = await stack.status();
    expect(status.camera.seq - seqBefore).toBe(da.cameraSent);
    expect(status.camera.by).toBe(da.clientId);
    await sleep(1000);
    expect((await stack.status()).camera.seq).toBe(status.camera.seq);
    expect(Math.abs(db.camera.lon - 14)).toBeLessThan(0.01);
  });

  test('upstream call count is the same with one browser or three', async ({ stack, page, context }) => {
    const count = async () => {
      const before = (await stack.stats()).stateRequests;
      await sleep(16000);
      return (await stack.stats()).stateRequests - before;
    };
    await openApp(page, stack);
    await sleep(1000);
    const one = await count();
    await openApp(await context.newPage(), stack);
    await openApp(await context.newPage(), stack);
    const three = await count();
    expect(one).toBeGreaterThanOrEqual(3);
    expect(Math.abs(one - three)).toBeLessThanOrEqual(1);
  });

  test('a detached browser keeps its own camera and adds its view to the query', async ({ stack, page, context }) => {
    const a = await openApp(page, stack);
    const b = await openApp(await context.newPage(), stack);
    await moveCamera(a, 10, 48, 200000);
    await expect.poll(async () => Math.abs((await debug(b)).camera.lon - 10) < 0.01).toBe(true);
    await b.locator('#detach').click();
    await moveCamera(b, 100, 30, 200000);
    await expect.poll(async () => (await stack.status()).scope.boxes?.length, { timeout: 3000 }).toBe(2);
    await sleep(800);
    // The shared camera did not follow the detached browser, and A stayed put.
    expect(Math.abs((await stack.status()).camera.lon - 10)).toBeLessThan(0.01);
    expect(Math.abs((await debug(a)).camera.lon - 10)).toBeLessThan(0.01);
    // Nor does the detached browser follow the shared camera.
    await moveCamera(a, 11, 48, 200000);
    await sleep(1200);
    expect(Math.abs((await debug(b)).camera.lon - 100)).toBeLessThan(0.01);
    // Re-attaching returns to the shared camera and drops the extra box.
    await b.locator('#attach').click();
    await expect.poll(async () => Math.abs((await debug(b)).camera.lon - 11) < 0.01, { timeout: 4000 }).toBe(true);
    await expect.poll(async () => (await stack.status()).scope.boxes?.length).toBe(1);
  });
});
