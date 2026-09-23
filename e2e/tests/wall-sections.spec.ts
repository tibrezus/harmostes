// E2E scenario (#554) — the live wall reads template → workflow → subject:
// sections by owning template, each workflow block carrying its compiled
// shape as a step-timing strip (widths ∝ the latest attempt's wall clock),
// and its live subjects beside the strip in a rowspan cell.
//
// The fixture world provides exactly one template-backed instance
// (pr-review → pr-review-instance) and two graph-native workflows
// (pr-review-demo with two live PRs, merge-sync-demo) — enough to pin the
// whole hierarchy without a cluster.
import { test, expect } from '@playwright/test';

test('the wall sections by template and each workflow carries a step-timing strip', async ({ page }) => {
  await page.goto('/');

  // Two sections: the template-backed instance's class and the
  // graph-native remainder.
  const sections = page.getByTestId('wall-section');
  await expect(sections).toHaveCount(2);

  const prSec = page.locator('[data-testid="wall-section"][data-template="pr-review"]');
  await expect(prSec).toBeVisible();
  await expect(prSec).toContainText('pr-review-instance');
  await expect(prSec.getByTestId('wall-card')).toHaveCount(1);

  const otherSec = page.locator('[data-testid="wall-section"][data-template="other workflows"]');
  // Live selection: the superseded merge-sync subject never renders.
  await expect(otherSec.getByTestId('wall-card')).toHaveCount(2);

  // Subject rows fold under their workflow: pr-review-demo tracks two
  // live PRs → its block cell spans both rows and names the workflow.
  const demoCell = otherSec
    .locator('[data-testid="wall-workflow-cell"]')
    .filter({ has: page.getByTestId('wall-workflow-link').filter({ hasText: 'pr-review-demo' }) });
  await expect(demoCell).toHaveCount(1);
  await expect(demoCell).toHaveAttribute('rowspan', '2');

  // Every live workflow shows its step-timing strip; segments paint with
  // the same state classes as the run-detail waterfall.
  const strips = page.getByTestId('wall-steps');
  await expect(strips).toHaveCount(2);
  await expect(strips.first().locator('rect')).not.toHaveCount(0);

  // The demo strip's segments sit in dependency order (x is increasing).
  const xs = await demoCell
    .getByTestId('wall-steps')
    .locator('rect')
    .evaluateAll((rects) => rects.map((r) => Number(r.getAttribute('x'))));
  expect(xs).toHaveLength(4); // prepare → agent → gate → deploy
  for (let i = 1; i < xs.length; i++) expect(xs[i]).toBeGreaterThan(xs[i - 1]);

  // Segments carry honest titles (label · duration · state glyph).
  const firstTitle = await demoCell
    .getByTestId('wall-steps')
    .locator('rect title')
    .first()
    .textContent();
  expect(firstTitle).toMatch(/prepare/);
});

test('workflow blocks and subject rows both drill down', async ({ page }) => {
  await page.goto('/');

  // Workflow cell → workflow detail.
  const wfLinks = page.getByTestId('wall-workflow-link');
  await expect(wfLinks.first()).toHaveAttribute('href', /^\/workflows\//);

  // Subject row → the latest run.
  const subjectLink = page.getByTestId('wall-card-title').first();
  await expect(subjectLink).toHaveAttribute('href', /^\/runs\//);
});
