// E2E scenario 1 — the wall answers "what is running, in what workflow,
// where is it" with no clicks (milestone acceptance criterion).
//
// The fixture world (#554): three review subjects (⟡) and one deterministic
// subject — four cards, each linking straight to its latest run. The
// template → workflow → subject organization has its own spec
// (wall-sections.spec.ts).
import { test, expect } from '@playwright/test';

test('the wall renders every fixture subject with review marks and direct links', async ({ page }) => {
  await page.goto('/');

  const cards = page.getByTestId('wall-card');
  await expect(cards).toHaveCount(4);

  // Review subjects are marked on their row; the deterministic subject is one row too.
  await expect(page.locator('[data-testid="wall-card"][data-review="true"]')).toHaveCount(3);
  await expect(page.getByTestId('wall-card-title').locator('.ids')).toHaveCount(1);

  // Every card links directly to its latest run — no clicks to find it.
  const subjects = ['demo-rezuscloud/harmostes#42', 'demo-rezuscloud/harmostes#43', 'demo-rezuscloud/harmostes#44', 'demo-rezuscloud/harmostes'];
  for (const subject of subjects) {
    await expect(page.locator(`[data-testid="wall-card"][data-subject="${subject}"]`)).toHaveCount(1);
  }
  await expect(page.locator('[data-testid="wall-card"][data-subject="demo-rezuscloud/harmostes#42"] [data-testid="wall-card-title"]'))
    .toHaveAttribute('href', '/runs/attempt-pr-review-demo-42a1');
});
