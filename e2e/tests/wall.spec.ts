// E2E scenario 1 — the wall answers "what is running, in what workflow,
// where is it" with no clicks (milestone acceptance criterion).
//
// The fixture world (#554): three review subjects (⟡) and one deterministic
// subject — four cards, each linking straight to its latest run. The
// template → workflow → subject organization has its own spec
// (wall-sections.spec.ts).
import { test, expect } from '@playwright/test';

test('the wall renders every fixture subject with review marks and direct links', async ({ page }) => {
  await page.goto('/');

  const cards = page.getByTestId('wall-card');
  // The wall is LIVE (live is not history): the superseded merge-sync
  // subject never renders; four review subjects remain (incl. the parked
  // hold-line fixture #45).
  await expect(cards).toHaveCount(4);

  // Review subjects are marked on their row; the deterministic subject is one row too.
  await expect(page.locator('[data-testid="wall-card"][data-review="true"]')).toHaveCount(4);
  // Every surviving subject is a review — titles render bold, not .ids.
  await expect(page.getByTestId('wall-card-title').locator('strong')).toHaveCount(4);

  // Every card links directly to its latest run — no clicks to find it.
  const subjects = ['demo-rezuscloud/harmostes#42', 'demo-rezuscloud/harmostes#43', 'demo-rezuscloud/harmostes#44', 'demo-rezuscloud/harmostes#45'];
  for (const subject of subjects) {
    await expect(page.locator(`[data-testid="wall-card"][data-subject="${subject}"]`)).toHaveCount(1);
  }
  await expect(page.locator('[data-testid="wall-card"][data-subject="demo-rezuscloud/harmostes#42"] [data-testid="wall-card-title"]'))
    .toHaveAttribute('href', '/runs/attempt-pr-review-demo-42a1');
});

// The parked claim's WHY (#user wall refactor): the queued row renders the
// gate's verbatim hold reason + waiting age under its chip, and its token
// cell is the honest em-dash — a queued run has no usage of its own, and
// the workflow-cache fallback that painted one session's numbers onto every
// row is gone.
test('queued row carries its hold reason, waiting age, and no borrowed tokens', async ({ page }) => {
  await page.goto('/');

  const parked = page.locator('[data-testid="wall-card"][data-subject="demo-rezuscloud/harmostes#45"]');
  await expect(parked).toBeVisible();
  await expect(parked.locator('.chip')).toHaveText('queued · waiting ci');

  const hold = parked.locator('[data-testid="wall-hold"]');
  await expect(hold).toContainText('ci pending at head f00dfeed1234567');
  await expect(hold).toContainText('dispatch on green');
  await expect(hold).toContainText(/waiting (now|\d+[mhd])/);

  // The CHIP itself qualifies (kestra/windmill: the status names what it
  // waits for) — "queued" alone answers nothing.
  await expect(parked.locator('.chip')).toContainText('queued · waiting ci');

  // The workflow block carries the STATE summary: one cell per subject
  // (row order — an index, not a blur) + the counts mirror.
  const summary = page
    .locator('[data-testid="wall-workflow-cell"]')
    .filter({ hasText: 'pr-review-demo' })
    .locator('[data-testid="wall-state-summary"]');
  await expect(summary).toBeVisible();
  await expect(summary.locator('.wall-states-cell')).toHaveCount(3);
  await expect(summary.locator('.wall-states-cell').first()).toHaveAttribute('title', /demo-rezuscloud\/harmostes#\d+ — /);
  await expect(summary.locator('.wall-states-counts')).toContainText('1 in flight');
  await expect(summary.locator('.wall-states-counts')).toContainText('1 queued');
  await expect(summary.locator('.wall-states-counts')).toContainText('1 verdict');

  await expect(parked.locator('[data-testid="wall-tokens-none"]')).toHaveText('—');

  // Rows that DID run carry their own numbers — no identical repeated
  // token counts across rows (the old cache bug).
  const verdict = page.locator('[data-testid="wall-card"][data-subject="demo-rezuscloud/harmostes#42"]');
  await expect(verdict.locator('.num').nth(1)).toContainText('↑45703 ↓26154');
});
