import { expect, test } from '@playwright/test';

// The wall header carries a kestra-style state tally: one chip per live
// state, counted over the whole live selection (pre-budget). Aged history
// and superseded rows are never counted — the wall counts LIVE work.
test('the wall header tallies live states', async ({ page }) => {
  await page.goto('/');

  const counts = page.getByTestId('wall-counts');
  await expect(counts).toBeVisible();
  // Fixture world: one in-flight review (#43) + two fresh verdicts (#42, #44).
  await expect(counts.locator('.chip--run')).toHaveText('in flight 1');
  await expect(counts.locator('.chip--ok')).toHaveText('verdict 2');
  await expect(counts.locator('.chip--fail')).toHaveCount(0);
  await expect(counts.locator('.chip--warn')).toHaveCount(0);
});

// The in-flight row streams the executing run's usage: the attempt's
// Progress window renders as the live token cell (↻ marker, turns in the
// title). Terminal rows never carry it.
test('the wall streams live token counts on the in-flight row', async ({ page }) => {
  await page.goto('/');

  const live = page.locator('[data-testid="wall-live-tokens"]');
  await expect(live).toHaveCount(1);
  const card = page.locator('[data-testid="wall-card"][data-subject="demo-rezuscloud/harmostes#43"]');
  await expect(card.locator('[data-testid="wall-live-tokens"]')).toHaveText('↻ ↑2140 ↓388');
  await expect(card.locator('[data-testid="wall-live-tokens"]')).toHaveAttribute('title', 'live · 4 turns');
});
