import { defineConfig } from '@playwright/test';

// The tests run the real airspace binary against a fake OpenSky that counts
// every request. They never call the real OpenSky API.
// WebGL needs a renderer. A Mac has a GPU; a CI machine does not, so there
// Chromium draws in software.
const gpuArgs = process.platform === 'darwin'
  ? ['--use-angle=metal', '--enable-gpu', '--ignore-gpu-blocklist']
  : ['--use-angle=swiftshader', '--enable-unsafe-swiftshader', '--ignore-gpu-blocklist'];

export default defineConfig({
  testDir: './tests',
  workers: 1, // each test drives WebGL pages; one at a time keeps timing honest
  timeout: 120_000,
  expect: { timeout: 15_000 },
  reporter: [['list']],
  outputDir: '.tmp/results',
  use: {
    viewport: { width: 1280, height: 800 },
    launchOptions: { args: gpuArgs },
  },
});
