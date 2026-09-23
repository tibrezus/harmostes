// E2E scenario 4 — the live position: a mid-flight attempt shows exactly one
// running node with its pulse, and the waterfall never invents unsettled
// work.
import { test, expect } from '@playwright/test';

test('running run detail: live position on the agent node, settled lanes only', async ({ page }) => {
  await page.goto('/runs/attempt-pr-review-demo-43c2');

  const nodes = page.getByTestId('graph-node');
  // 5 = virtual trigger + the four graph nodes (#541).
  await expect(nodes).toHaveCount(5);

  await expect(page.locator('.rg-node--running')).toHaveCount(1);
  await expect(page.locator('.rg-node--ok')).toHaveCount(1);
  await expect(page.locator('.rg-node--pending')).toHaveCount(2);

  // The pulse rides the running node — the "where is it" answer.
  await expect(page.locator('.rg-pulse')).toHaveCount(1);

  // Settled work PLUS the live lane: the fixture has a single prepare/ok
  // envelope and the agent is in flight — exactly two lanes: prepare
  // (settled) + agent (live, growing, pulsing). A pass-on-nothing loop
  // cannot catch a lane regrowing; the count pins it.
  await expect(page.getByTestId('timing-lane')).toHaveCount(2);
  await expect(page.locator('[data-testid="timing-lane"][data-label="prepare"]')).toHaveCount(1);
  const liveBar = page.locator('.rg-timing-live');
  await expect(liveBar).toHaveCount(1);
  const title = await liveBar.locator('title').textContent();
  expect(title).toMatch(/in flight/);
});
