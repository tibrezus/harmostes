// E2E scenario 2 — the runs spine: list, phases, navigation into a run.
import { test, expect } from '@playwright/test';

test('the runs list surfaces all phases and navigates into a run', async ({ page }) => {
  await page.goto('/runs');

  const links = page.getByTestId('run-link');
  await expect(links.filter({ hasText: 'attempt-pr-review-demo-42a1' })).toHaveCount(1);
  await expect(links.filter({ hasText: 'attempt-pr-review-demo-43c2' })).toHaveCount(1);
  await expect(links.filter({ hasText: 'attempt-merge-sync-demo-e5f6' })).toHaveCount(1);

  // The fourth terminal phase is first-class in the list (phase rides the
  // data-phase attribute; the anchor text is the run name).
  for (const phase of ['validated', 'reconciling', 'superseded']) {
    await expect(page.locator(`[data-testid="run-link"][data-phase="${phase}"]`)).toHaveCount(1);
  }

  // Navigation: click the terminal review run, land on its detail page.
  await page.getByTestId('run-link').filter({ hasText: 'attempt-pr-review-demo-42a1' }).click();
  await expect(page).toHaveURL(/\/runs\/attempt-pr-review-demo-42a1$/);
  await expect(page.getByTestId('graph-node')).toHaveCount(4);
});
