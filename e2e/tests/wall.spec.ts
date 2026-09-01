// E2E scenario 1 — the wall answers "what is running, in what workflow,
// where is it" with no clicks (milestone acceptance criterion).
//
// The fixture world: two review subjects (⟡) and one deterministic subject —
// three cards, each linking straight to its latest run.
import { test, expect } from '@playwright/test';

test('the wall renders every fixture subject with review marks and direct links', async ({ page }) => {
  await page.goto('/');

  const cards = page.getByTestId('wall-card');
  await expect(cards).toHaveCount(3);

  // Review subjects carry the ⟡ mark in their title; deterministic ones render as code.
  const titles = page.getByTestId('wall-card-title');
  await expect(titles.filter({ hasText: '⟡' })).toHaveCount(2);
  await expect(titles.locator('code')).toHaveCount(1);

  // Every card links directly to its latest run — no clicks to find it.
  const subjects = ['demo-rezuscloud/harmostes#42', 'demo-rezuscloud/harmostes#43', 'demo-rezuscloud/harmostes'];
  for (const subject of subjects) {
    await expect(page.locator(`[data-testid="wall-card"][data-subject="${subject}"]`)).toHaveCount(1);
  }
  await expect(page.locator('[data-testid="wall-card"][data-subject="demo-rezuscloud/harmostes#42"]'))
    .toHaveAttribute('href', '/runs/attempt-pr-review-demo-42a1');
});
