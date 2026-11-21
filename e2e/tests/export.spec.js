import { execFileSync } from 'node:child_process';
import { statSync } from 'node:fs';
import { join } from 'node:path';
import { test, expect, openApp, replayDebug, sleep, moveCamera } from '../helpers.js';

const EUROPE = '44,-2,56,22';

function probe(file) {
  const out = execFileSync('ffprobe', ['-v', 'error', '-count_frames', '-select_streams', 'v:0', '-show_entries',
    'stream=codec_name,width,height,r_frame_rate,nb_read_frames:format=duration,format_name', '-of', 'json', file]);
  const j = JSON.parse(out);
  return { ...j.streams[0], frames: Number(j.streams[0].nb_read_frames), duration: Number(j.format.duration), format: j.format.format_name };
}

// Every decoded frame's checksum, to compare two exports exactly.
function frameHashes(file) {
  return execFileSync('ffmpeg', ['-v', 'error', '-i', file, '-f', 'framemd5', '-'], { maxBuffer: 64 << 20 })
    .toString().split('\n').filter((l) => l && !l.startsWith('#')).map((l) => l.split(',').pop().trim());
}

async function setup(stack, page, query) {
  stack.seed({ bbox: EUROPE, name: 'Europe capture' });
  await stack.startApp();
  await openApp(page, stack, query);
  await page.locator('#btn-sessions').click();
  await page.locator('#sessions .session [data-do=replay]').first().click();
  await page.waitForFunction(() => {
    const d = window.airspace.replay.debug();
    return d.active && !d.buffering && d.aircraft > 0;
  }, null, { timeout: 20_000 });
}

async function exportClip(page, dir, name, fields) {
  await page.locator('#rp-export').click();
  await expect(page.locator('#export-dialog')).toBeVisible();
  for (const [id, value] of Object.entries(fields)) {
    const el = page.locator(`#ex-${id}`);
    if (typeof value === 'boolean') await el.setChecked(value);
    else if ((await el.evaluate((e) => e.tagName)) === 'SELECT') await el.selectOption(String(value));
    else await el.fill(String(value));
  }
  const [download] = await Promise.all([
    page.waitForEvent('download', { timeout: 240_000 }),
    page.locator('#ex-start').click(),
  ]);
  const file = join(dir, name);
  await download.saveAs(file);
  await expect(page.locator('#ex-msg')).toContainText('Done:');
  const last = await page.evaluate(() => window.airspace.exporter.last);
  await page.locator('#ex-cancel').click();
  await expect(page.locator('#export-dialog')).toBeHidden();
  return { file, last, name: download.suggestedFilename() };
}

async function placeKeyframes(page) {
  await page.locator('#rp-camera').selectOption('free');
  await page.evaluate(() => window.airspace.replay.seek(window.airspace.replay.start));
  await moveCamera(page, 6, 51, 1_200_000);
  await page.locator('#rp-key').click();
  await page.evaluate(() => window.airspace.replay.seek(window.airspace.replay.start + 60_000));
  await moveCamera(page, 14, 47, 600_000);
  await page.locator('#rp-key').click();
}

