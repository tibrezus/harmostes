// Transient-retry surfacing (ADR-0012 §9, #556): the fixture's terminal
// demo attempt has a gate envelope with attempt=2 — the run graph's panel
// must name the Attempts row and the waterfall must title the retry.
import { test, expect } from '@playwright/test';

test('retried node surfaces its attempt count in the panel and waterfall', async ({ page }) => {
  await page.goto('/runs/attempt-pr-review-demo-42a1');

  const graph = page.getByTestId('run-graph-section');
  await expect(graph).toBeVisible();

  // Waterfall title: the retried gate segment names the retry (SVG <title>).
  const gateTitle = graph.locator('[data-testid="timing-lane"][data-label="gate"] .rg-timing-dur');
  await expect(gateTitle).toHaveText(/retry ×2/);

  // Panel: click the gate node card → the Attempts row appears between
  // Duration and Run.
  await graph.locator('[data-node="gate"]').first().click();
  const panel = page.locator('#rg-panel');
  await expect(panel).toBeVisible();
  await expect(panel).toContainText('Attempts');
  await expect(panel).toContainText('2 (transient retries)');
});
