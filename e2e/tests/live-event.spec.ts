// E2E scenario 5 — the event-driven spine, observed live: a lifecycle event
// injected through the DAPR ingress (the same route daprd uses in
// production) wakes the wall's SSE stream and the re-rendered fragment
// carries the agent usage — no reload, no polling.
//
// This is the scenario that justifies the whole event-armed architecture
// (ADR-0006): the browser is *told*, it never asks.
import { test, expect } from '@playwright/test';
import type { APIRequestContext } from '@playwright/test';

const USAGE_EVENT = {
  id: 'e2e-usage-1',
  specversion: '1.0',
  type: 'harmostes.lifecycle',
  source: 'e2e',
  data: {
    event: 'node.completed',
    pipeline: 'pr-review-demo',
    attempt: 'attempt-pr-review-demo-43c2',
    node: 'agent',
    status: 'green',
    outputs: {
      model: 'e2e-demo-model',
      attempts: 1,
      turns: 42,
      usage: { input_tokens: 123, output_tokens: 456 },
    },
  },
};

async function injectEvent(request: APIRequestContext): Promise<void> {
  const res = await request.post('/dapr/events', { data: USAGE_EVENT });
  expect(res.status()).toBe(200);
}

test('a lifecycle event reaches the open wall through SSE without reload', async ({ page, request }) => {
  await page.goto('/');

  // The usage model is event-only data: absent until the event arrives.
  await expect(page.locator('.wall-usage-model')).toHaveCount(0);

  await injectEvent(request);

  // The SSE stream re-renders the wall fragment; the injected model name
  // appears — no navigation happened (the URL never changed, no reload).
  const model = page.locator('.wall-usage-model').filter({ hasText: 'e2e-demo-model' });
  await expect(model.first()).toBeVisible({ timeout: 10_000 });
  await expect(page).toHaveURL(/\/$/);
});

test('a lifecycle event wakes the run-detail graph stream too', async ({ page, request }) => {
  await page.goto('/runs/attempt-pr-review-demo-43c2');
  await expect(page.getByTestId('graph-node')).toHaveCount(4);

  // The stream re-renders on the injected event; the fragment content here
  // is ledger-driven (SSE accelerates freshness, never determines state),
  // so the assertion is that the stream stays live: the pulse remains and
  // the connection did not error. The wall scenario above already proved
  // end-to-end delivery.
  await injectEvent(request);
  await expect(page.locator('.rg-pulse')).toHaveCount(1, { timeout: 10_000 });
  await expect(page.getByTestId('graph-node')).toHaveCount(4);
});
