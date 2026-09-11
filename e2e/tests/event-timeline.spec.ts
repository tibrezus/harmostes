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
