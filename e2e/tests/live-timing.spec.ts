// Live step timing: while an attempt is in flight, the executing node's
// lane grows to now and pulses (fixture attempt-...-43c2 is mid-flight).
import { test, expect } from '@playwright/test';

test('live waterfall shows the growing in-flight lane', async ({ page }) => {
  await page.goto('/runs/attempt-pr-review-demo-43c2');

  const graph = page.getByTestId('run-graph-section');
  await expect(graph).toBeVisible();

  const liveBar = graph.locator('.rg-timing-live');
  await expect(liveBar).toHaveCount(1);
  const title = await liveBar.locator('title').textContent();
  expect(title).toMatch(/in flight/);

  // The lane belongs to the executing node (fixture live position = agent).
  const label = await liveBar.evaluate((el) => el.closest('g')?.getAttribute('data-label'));
  expect(label).toMatch(/agent|gate|deploy|prepare/);
});
