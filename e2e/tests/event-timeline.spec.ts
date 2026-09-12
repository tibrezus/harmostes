// E2E — the Event Timeline (ADR-0012 §4): the tab lazy-loads the fragment,
// the full narrative renders as append-only rows, payload expanders open,
// and the state chips speak the console vocabulary.
//
// Reload-equals-live is the design contract; the SSE path is exercised by
// the same fragment swap the tab click performs.
import { test, expect } from '@playwright/test';

test('event timeline: tab, narrative rows, payload expander', async ({ page }) => {
  await page.goto('/runs/attempt-pr-review-demo-42a1');

  // The graph is the default view; the timeline pane stays hidden.
  await expect(page.getByTestId('graph-tab')).toBeVisible();
  const pane = page.getByTestId('event-timeline-pane');
  await expect(pane).toBeHidden();

  // Activate the Events tab: the pane loads the fragment.
  await page.getByTestId('event-timeline-tab').click();
  await expect(pane).toBeVisible();

  const rows = page.getByTestId('timeline-row');
  await expect(rows).toHaveCount(21); // fixture narrative: 18 store + 3 ledger rows

  // The story reads in order: trigger first, terminal last.
  await expect(rows.first()).toContainText('objective anchored');
  await expect(rows.last()).toContainText('run.completed');

  // Every row has a state chip and a kind label.
  await expect(page.getByTestId('timeline-row-kind').first()).toBeVisible();
  await expect(page.getByTestId('timeline-row-time').first()).toBeVisible();

  // A payload expander opens (details → pre).
  const details = page.getByTestId('timeline-row-details').first();
  await details.click();
  await expect(page.getByTestId('timeline-row-payload').first()).toBeVisible();

  // Gate feedback content is visible somewhere in the narrative.
  await expect(pane).toContainText('verdict posted');
  await expect(pane).toContainText('claim armed');
});

// SSE convergence (#421): with the Events tab open on the in-flight
// attempt, a lifecycle event arriving through the production ingress
// (/dapr/events — the same cloud-event route daprd posts) converges into
// the pane WITHOUT a reload: the fixture store grows from the event, the
// hub wakes the attempt's stream, the fragment re-renders. This is the
// browser being told, never asking — ADR-0006's spine, observed on the
// Event Timeline.
test('a lifecycle event converges into the open timeline through SSE', async ({ page, request }) => {
  await page.goto('/runs/attempt-pr-review-demo-43c2');
  await page.getByTestId('event-timeline-tab').click();

  const rows = page.getByTestId('timeline-row');
  // The tab click fires the htmx fetch asynchronously — wait for the
  // initial fragment before reading the baseline count (CI is cold; a
  // local warm server hides this race).
  await expect(rows.first()).toBeVisible();
  const before = await rows.count();

  // In-place sentinel: a full reload would drop it; an SSE fragment swap
  // keeps it. The convergence assertion is worth nothing without this.
  await page.evaluate(() =>
    document.querySelector('[data-testid="event-timeline-pane"]')?.setAttribute('data-e2e-live', '1'),
  );

  await request.post('/dapr/events', {
    data: {
      id: 'e2e-timeline-1',
      specversion: '1.0',
      type: 'harmostes.lifecycle',
      source: 'e2e',
      data: {
        event: 'node.completed',
        pipeline: 'pr-review-demo',
        attempt: 'attempt-pr-review-demo-43c2',
        node: 'agent',
        nodeType: 'agent',
        status: 'green',
        feedback: 'e2e convergence row',
      },
    },
  });

  // The row arrives through the stream: count grows, the new row is last
  // (the fixture store appends at the story's tail), and the sentinel
  // survived — the pane updated in place.
  await expect(rows).toHaveCount(before + 1, { timeout: 10_000 });
  await expect(rows.last()).toContainText('e2e convergence row');
  await expect(page.locator('[data-testid="event-timeline-pane"][data-e2e-live="1"]')).toHaveCount(1);
});
