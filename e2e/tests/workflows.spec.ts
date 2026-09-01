// E2E scenario 6 — the workflow catalog: read-only reference for the GitOps
// YAML, with the compiled graph per workflow.
import { test, expect } from '@playwright/test';

test('the workflow catalog lists both fixture workflows and renders their graphs', async ({ page }) => {
  await page.goto('/workflows');

  const body = page.locator('body');
  await expect(body).toContainText('pr-review-demo');
  await expect(body).toContainText('merge-sync-demo');

  // Detail: the compiled graph of the review workflow (node labels render
  // uppercase).
  await page.goto('/workflows/pr-review-demo');
  await expect(page.locator('.pg-node-label').filter({ hasText: 'PREPARE' })).toHaveCount(1);
  await expect(page.locator('.pg-node-label').filter({ hasText: 'DEPLOY' })).toHaveCount(1);
  await expect(page.locator('.pg-node--agent')).toHaveCount(1);
});
