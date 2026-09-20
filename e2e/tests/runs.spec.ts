// E2E scenario 2 — the runs spine: list, phases, navigation into a run.
import { test, expect } from '@playwright/test';

test('the runs list surfaces all phases and navigates into a run', async ({ page }) => {
  await page.goto('/runs');

  // v2 console list: attempts live in collapsed sub-rows, rendered with
  // short human labels (workflow · hash). The DOM carries them all.
  // The filter tabs carry the window summary: total, failed, in flight.
  // Fixture world: #42 and #44 completed their reviews (verdicts →
  // history), #43 is the live one, merge-sync is superseded.
  await expect(page.getByTestId('tab-all')).toHaveText(/All\s*4/);
  await expect(page.getByTestId('tab-failed')).toHaveText(/Failed\s*0/);
  await expect(page.getByTestId('tab-inflight')).toHaveText(/In flight\s*1/);
  await expect(page.getByTestId('tab-verdicts')).toHaveText(/Verdicts\s*2/);

  const links = page.getByTestId('run-link');
  await expect(links.filter({ hasText: 'pr-review-demo · 42a1' })).toHaveCount(1);
  await expect(links.filter({ hasText: 'pr-review-demo · 43c2' })).toHaveCount(1);
  await expect(links.filter({ hasText: 'merge-sync-demo · e5f6' })).toHaveCount(1);

  // The fourth terminal phase is first-class in the list (phase rides the
  // data-phase attribute; the anchor text is the run name).
  await expect(page.locator('[data-testid="run-link"][data-phase="validated"]')).toHaveCount(2);
  await expect(page.locator('[data-testid="run-link"][data-phase="reconciling"]')).toHaveCount(1);
  await expect(page.locator('[data-testid="run-link"][data-phase="superseded"]')).toHaveCount(1);

  // Navigation: expand the link's own group, then click through.
  const link = page.getByTestId('run-link').filter({ hasText: 'pr-review-demo · 42a1' });
  const group = await link.evaluate((el) => el.closest('tr').dataset.group);
  await page.locator(`.tbl .exp[data-group="${group}"]`).click();
  await link.click();
  await expect(page).toHaveURL(/\/runs\/attempt-pr-review-demo-42a1$/);
  // 5 = the virtual trigger + prepare/agent/gate/deploy (#541).
  await expect(page.getByTestId('graph-node')).toHaveCount(5);
  // The identity cards carry workflow semantics: the trigger names its
  // kind, the agent card carries the fix-loop budget.
  await expect(page.locator('[data-node="trigger"] .rg-title')).toContainText('webhook');
  await expect(page.locator('[data-node="agent"]')).toContainText('maxFixes');
  await expect(page.getByTestId('trigger-edge')).toHaveCount(1);
});
