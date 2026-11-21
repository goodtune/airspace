import { defineConfig } from '@playwright/test';

// The tests run the real airspace binary against a fake OpenSky that counts
// every request. They never call the real OpenSky API.
// WebGL needs a renderer. A Mac has a GPU; a CI machine does not, so there
// Chromium draws in software.
const gpuArgs = process.platform === 'darwin'
  ? ['--use-angle=metal', '--enable-gpu', '--ignore-gpu-blocklist']
  : ['--use-angle=swiftshader', '--enable-unsafe-swiftshader', '--ignore-gpu-blocklist'];

// Software rendering is slow, so CI draws a smaller page. The shape is the
// same, which keeps what the camera sees, and so the query costs, unchanged.
const small = process.platform !== 'darwin' || !!process.env.AIRSPACE_E2E_SMALL;
const viewport = small ? { width: 960, height: 600 } : { width: 1280, height: 800 };

export default defineConfig({
  testDir: './tests',
  workers: 1, // each test drives WebGL pages; one at a time keeps timing honest
  timeout: 120_000,
  expect: { timeout: 15_000 },
  reporter: [['list']],
  outputDir: '.tmp/results',
  use: {
    viewport,
    launchOptions: { args: gpuArgs },
  },
});
