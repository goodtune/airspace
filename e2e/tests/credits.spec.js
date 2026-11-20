import { test, expect, openApp, debug, sleep, moveCamera } from '../helpers.js';

test.describe('credits and interval', () => {
  test('cost and interval floor follow the view', async ({ stack, page }) => {
    await stack.startFake();
    await stack.startApp({ POLL_INTERVAL: '10s' });
    await openApp(page, stack);

    // A region of 25 square degrees or less: 1 credit, and 5 s is allowed.
    await expect(page.locator('#ro-cost')).toHaveText('1 credit');
    await expect(page.locator('#ro-bounds')).toHaveText('minimum 5 s');
    await page.locator('#interval').fill('5');
    await page.locator('#interval').press('Enter');
    await expect.poll(async () => (await stack.status()).interval.seconds).toBe(5);
    await expect(page.locator('#ro-reason')).toBeHidden();

    // Zooming out raises the cost and, where the floor requires it, the interval.
    await moveCamera(page, 8.5, 50, 1_500_000);
    await expect(page.locator('#ro-cost')).toHaveText('3 credits');
    await expect(page.locator('#interval')).toHaveValue('6');
    await expect(page.locator('#ro-reason')).toHaveText('operator minimum');
    await expect(page.locator('#ro-bounds')).toHaveText('minimum 6 s');

    await moveCamera(page, 8.5, 50, 9_000_000);
    await expect(page.locator('#ro-cost')).toHaveText('4 credits');
    await expect(page.locator('#interval')).toHaveValue('8');
    await expect(page.locator('#ro-scope')).toHaveText('whole world');

    // Zooming back in restores the interval that was asked for.
    await moveCamera(page, 8.5, 50, 300_000);
    await expect(page.locator('#ro-cost')).toHaveText('1 credit');
    await expect(page.locator('#interval')).toHaveValue('5');
    await expect(page.locator('#ro-reason')).toBeHidden();

    // The burn rate and forecast match the arithmetic: 1 credit every 5 s.
    await expect(page.locator('#ro-burn')).toHaveText('720 / hour');
    const s = await stack.status();
    expect(s.credits.secondsLeft).toBeCloseTo((s.credits.remaining / 720) * 3600, 0);
    await expect(page.locator('#ro-exhaust')).not.toHaveText('–');
  });

  test('a view never costs more than 4 credits, and a camera move costs nothing extra', async ({ stack, page }) => {
    await stack.startFake();
    await stack.startApp();
    await openApp(page, stack);
    await expect.poll(async () => (await stack.stats()).stateRequests).toBe(1);
    const before = await stack.stats();
    for (let i = 0; i < 12; i++) {
      await moveCamera(page, -170 + i * 30, (i % 5) * 15 - 30, 200_000 + i * 700_000);
      await sleep(120);
      expect((await stack.status()).scope.cost).toBeLessThanOrEqual(4);
    }
    // All that movement took under one interval: no extra poll happened.
    expect((await stack.stats()).stateRequests - before.stateRequests).toBeLessThanOrEqual(1);
  });

  test('global coverage shows its cost before it takes effect', async ({ stack, page, context }) => {
    await stack.startFake();
    await stack.startApp();
    const a = await openApp(page, stack);
    const b = await openApp(await context.newPage(), stack);
    await a.locator('#cov-global').click();
    await expect(a.locator('#confirm-global')).toBeVisible();
    await expect(a.locator('#confirm-global')).toContainText('4 credits');
    await expect(a.locator('#confirm-global')).toContainText('1,800 credits an hour'); // 4 credits every 8 s
    expect((await stack.status()).coverage).toBe('viewport'); // nothing has changed yet
    await a.locator('#confirm-global-yes').click();

    for (const p of [a, b]) {
      await expect(p.locator('.chip.global')).toBeVisible();
      await expect(p.locator('#ro-cost')).toHaveText('4 credits');
      await expect(p.locator('#ro-bounds')).toHaveText('minimum 8 s');
      await expect(p.locator('#interval')).toHaveValue('8');
    }
    await b.locator('#cov-viewport').click();
    await expect(a.locator('.chip.global')).toBeHidden();
    await expect(a.locator('#ro-cost')).toHaveText('1 credit');
  });

  test('MIN_INTERVAL is the lower limit in every browser', async ({ stack, page, context }) => {
    await stack.startFake();
    await stack.startApp({ MIN_INTERVAL: '30s', MAX_INTERVAL: '2m', POLL_INTERVAL: '10s' });
    expect(stack.logs).toContain('clamped to 30s');
    const a = await openApp(page, stack);
    const b = await openApp(await context.newPage(), stack);
    for (const p of [a, b]) {
      await expect(p.locator('#ro-bounds')).toHaveText('allowed 30 to 120 s');
      await expect(p.locator('#interval')).toHaveAttribute('min', '30');
      await expect(p.locator('#interval')).toHaveValue('30');
    }
    await a.locator('#interval').fill('7');
    await a.locator('#interval').press('Enter');
    await expect(b.locator('#ro-reason')).toHaveText('operator minimum');
    await expect(b.locator('#interval')).toHaveValue('30');
    await b.locator('#interval').fill('500');
    await b.locator('#interval').press('Enter');
    await expect(a.locator('#interval')).toHaveValue('120');
    await expect(a.locator('#ro-reason')).toHaveText('operator maximum');
  });

  test('a low balance stretches the interval and warns', async ({ stack, page }) => {
    await stack.startFake({ credits: 101 });
    await stack.startApp({ POLL_INTERVAL: '5s' });
    await openApp(page, stack);
    await expect(page.locator('#ro-reason')).toHaveText('credit reserve');
    await expect(page.locator('#interval')).toHaveValue('10'); // 100 left of a 200 reserve: doubled
    await expect(page.locator('#ro-low')).toBeVisible();
    await expect(page.locator('#ro-credits')).toHaveText('100');
  });

  test('a rate limit is shown and obeyed', async ({ stack, page }) => {
    await stack.startFake({ credits: 0 });
    await stack.startApp();
    await openApp(page, stack);
    await expect(page.locator('#ro-reason')).toHaveText('rate limited');
    const n = (await stack.stats()).stateRequests;
    await sleep(8000); // the fake asks for a 60 s wait
    expect((await stack.stats()).stateRequests).toBe(n);
  });
});

