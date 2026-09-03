// E2E scenario 2 — the runs spine: list, phases, navigation into a run.
import { test, expect } from '@playwright/test';

test('the runs list surfaces all phases and navigates into a run', async ({ page }) => {
  await page.goto('/runs');

  // v2 console list: attempts live in collapsed sub-rows, rendered with
  // short human labels (workflow · hash). The DOM carries them all.
  // The filter tabs carry the window summary: total, failed, in flight.
  await expect(page.getByTestId('tab-all')).toHaveText(/All\s*3/);
  await expect(page.getByTestId('tab-failed')).toHaveText(/Failed\s*0/);
  await expect(page.getByTestId('tab-inflight')).toHaveText(/In flight\s*2/);

  const links = page.getByTestId('run-link');
  await expect(links.filter({ hasText: 'pr-review-demo · 42a1' })).toHaveCount(1);
  await expect(links.filter({ hasText: 'pr-review-demo · 43c2' })).toHaveCount(1);
  await expect(links.filter({ hasText: 'merge-sync-demo · e5f6' })).toHaveCount(1);

  // The fourth terminal phase is first-class in the list (phase rides the
  // data-phase attribute; the anchor text is the run name).
  for (const phase of ['validated', 'reconciling', 'superseded']) {
    await expect(page.locator(`[data-testid="run-link"][data-phase="${phase}"]`)).toHaveCount(1);
  }

  // Navigation: expand the link's own group, then click through.
  const link = page.getByTestId('run-link').filter({ hasText: 'pr-review-demo · 42a1' });
  const group = await link.evaluate((el) => el.closest('tr').dataset.group);
  await page.locator(`.tbl .exp[data-group="${group}"]`).click();
  await link.click();
  await expect(page).toHaveURL(/\/runs\/attempt-pr-review-demo-42a1$/);
  await expect(page.getByTestId('graph-node')).toHaveCount(4);
});
