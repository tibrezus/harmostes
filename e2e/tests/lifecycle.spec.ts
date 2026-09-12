import { test, expect } from '@playwright/test';

// Run-with-inputs round-trip (#418, ADR-0012 §7): create → visible → arm →
// trigger → delete, driven as a browser through the real HTTP surface.
// The fixture server runs with dev writes enabled; the identity header
// (X-Harmostes-Dev-User) is the dev-identity path — writes stamp its owner
// label, and the owner label is what the visibility checks below actually
// verify (the harmostes verification loop: owner-label check + UI
// visibility check, per identity).
//
// The scenario is serial over one shared fixture world and cleans up after
// itself through the UI's own delete verb — a failed step leaves the
// created object behind, so afterAll re-issues the delete (404 tolerated).

const NAME = 'e2e-created-418';
const WRITER = { 'X-Harmostes-Dev-User': 'writer' };
const OTHER = { 'X-Harmostes-Dev-User': 'someoneelse' };
const READER = { 'X-Forwarded-User': 'browsing-mallory' };

test.describe.serial('run-with-inputs round-trip', () => {
  test.afterAll(async ({ request }) => {
    // Safety net: remove the created instance through the same verb the
    // UI ships. 404 (already gone / never created) is fine.
    await request.post(`/workflows/${NAME}/delete`, { headers: WRITER, maxRedirects: 0 });
  });

  test('read-only identities browse but see no CTA', async ({ browser }) => {
    const ctx = await browser.newContext({ extraHTTPHeaders: READER });
    const page = await ctx.newPage();
    await page.goto('/workflows');
    await expect(page.getByTestId('wf-new-link')).toHaveCount(0);
    // The catalog itself still renders.
    await expect(page.locator('.page-tab--active')).toContainText('Workflows');
    // Deep-linking to the form redirects to the catalog.
    await page.goto('/workflows/new');
    await expect(page).toHaveURL(/\/workflows$/);
    await ctx.close();
  });

  test('create from the template form → detail, paused', async ({ page }) => {
    page.setExtraHTTPHeaders(WRITER);
    await page.goto('/workflows/new?template=pr-review');
    await expect(page.getByTestId('wf-new-form')).toBeVisible();

    // Scope fields render from the template's declaration.
    await page.getByTestId('wf-new-name').fill(NAME);
    await page.fill('input[name="scope-pr-review-repos"]', 'demo-rezuscloud/harmostes');
    await page.fill('input[name="scope-pr-review-label"]', 'needs-review');
    // Cadence: pick webhook (the schedule radio is the checked default).
    await page.check('input[name="sourceKind"][value="webhook"]');
    await page.getByTestId('wf-new-submit').click();

    await expect(page).toHaveURL(new RegExp(`/workflows/${NAME}$`));
    // Created paused, by design: Arm offered, Trigger not.
    await expect(page.getByTestId('wf-enable')).toBeVisible();
    await expect(page.getByTestId('wf-trigger')).toHaveCount(0);
    await expect(page.locator('.status-badge').first()).toContainText('Paused');
  });

  test('created instance is visible to its owner and nobody else', async ({ browser }) => {
    // Owner sees it in the catalog — the owner label drives the filter.
    const own = await browser.newContext({ extraHTTPHeaders: WRITER });
    const ownPage = await own.newPage();
    await ownPage.goto('/workflows');
    await expect(ownPage.locator(`a[href="/workflows/${NAME}"]`).first()).toBeVisible();
    await own.close();

    // Another identity does not — the same object, the same route.
    const other = await browser.newContext({ extraHTTPHeaders: OTHER });
    const otherPage = await other.newPage();
    await otherPage.goto('/workflows');
    await expect(otherPage.locator(`a[href="/workflows/${NAME}"]`)).toHaveCount(0);
    // And its detail page is a 404 for them (existence not leaked).
    const resp = await otherPage.goto(`/workflows/${NAME}`);
    expect(resp?.status()).toBe(404);
    await other.close();
  });

  test('arm → trigger round-trip', async ({ page }) => {
    page.setExtraHTTPHeaders(WRITER);
    await page.goto(`/workflows/${NAME}`);

    // Arm: the state verbs swap (Arm disappears, Trigger + Pause appear).
    await page.getByTestId('wf-enable').click();
    await expect(page).toHaveURL(new RegExp(`/workflows/${NAME}$`));
    await expect(page.getByTestId('wf-trigger')).toBeVisible();
    await expect(page.getByTestId('wf-disable')).toBeVisible();
    await expect(page.getByTestId('wf-enable')).toHaveCount(0);

    // Trigger: the manual wake POSTs and bounces back to the detail page
    // (the annotation lands on the CR — the component tier pins that; here
    // we pin that the verb round-trips a browser cleanly).
    await page.getByTestId('wf-trigger').click();
    await expect(page).toHaveURL(new RegExp(`/workflows/${NAME}$`));
    await expect(page.getByTestId('wf-trigger')).toBeVisible();
  });

  test('delete removes the instance', async ({ page }) => {
    page.setExtraHTTPHeaders(WRITER);
    await page.goto(`/workflows/${NAME}`);

    page.once('dialog', (dialog) => dialog.accept());
    await page.getByTestId('wf-delete').click();
    await expect(page).toHaveURL(/\/workflows$/);
    await expect(page.locator(`a[href="/workflows/${NAME}"]`)).toHaveCount(0);
  });
});
