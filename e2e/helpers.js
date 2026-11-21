// Test harness: starts a fake OpenSky and the real airspace binary.

import { spawn, execFileSync } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { test as base, expect } from '@playwright/test';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');
export const CAPTURE = join(root, 'testdata/capture/global-2026-11-21.jsonl.gz');
const BIN = join(root, 'bin/airspace');
const DEVTOOL = join(root, 'bin/devtool');

function freePort() {
  return new Promise((resolve, reject) => {
    const s = createServer();
    s.once('error', reject);
    s.listen(0, '127.0.0.1', () => {
      const { port } = s.address();
      s.close(() => resolve(port));
    });
  });
}

async function waitForHTTP(url, what) {
  for (let i = 0; i < 200; i++) {
    try {
      if ((await fetch(url)).ok) return;
    } catch { /* not up yet */ }
    await new Promise((r) => setTimeout(r, 50));
  }
  throw new Error(`${what} did not start at ${url}`);
}

function stop(child) {
  return new Promise((resolve) => {
    if (!child || child.exitCode !== null) return resolve();
    child.once('exit', resolve);
    child.kill('SIGTERM');
    setTimeout(() => child.kill('SIGKILL'), 3000);
  });
}

// A Stack is one fake upstream plus one app, with its own database.
export class Stack {
  constructor() {
    this.dir = mkdtempSync(join(tmpdir(), 'airspace-e2e-'));
    this.db = join(this.dir, 'airspace.db');
    this.logs = '';
  }

  // capture: replay the captured global traffic instead of synthetic data.
  async startFake({ capture = false, credits = 4000 } = {}) {
    this.fakePort = await freePort();
    const args = ['fake', '-addr', `127.0.0.1:${this.fakePort}`, '-credits', String(credits)];
    if (capture) args.push('-capture', CAPTURE);
    this.fake = spawn(DEVTOOL, args, { stdio: 'ignore' });
    this.fakeURL = `http://127.0.0.1:${this.fakePort}`;
    await waitForHTTP(`${this.fakeURL}/_stats`, 'fake OpenSky');
  }

  async stopFake() {
    await stop(this.fake);
    this.fake = null;
  }

  async startApp(env = {}) {
    this.port = await freePort();
    this.url = `http://127.0.0.1:${this.port}`;
    this.app = spawn(BIN, [], {
      env: {
        PATH: process.env.PATH,
        OPENSKY_CLIENT_ID: 'e2e-client', OPENSKY_CLIENT_SECRET: Stack.SECRET,
        OPENSKY_TOKEN_URL: `${this.fakeURL ?? 'http://127.0.0.1:9'}/token`,
        OPENSKY_API_URL: `${this.fakeURL ?? 'http://127.0.0.1:9'}/api`,
        LISTEN_ADDR: `127.0.0.1:${this.port}`, DB_PATH: this.db,
        POLL_INTERVAL: '5s', START_VIEW: '8.5,50,300000',
        ...env,
      },
      stdio: ['ignore', 'ignore', 'pipe'],
    });
    this.app.stderr.on('data', (d) => { this.logs += d; });
    await waitForHTTP(`${this.url}/healthz`, 'airspace');
  }

  async stopApp() {
    await stop(this.app);
    this.app = null;
  }

  // Write the captured traffic into the database as a recorded session.
  seed({ bbox, name } = {}) {
    const args = ['seed', '-db', this.db, '-capture', CAPTURE];
    if (bbox) args.push('-bbox', bbox);
    if (name) args.push('-name', name);
    execFileSync(DEVTOOL, args, { stdio: 'ignore' });
  }

  async stats() {
    return (await fetch(`${this.fakeURL}/_stats`)).json();
  }

  // Total upstream requests of any kind.
  async upstream() {
    const s = await this.stats();
    return s.tokenRequests + s.stateRequests;
  }

  async setFailing(on) {
    await fetch(`${this.fakeURL}/_failing?on=${on ? 1 : 0}`);
  }

  async setRemaining(n) {
    await fetch(`${this.fakeURL}/_remaining?n=${n}`);
  }

  async status() {
    return (await fetch(`${this.url}/api/status`)).json();
  }

  async close() {
    await this.stopApp();
    await this.stopFake();
    rmSync(this.dir, { recursive: true, force: true });
  }
}
Stack.SECRET = 'e2e-secret-must-never-appear';

export const test = base.extend({
  // A fresh, empty stack. Tests start the parts they need.
  stack: async ({}, use) => {
    const stack = new Stack();
    await use(stack);
    await stack.close();
  },
});
export { expect };

// Open the app with the plain grid imagery, so tests need no internet.
export async function openApp(page, stack, query = 'imagery=off') {
  await page.goto(`${stack.url}/?${query}`);
  await page.waitForFunction(() => window.airspace?.ready && window.airspace.debug().clientId);
  return page;
}

export const debug = (page) => page.evaluate(() => window.airspace.debug());
export const replayDebug = (page) => page.evaluate(() => window.airspace.replay.debug());
export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// Move the camera the way a user drag would end up: Cesium sees a camera
// change that no remote update caused.
export async function moveCamera(page, lon, lat, height) {
  await page.evaluate(([lo, la, h]) => {
    const C = window.Cesium;
    window.airspace.viewer.camera.setView({ destination: C.Cartesian3.fromDegrees(lo, la, h) });
  }, [lon, lat, height]);
}