test.describe('activity rules', () => {
  test('an idle session pauses, and a click resumes it', async ({ stack, page }) => {
    await stack.startFake();
    await stack.startApp({ IDLE_TIMEOUT: '4s' });
    await openApp(page, stack);
    await expect(page.locator('#paused')).toBeVisible({ timeout: 10000 });
    await expect(page.locator('#mode-chip')).toHaveText('PAUSED');
    await sleep(500);
    const n = await stack.upstream();
    await sleep(7000);
    expect(await stack.upstream()).toBe(n);
    await page.locator('#paused').click();
    await expect(page.locator('#paused')).toBeHidden();
    await expect.poll(() => stack.upstream()).toBeGreaterThan(n);
  });

  test('a hidden tab stops counting after HIDDEN_TIMEOUT', async ({ stack, page }) => {
    await stack.startFake();
    await stack.startApp({ HIDDEN_TIMEOUT: '2s' });
    await openApp(page, stack);
    await expect.poll(() => stack.upstream()).toBeGreaterThan(0);
    // Headless pages are never really hidden, so report it the way the page does.
    await page.evaluate(() => window.act('presence', { visible: false, replay: false, detached: false }));
    expect((await stack.status()).status.active).toBe(1); // still inside the timeout
    await expect.poll(async () => (await stack.status()).status.active, { timeout: 5000 }).toBe(0);
    await sleep(500);
    const n = await stack.upstream();
    await sleep(7000);
    expect(await stack.upstream()).toBe(n);
    await page.evaluate(() => window.act('presence', { visible: true, replay: false, detached: false }));
    await expect.poll(() => stack.upstream()).toBeGreaterThan(n);
    expect((await debug(page)).mode).toBe('live');
  });
});
