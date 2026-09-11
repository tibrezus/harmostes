// Workflow Code island (issue #416, ADR-0012 §2): the template document as
// YAML in the Monaco + monaco-yaml island, validated against the CRD-derived
// schema from /api/schema. This is the tier that proves the schema is
// actually DRIVING the editor — completion offers schema properties, schema
// violations raise markers, and the shipped fixture world is enough for both.
import { test, expect } from '@playwright/test';

test('Workflow Code island renders, completes, and validates schema-driven', async ({ page }) => {
  await page.goto('/templates/pr-review');

  const island = page.getByTestId('code-island');
  // The bundle is 2.3 MB; state flips to ready once mount() ran.
  await expect(island).toHaveAttribute('data-island-state', 'ready', { timeout: 20000 });

  // The document itself is visible in the editor surface.
  await expect(island.locator('.view-lines')).toContainText('kind: WorkflowTemplate');

  // A valid document produces no validation markers.
  expect(await page.evaluate(() => window.harmostesCodeIsland.markers().length)).toBe(0);

  // Completion: at `spec:` (line 5, after "spec:") the language service
  // offers the schema's properties — description and scope come from the
  // fixture CRD schema, nothing else knows them.
  await page.evaluate(() => {
    window.harmostesCodeIsland.setText('apiVersion: harmostes.dev/v1alpha1\nkind: WorkflowTemplate\nspec:\n');
    window.harmostesCodeIsland.triggerSuggest();
  });
  const suggest = island.locator('.suggest-widget');
  await expect(suggest).toBeVisible({ timeout: 10000 });
  await expect(suggest).toContainText('description', { timeout: 10000 });
  await expect(suggest).toContainText('scope');
  await page.keyboard.press('Escape');

  // Validation: a spec that violates the schema (object vs integer) raises
  // a marker — the island's verdict surface, same API the MR-bridge reads.
  await page.evaluate(() => window.harmostesCodeIsland.setText('spec: 42\n'));
  await expect
    .poll(() => page.evaluate(() => window.harmostesCodeIsland.markers().length), { timeout: 10000 })
    .toBeGreaterThan(0);

  // Restoring a valid document clears the markers — the verdict is live,
  // not a one-shot lint.
  await page.evaluate(() =>
    window.harmostesCodeIsland.setText('apiVersion: harmostes.dev/v1alpha1\nkind: WorkflowTemplate\nspec:\n  description: ok\n'),
  );
  await expect
    .poll(() => page.evaluate(() => window.harmostesCodeIsland.markers().length), { timeout: 10000 })
    .toBe(0);
});
