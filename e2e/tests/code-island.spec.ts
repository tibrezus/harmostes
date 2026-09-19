// Workflow Code island (issue #416, ADR-0012 §2): the template document as
// YAML in the Monaco + monaco-yaml island, validated against the CRD-derived
// schema from /api/schema. This is the tier that proves the schema is
// actually DRIVING the editor — completion offers schema properties, schema
// violations raise markers, and the shipped fixture world is enough for both.
import { test, expect } from '@playwright/test';
import { islandReady } from './helpers';

test('Workflow Code island renders, completes, and validates schema-driven', async ({ page }) => {
  await page.goto('/templates/pr-review');

  // The bundle is 2.3 MB; state flips to ready once mount() ran.
  const island = await islandReady(page);

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
  // not a one-shot lint. The document must satisfy the REAL CRD now served
  // (#436): spec requires description, prepare, agent and deploy — the
  // three markers above were exactly those missing keys.
  await page.evaluate(() =>
    window.harmostesCodeIsland.setText(
      'apiVersion: harmostes.dev/v1alpha1\nkind: WorkflowTemplate\nspec:\n  description: ok\n  prepare:\n    plugin:\n      name: workspace\n  agent:\n    model: any\n  deploy:\n    plugin:\n      name: post-review\n',
    ),
  );
  await expect
    .poll(() => page.evaluate(() => window.harmostesCodeIsland.markers().length), { timeout: 10000 })
    .toBe(0);
});

// Run detail (#533): the same island mounts in the Temporal band with the
// run's RESOLVED workflow document — kind Workflow (not WorkflowTemplate),
// schema-driven against the workflow CRD key. The document is what this
// run's worker compiled; read-only like the template surface.
test('run detail: the island renders the resolved Workflow document', async ({ page }) => {
  await page.goto('/runs/attempt-pr-review-demo-42a1');

  await expect(page.getByTestId('workflow-code-pane')).toBeVisible();
  const island = await islandReady(page);

  // The resolved document: identity + spec, graph-native fixture shape.
  await expect(island.locator('.view-lines')).toContainText('kind: Workflow');
  await expect(island.locator('.view-lines')).toContainText('pr-review-demo');

  // Read-only on this surface: the band is a read, the MR-bridge owns
  // writes. The handle exposes no isReadOnly — the mount attribute is the
  // contract the glue reads (pinned in the goquery tier too).
  await expect(page.getByTestId('code-island')).toHaveAttribute('data-readonly', 'true');

  // A valid fixture document produces no markers — the workflow schema
  // validates the resolved spec exactly as the template one does.
  expect(await page.evaluate(() => window.harmostesCodeIsland.markers().length)).toBe(0);
});

// Theme regression (#537): the island's colors are mapped from tokens.css —
// toHex once divided oklch lightness by 100 unconditionally, but tokens use
// 0–1 form, so EVERY mapped color collapsed to near-black: dark text on a
// dark editor, in both themes. Text-presence assertions cannot see this;
// computed CONTRAST can. Runs in the default (dark) theme.
test('island theme: rendered text meets contrast against the editor surface', async ({ page }) => {
  await page.goto('/templates/pr-review');
  const island = await islandReady(page);

  const ratio = await page.evaluate(() => {
    const editor = document.querySelector('.monaco-editor') as HTMLElement | null;
    const lines = document.querySelector('.view-lines') as HTMLElement | null;
    if (!editor || !lines) return -1;
    const bg = getComputedStyle(editor).backgroundColor;
    const fg = getComputedStyle(lines).color;
    const lum = (c: string) => {
      const m = /rgba?\((\d+), (\d+), (\d+)/.exec(c);
      if (!m) return -1;
      const [r, g, b] = [Number(m[1]), Number(m[2]), Number(m[3])].map((v) => {
        const s = v / 255;
        return s <= 0.03928 ? s / 12.92 : Math.pow((s + 0.055) / 1.055, 2.4);
      });
      return 0.2126 * r + 0.7152 * g + 0.0722 * b;
    };
    const l1 = lum(bg), l2 = lum(fg);
    if (l1 < 0 || l2 < 0) return -1;
    return (Math.max(l1, l2) + 0.05) / (Math.min(l1, l2) + 0.05);
  });

  expect(ratio).toBeGreaterThanOrEqual(4.5);
  // And the surface is not a collapsed-to-black wash: the editor background
  // must be distinguishable from pure black (the bug rendered ~#000).
  const bg = await island.locator('.monaco-editor').evaluate((el) => getComputedStyle(el).backgroundColor);
  expect(bg).not.toBe('rgb(0, 0, 0)');
});
