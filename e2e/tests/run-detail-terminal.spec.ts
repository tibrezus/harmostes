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

  // #547 structure: the masthead carries claim + objective as a fact grid,
  // the ledger below renders as tables (not stacked dl/card rows).
  const facts = page.getByTestId('fact-grid');
  await expect(facts).toContainText('demo-rezuscloud/harmostes#42');
  await expect(facts).toContainText('verdict posted');

  const runs = page.getByTestId('runs-table');
  await expect(runs.locator('tbody tr')).toHaveCount(3);
  await expect(runs).toContainText('13.1m');
  await expect(runs.locator('a[href*="/session"]')).toHaveCount(3);

  const results = page.getByTestId('noderes-table');
  await expect(results.locator('tbody tr')).toHaveCount(4);
  await expect(results).toContainText('13.0m');

  // #551 log drawer: Logs opens FULL WIDTH below the runs table (never
  // inside the action cell), names the run, and advertises the ordering.
  // The fixture seeds no pods — the honest pod-gone note is the body.
  await runs.locator('button[hx-target="#runlogs-drawer"]').first().click();
  const drawer = page.getByTestId('runlogs-drawer');
  await expect(drawer).toBeVisible();
  await expect(drawer).toContainText('pr-review-demo-42a1-prepare');
  await expect(drawer).toContainText('newest first');
  await expect(drawer).toContainText('Pod recycled');
  const below = await drawer.evaluate((el) => {
    const t = document.querySelector('[data-testid="runs-table"]')!.getBoundingClientRect();
    const d = el.getBoundingClientRect();
    return d.top >= t.bottom - 2 && d.width >= t.width - 10;
  });
  expect(below).toBe(true);

  // The ✕ closes it; the next Logs click reopens (swap replaces wholesale).
  await drawer.locator('.runlogs-close').click();
  await expect(drawer).not.toBeVisible(); // :empty { display: none }
});
