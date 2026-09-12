import { test, expect } from '@playwright/test';

// Topology projection + revision graph diff (#417, ADR-0012 §3).
// The component tier (topology_component_test.go) pins the goquery DOM
// contract; this tier drives the same pages as a browser: template
// topology, thin-instance merged shape, and the revision diff view
// (node/edge delta classes + the YAML diff pane).

test.describe('topology projection', () => {
  test('template detail renders the auto-layouted topology', async ({ page }) => {
    await page.goto('/templates/pr-review');

    const nodes = page.getByTestId('topology-node');
    // The fixture template compiles agent-enabled: prepare → agent → deploy.
    await expect(nodes).toHaveCount(3);

    const agent = page.locator('[data-testid="topology-node"][data-node="agent"]');
    await expect(agent).toBeVisible();
    // Palette is schema-derived: agent is in the CRD enum → not flagged.
    await expect(agent).not.toHaveAttribute('data-unknown-type', 'true');

    // Edges carry the from→to identity.
    await expect(page.getByTestId('topology-edge')).toHaveCount(2);

    // History exists in the fixture: the revisions link must appear.
    await expect(page.getByTestId('revisions-link')).toBeVisible();
    await expect(page.getByTestId('revisions-link')).toContainText('2 revisions');
  });

  test('thin instance renders its merged shape', async ({ page }) => {
    await page.goto('/workflows/pr-review-instance');

    // The stored CR is thin (templateRef + source only) — the topology
    // shows the merged shape: the template's agent appears.
    const nodes = page.getByTestId('topology-node');
    await expect(nodes).toHaveCount(3);
    await expect(page.locator('[data-testid="topology-node"][data-node="agent"]')).toBeVisible();
    await expect(page.locator('[data-testid="topology-node"][data-node="deploy"]')).toContainText('post-review');
  });

  test('revision graph diff marks node and edge deltas', async ({ page }) => {
    await page.goto('/templates/pr-review/revisions');

    const panes = page.getByTestId('topology-pane');
    await expect(panes).toHaveCount(2);

    // r1 (deterministic-only, stale fetch) → head (agent enabled):
    // older pane ghosts the agent in as added; newer pane owns it as added.
    const older = page.locator('[data-testid="topology-pane"][data-pane="older"]');
    const newer = page.locator('[data-testid="topology-pane"][data-pane="newer"]');
    await expect(older.locator('[data-testid="topology-node"][data-diff="added"]')).toHaveCount(1);
    await expect(newer.locator('[data-testid="topology-node"][data-diff="added"]')).toHaveCount(1);
    await expect(older.locator('[data-testid="topology-node"][data-diff="added"]')).toHaveAttribute('data-node', 'agent');

    // prepare changed its plugin (pr-fetch-stale → pr-fetch): changed in both panes.
    await expect(older.locator('[data-testid="topology-node"][data-diff="changed"]')).toHaveCount(1);
    await expect(newer.locator('[data-testid="topology-node"][data-diff="changed"]')).toHaveCount(1);

    // Edge deltas: prepare→agent and agent→deploy added (both panes — ghost
    // and own), prepare→deploy removed.
    await expect(older.locator('[data-testid="topology-edge"][data-diff="added"]')).toHaveCount(2);
    await expect(newer.locator('[data-testid="topology-edge"][data-diff="added"]')).toHaveCount(2);
    await expect(older.locator('[data-testid="topology-edge"][data-diff="removed"]')).toHaveCount(1);
    await expect(newer.locator('[data-testid="topology-edge"][data-diff="removed"]')).toHaveCount(1);

    // The YAML diff pane carries added and removed lines with real deltas.
    await expect(page.locator('[data-testid="yaml-diff-line"][data-diff="added"]').filter({ hasText: 'mistral-small-latest' })).toHaveCount(1);
    await expect(page.locator('[data-testid="yaml-diff-line"][data-diff="removed"]').filter({ hasText: 'pr-fetch-stale' })).toHaveCount(1);

    // The legend explains the vocabulary.
    await expect(page.getByTestId('topology-legend')).toContainText('added');
    await expect(page.getByTestId('topology-legend')).toContainText('removed');

    // Revision picker: both revisions offered; the head is r2.
    await expect(page.getByTestId('rev-option')).toHaveCount(2);
  });
});
