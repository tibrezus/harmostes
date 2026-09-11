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

// ADR-0012 §5: creation is a sanctioned surface again. The form renders from
// the fixture template catalog; submitting creates an owner-stamped instance
// and lands on its detail page (which renders the merged template shape).
test('the creation form creates a thin instance and lands on its detail page', async ({ page }) => {
  await page.goto('/workflows/new');

  const form = page.getByTestId('wf-new-form');
  await expect(form).toBeVisible();
  await expect(page.getByTestId('wf-new-template')).toHaveCount(1); // the fixture catalog: pr-review

  await page.getByTestId('wf-new-name').fill('e2e-created');
  await page.getByTestId('wf-new-submit').click();

  await expect(page).toHaveURL(/\/workflows\/e2e-created$/);
  await expect(page.locator('body')).toContainText('e2e-created');

  // The instance appears in the owner's catalog (owner-stamped by the server).
  await page.goto('/workflows');
  await expect(page.locator('body')).toContainText('e2e-created');
});

// ADR-0012 §2: /api/schema is a CRD projection, served as JSON — the one
// document the future editor island renders from.
test('GET /api/schema serves both CRD-derived schemas', async ({ request }) => {
  const res = await request.get('/api/schema');
  expect(res.status()).toBe(200);
  expect(res.headers()['content-type']).toContain('application/json');

  const body = await res.json();
  expect(body.workflow?.properties?.spec?.type).toBe('object');
  expect(body.workflowtemplate?.properties?.spec?.type).toBe('object');
});