test.describe('video export', () => {
  test.setTimeout(300_000);

  test('exports a clip on a new camera path that a standard player opens', async ({ stack, page }) => {
    await setup(stack, page, 'imagery=off');
    const attempts = () => (stack.logs.match(/upstream poll/g) ?? []).length;
    const attemptsBefore = attempts();
    await placeKeyframes(page);
    const { file, last, name } = await exportClip(page, stack.dir, 'clip.mp4',
      { from: 0, to: 60, speed: 10, size: '720p', fps: 30, camera: 'keyframes', outline: true, clock: true, attrib: true });
    expect(name).toMatch(/^airspace-\d{8}-\d{4}\.mp4$/);
    expect(last.frames).toBe(180); // 60 s at 10x is 6 s, at 30 fps
    expect(last.cancelled).toBe(false);

    const info = probe(file);
    expect(info.format).toContain('mp4');
    expect([info.width, info.height]).toEqual([1280, 720]);
    expect(info.r_frame_rate).toBe('30/1');
    expect(info.frames).toBe(180);
    expect(info.duration).toBeCloseTo(6, 1);
    expect(['h264', 'vp9', 'av1']).toContain(info.codec_name);
    expect(statSync(file).size).toBeGreaterThan(50_000);
    // ffmpeg decodes every frame without complaint.
    expect(frameHashes(file)).toHaveLength(180);
    // The video is not a still: the first and last frames differ.
    const h = frameHashes(file);
    expect(h[0]).not.toBe(h[179]);

    // Export left the replay as it was, and the page still works.
    const r = await replayDebug(page);
    expect(r.active).toBe(true);
    expect(r.time).toBe(r.start + 60_000);
    await expect(page.locator('#replay-bar')).toBeVisible();
    // Replay and export made no attempt to reach OpenSky.
    expect(attempts()).toBe(attemptsBefore);
    test.info().annotations.push({ type: 'codec', description: `${last.codec} (${info.codec_name})` });
    console.log(`exported with ${last.codec}: ${info.codec_name} ${info.width}x${info.height}`);
  });

  test('output is identical from one run to the next', async ({ stack, page }) => {
    await setup(stack, page, 'imagery=off');
    await placeKeyframes(page);
    const opts = { from: 5, to: 35, speed: 10, size: '720p', fps: 24, camera: 'keyframes', outline: true, clock: true, attrib: false };
    await page.evaluate(() => { window.airspace.exporter.hashFrames = true; });
    const a = await exportClip(page, stack.dir, 'a.mp4', opts);
    // Load the machine during the second run: timing must not matter.
    await page.evaluate(() => { window.busy = setInterval(() => { const t = performance.now(); while (performance.now() - t < 12); }, 20); });
    const b = await exportClip(page, stack.dir, 'b.mp4', opts);
    await page.evaluate(() => clearInterval(window.busy));
    // Every frame drawn is the same, pixel for pixel.
    expect(a.last.hashes).toHaveLength(72);
    expect(new Set(a.last.hashes).size).toBeGreaterThan(60); // and the frames are not all alike
    expect(b.last.hashes).toEqual(a.last.hashes);
    // The encoded files hold the same number of frames. Hardware encoders
    // are not bit-exact from run to run, so the bytes are only reported.
    const ha = frameHashes(a.file), hb = frameHashes(b.file);
    expect(hb).toHaveLength(ha.length);
    console.log(`decoded frames identical: ${ha.filter((h, i) => h === hb[i]).length} of ${ha.length}`);
  });

  test('can be cancelled, and says so when WebCodecs is missing', async ({ stack, page, context }) => {
    await setup(stack, page, 'imagery=off');
    await page.locator('#rp-export').click();
    await page.locator('#ex-speed').selectOption('1');
    await page.locator('#ex-size').selectOption('720p');
    await page.locator('#ex-to').fill('600');
    await expect(page.locator('#ex-msg')).toContainText('frames');
    await page.locator('#ex-start').click();
    await expect(page.locator('#ex-progress')).toBeVisible();
    await expect(page.locator('#ex-msg')).toContainText('Frame');
    await expect(page.locator('body')).toHaveClass(/exporting/);
    await page.locator('#ex-cancel').click();
    await expect(page.locator('#ex-msg')).toHaveText('Export cancelled.');
    await expect(page.locator('body')).not.toHaveClass(/exporting/);
    const last = await page.evaluate(() => window.airspace.exporter.last);
    expect(last.cancelled).toBe(true);
    expect(last.frames).toBeLessThan(last.total);
    await page.locator('#ex-cancel').click();
    await page.locator('#rp-play').click(); // replay still runs
    await sleep(500);
    expect((await replayDebug(page)).time).toBeGreaterThan((await replayDebug(page)).start);

    // A browser with no WebCodecs is told so, and gets no fallback.
    const old = await context.newPage();
    await old.addInitScript(() => { delete window.VideoEncoder; });
    await openApp(old, stack);
    await old.locator('#btn-sessions').click();
    await old.locator('#sessions .session [data-do=replay]').first().click();
    await old.locator('#rp-export').click();
    await expect(old.locator('#ex-unsupported')).toContainText('no WebCodecs video encoder');
    await expect(old.locator('#ex-start')).toBeDisabled();
  });

  // The acceptance clip: 30 seconds of 1080p over real imagery. It takes a
  // few minutes and needs internet for NASA GIBS, so it is opt-in:
  // AIRSPACE_FULL=1 npx playwright test export
  test('acceptance: a 30 second 1080p clip over NASA imagery', async ({ stack, page }) => {
    test.skip(!process.env.AIRSPACE_FULL, 'set AIRSPACE_FULL=1 to run');
    test.setTimeout(1_500_000);
    await setup(stack, page, '');
    await placeKeyframes(page);
    const { file, last } = await exportClip(page, process.env.AIRSPACE_OUT ?? stack.dir, 'airspace-acceptance.mp4',
      { from: 0, to: 300, speed: 10, size: '1080p', fps: 30, camera: 'keyframes', outline: true, clock: true, attrib: true });
    const info = probe(file);
    expect([info.width, info.height]).toEqual([1920, 1080]);
    expect(info.frames).toBe(900);
    expect(info.duration).toBeCloseTo(30, 1);
    expect(last.slowTiles).toBe(0);
    console.log(`acceptance clip: ${file}, ${last.codec}, ${statSync(file).size} bytes`);
  });
});
