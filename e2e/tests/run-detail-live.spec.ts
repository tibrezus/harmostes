// E2E scenario 4 — the live position: a mid-flight attempt shows exactly one
// running node with its pulse, and the waterfall never invents unsettled
// work.
import { test, expect } from '@playwright/test';

test('running run detail: live position on the agent node, settled lanes only', async ({ page }) => {
  await page.goto('/runs/attempt-pr-review-demo-43c2');

  const nodes = page.getByTestId('graph-node');
  await expect(nodes).toHaveCount(4);

  await expect(page.locator('.rg-node--running')).toHaveCount(1);
  await expect(page.locator('.rg-node--ok')).toHaveCount(1);
  await expect(page.locator('.rg-node--pending')).toHaveCount(2);

  // The pulse rides the running node — the "where is it" answer.
  await expect(page.locator('.rg-pulse')).toHaveCount(1);

  // Settled lanes only: the agent/gate/deploy nodes have no envelopes yet.
  const laneLabels = await page.getByTestId('timing-lane').evaluateAll((els) =>
    els.map((el) => el.getAttribute('data-label')));
  for (const label of laneLabels) {
    expect(['agent', 'gate', 'deploy'], `unsettled lane ${label} must not render`).not.toContain(label);
  }
});
