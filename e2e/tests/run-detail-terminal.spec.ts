// E2E scenario 3 — the terminal run's centerpiece: the compiled graph, the
// timing waterfall (agent bar dominant), and the hover panel with live data.
//
// The hover panel uses event delegation (mouseover on [data-node]) — a real
// browser hover is exactly the interaction a component test cannot fake.
import { test, expect } from '@playwright/test';

test('terminal run detail: graph, waterfall proportions, hover panel', async ({ page }) => {
  await page.goto('/runs/attempt-pr-review-demo-42a1');

  // Graph: the full 4-node pipeline.
  const nodes = page.getByTestId('graph-node');
  // 5 = virtual trigger + the four graph nodes (#541).
  await expect(nodes).toHaveCount(5);

  // Waterfall: overhead + 4 node lanes; the 13m agent bar must dominate.
  const lanes = page.getByTestId('timing-lane');
  await expect(lanes).toHaveCount(5);

  const widthOf = (label: string) =>
    page.locator(`[data-testid="timing-lane"][data-label="${label}"] rect`).first()
      .evaluate((el) => Number(el.getAttribute('width')));
  const agent = await widthOf('agent');
  const prepare = await widthOf('prepare');
  const gate = await widthOf('gate');
  expect(agent).toBeGreaterThan(prepare);
  expect(agent).toBeGreaterThan(gate);

  // Hover the agent node: the delegated panel shows identity rows first
  // (#541 — what the node IS), then the settled state and the humanized
  // 13m duration, with the transcript one click away.
  await page.locator('[data-node="agent"]').hover();
  const panel = page.locator('#rg-panel');
  await expect(panel).toContainText('litellm/zai/glm-5.3-flash');
  await expect(panel).toContainText('skill pr-review');
  await expect(panel).toContainText('maxFixes 3');
  await expect(panel).toContainText('ok');
  await expect(panel).toContainText('13.0m');
  await expect(panel.locator('a[href*="/runs/pr-review-demo-42a1-agent/session"]')).toHaveCount(1);

  // The trigger's panel: the cause, spelled out (kind + push target).
  await page.locator('[data-node="trigger"]').hover();
  await expect(panel).toContainText('webhook trigger');
  await expect(panel).toContainText('demo-rezuscloud-harmostes');
});
