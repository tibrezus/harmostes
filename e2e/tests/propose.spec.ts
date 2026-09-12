import { test, expect } from '@playwright/test';
import { islandReady } from './helpers';

// Propose via MR (#420, ADR-0012 §5): the bridge itself is covered against a
// fake forge in the component tier; this tier pins the GLUE on the real
// page — the propose panel collects the island's CURRENT document (including
// unsaved inspector edits) and hands it to the bridge, then links the MR.

test('propose panel posts the island document and links the MR', async ({ page }) => {
  page.setExtraHTTPHeaders({ 'X-Harmostes-Dev-User': 'fixture-user' });

  // Intercept the bridge: capture the posted document, answer like a forge.
  let postedDocument = '';
  await page.route('**/templates/pr-review/propose', async (route) => {
    const body = route.request().postDataJSON() as { document: string };
    postedDocument = body.document;
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ url: 'https://git.example/mr/42', branch: 'harmostes-ui/pr-review-42' }),
    });
  });

  await page.goto('/templates/pr-review');
  await islandReady(page, 'code-island', 15000);
  await expect(page.getByTestId('propose-panel')).toBeVisible();
  await expect(page.getByTestId('propose-source')).toContainText('github:golden-owner/golden-repo');

  // An unsaved inspector edit must reach the bridge: the panel proposes the
  // island's CURRENT document, not the last-rendered one.
  await page.locator('input[name="agent.model"]').fill('proposed-model');
  await page.getByTestId('inspector-apply').click();
  await expect(page.getByTestId('inspector-status')).toContainText('applied 1 change');

  await page.getByTestId('propose-button').click();
  await expect(page.getByTestId('propose-status')).toContainText('merge request opened on branch harmostes-ui/pr-review-42');
  await expect(page.getByTestId('propose-link')).toHaveAttribute('href', 'https://git.example/mr/42');

  // The bridge received the EDITED document — the proposed model, not the
  // committed one.
  expect(postedDocument).toContain('proposed-model');
});

test('rejected proposals surface the reason', async ({ page }) => {
  page.setExtraHTTPHeaders({ 'X-Harmostes-Dev-User': 'fixture-user' });
  await page.route('**/templates/pr-review/propose', async (route) => {
    await route.fulfill({ status: 400, body: 'The document is identical to the committed template — nothing to propose' });
  });

  await page.goto('/templates/pr-review');
  await islandReady(page, 'code-island', 15000);
  await page.getByTestId('propose-button').click();
  await expect(page.getByTestId('propose-status')).toContainText('rejected:');
});
