import { defineConfig } from '@playwright/test';

// The tests run the real airspace binary against a fake OpenSky that counts
// every request. They never call the real OpenSky API.
export default defineConfig({
  testDir: './tests',
  workers: 1, // each test drives WebGL pages; one at a time keeps timing honest
  timeout: 120_000,
  expect: { timeout: 15_000 },
  reporter: [['list']],
  outputDir: '.tmp/results',
  use: {
    viewport: { width: 1280, height: 800 },
    launchOptions: { args: ['--use-angle=metal', '--enable-gpu', '--ignore-gpu-blocklist', '--enable-unsafe-swiftshader'] },
  },
});
