// Shared E2E helpers (#421): the pieces every island spec needs, stated
// once so hook renames and wait policy change in exactly one place.
//
//   - islandReady: the islands flip data-island-state to "ready" once their
//     bundle mounted — the one sanctioned way to wait for island behavior.
//   - identity headers: the dev-identity paths the fixture accepts
//     (X-Harmostes-Dev-User for write identities, X-Forwarded-User for
//     read-only ones).
import { expect, type Page, type Locator } from '@playwright/test';

export const WRITER = { 'X-Harmostes-Dev-User': 'writer' } as const;
export const OTHER = { 'X-Harmostes-Dev-User': 'someoneelse' } as const;
export const READER = { 'X-Forwarded-User': 'browsing-mallory' } as const;

// Wait until the island with the given testid has mounted. Returns the
// island locator. The bundle ships ~2.3 MB of Monaco; the default timeout
// covers a cold fixture server.
export async function islandReady(page: Page, testId = 'code-island', timeout = 20000): Promise<Locator> {
  const island = page.getByTestId(testId);
  await expect(island).toHaveAttribute('data-island-state', 'ready', { timeout });
  return island;
}
