import { test, expect } from '@playwright/test';

// Node inspector + version switcher (#419, ADR-0012 §7). The acceptance
// this tier exists for: an inspector edit updates the Workflow Code island
// (Monaco) WITHOUT text-diff surgery and without a page reload — the server
// applies a structured typed edit, the response document goes back into the
// editor through its handle (setText). Plus: the version switcher moves
// every projection (topology, document, inspector) together.

test.describe('node inspector', () => {
  test('apply patches the island document in place', async ({ page }) => {
    page.setExtraHTTPHeaders({ 'X-Harmostes-Dev-User': 'fixture-user' });
    await page.goto('/templates/pr-review');

    // The island mounted and carries the head document.
    await expect(page.getByTestId('code-island')).toHaveAttribute('data-island-state', 'ready', { timeout: 15000 });
    const before = await page.evaluate(() => window.harmostesCodeIsland.getText());
    expect(before).toContain('mistral-small-latest');

    // Edit the agent model in the inspector (default panel) and apply.
    const model = page.locator('input[name="agent.model"]');
    await expect(model).toHaveValue('mistral-small-latest');
    await model.fill('llama3:8b');
    await page.getByTestId('inspector-apply').click();
    await expect(page.getByTestId('inspector-status')).toContainText('applied 1 change');

    // The island content changed — same page, no navigation, no text diff:
    // the document came back from the structured transform.
    const after = await page.evaluate(() => window.harmostesCodeIsland.getText());
    expect(after).toContain('llama3:8b');
    expect(after).not.toContain('mistral-small-latest');

    // The server-side projections did NOT move: the edit lives in the
    // working document only (persistence is #420's bridge). A fresh load
    // shows the head model again.
    await page.reload();
    await expect(page.getByTestId('code-island')).toHaveAttribute('data-island-state', 'ready', { timeout: 15000 });
    expect(await page.evaluate(() => window.harmostesCodeIsland.getText())).toContain('mistral-small-latest');

    // The cleared-value path: emptying a field writes an explicit empty
    // string — the typed marshal's honest output (non-pointer strings carry
    // no omitempty; "" is the same zero value Go reads back).
    const skill = page.locator('input[name="agent.skill"]');
    await skill.fill('');
    await page.getByTestId('inspector-apply').click();
    await expect(page.getByTestId('inspector-status')).toContainText('applied 1 change');
    const cleared = await page.evaluate(() => window.harmostesCodeIsland.getText());
    expect(cleared).toContain('skill: ""');
  });

  test('apply with no changes is a no-op message', async ({ page }) => {
    page.setExtraHTTPHeaders({ 'X-Harmostes-Dev-User': 'fixture-user' });
    await page.goto('/templates/pr-review?node=deploy');
    await expect(page.getByTestId('code-island')).toHaveAttribute('data-island-state', 'ready', { timeout: 15000 });
    await page.getByTestId('inspector-apply').click();
    await expect(page.getByTestId('inspector-status')).toContainText('no changes');
  });

  test('boolean edit flips the field in the document', async ({ page }) => {
    page.setExtraHTTPHeaders({ 'X-Harmostes-Dev-User': 'fixture-user' });
    await page.goto('/templates/pr-review');
    await expect(page.getByTestId('code-island')).toHaveAttribute('data-island-state', 'ready', { timeout: 15000 });

    const enabled = page.locator('input[name="agent.enabled"]');
    await expect(enabled).toBeChecked(); // head agent is enabled
    await enabled.uncheck();
    await page.getByTestId('inspector-apply').click();
    await expect(page.getByTestId('inspector-status')).toContainText('applied 1 change');
    expect(await page.evaluate(() => window.harmostesCodeIsland.getText())).toContain('enabled: false');
  });
});

test.describe('version switcher', () => {
  test('switching revision moves every projection together', async ({ page }) => {
    page.setExtraHTTPHeaders({ 'X-Harmostes-Dev-User': 'fixture-user' });
    await page.goto('/templates/pr-review');
    await expect(page.getByTestId('code-island')).toHaveAttribute('data-island-state', 'ready', { timeout: 15000 });
    await expect(page.getByTestId('topology-node')).toHaveCount(3);
    expect(await page.evaluate(() => window.harmostesCodeIsland.getText())).toContain('mistral-small-latest');

    // Switch to r1: topology (2 nodes — deterministic-only), document
    // (stale fetch, no agent), and inspector values (model empty, enabled
    // unchecked) all move together. The historical panel is read-only.
    await page.locator('[data-testid="rev-switch-option"][data-rev="1"]').click();
    await expect(page).toHaveURL(/[?&]rev=1/);
    await expect(page.getByTestId('topology-node')).toHaveCount(2);
    await expect(page.getByTestId('code-island')).toHaveAttribute('data-island-state', 'ready', { timeout: 15000 });
    expect(await page.evaluate(() => window.harmostesCodeIsland.getText())).toContain('pr-fetch-stale');
    await expect(page.locator('input[name="agent.enabled"]')).not.toBeChecked();
    await expect(page.locator('input[name="agent.enabled"]')).toBeDisabled();
    await expect(page.getByTestId('inspector-apply')).toHaveCount(0);
    await expect(page.getByTestId('rev-historical')).toBeVisible();

    // Back to head via the switcher: everything restores.
    await page.locator('[data-testid="rev-switch-option"][data-rev="2"]').click();
    await expect(page.getByTestId('topology-node')).toHaveCount(3);
    await expect(page.getByTestId('code-island')).toHaveAttribute('data-island-state', 'ready', { timeout: 15000 });
    expect(await page.evaluate(() => window.harmostesCodeIsland.getText())).toContain('mistral-small-latest');
    await expect(page.getByTestId('inspector-apply')).toHaveCount(1);
    await expect(page.getByTestId('rev-historical')).toHaveCount(0);
  });
});
