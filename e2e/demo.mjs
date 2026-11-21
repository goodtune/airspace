// Render the README demo clip from the captured test data. No OpenSky calls.
import { chromium } from '@playwright/test';
import { Stack } from './helpers.js';
const out = process.argv[2];
const stack = new Stack();
try {
  stack.seed({ name: 'Whole world' });
  await stack.startApp();
  const b = await chromium.launch({ args: ['--use-angle=metal', '--enable-gpu', '--ignore-gpu-blocklist'] });
  const p = await b.newPage({ viewport: { width: 1280, height: 720 } });
  p.on('pageerror', (e) => console.log('pageerror', e.message));
  await p.goto(`${stack.url}/`);
  await p.waitForFunction(() => window.airspace?.ready && window.airspace.replay && window.airspace.debug().clientId, null, { timeout: 20000 });
  await p.locator('#btn-sessions').click();
  await p.locator('#sessions .session [data-do=replay]').first().click();
  await p.waitForFunction(() => { const d = window.airspace.replay.debug(); return d.active && !d.buffering && d.aircraft > 0; }, null, { timeout: 30000 });
  await p.evaluate(() => {
    const r = window.airspace.replay, k = (s, lon, lat, height) => ({ time: r.start + s * 1000, lon, lat, height, heading: 0, pitch: -90, outside: false });
    r.keyframes = [k(0, 20, 42, 1.5e7), k(90, 18, 44, 1.3e7), k(480, 8.7, 50.0, 7.5e5), k(720, 9.3, 50.2, 5.5e5)];
    r.setCameraMode('keyframes');
  });
  await p.locator('#rp-export').click();
  await p.locator('#ex-from').fill('0');
  await p.locator('#ex-to').fill('720');
  await p.locator('#ex-speed').selectOption('60');
  await p.locator('#ex-size').selectOption('720p');
  await p.locator('#ex-fps').selectOption('30');
  await p.locator('#ex-camera').selectOption('keyframes');
  const [dl] = await Promise.all([p.waitForEvent('download', { timeout: 500000 }), p.locator('#ex-start').click()]);
  await dl.saveAs(out);
  console.log(JSON.stringify({ ...(await p.evaluate(() => window.airspace.exporter.last)), hashes: undefined }));
  await b.close();
} finally {
  await stack.close();
}
