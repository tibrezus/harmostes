// Playwright E2E tier for the harmostes UI (milestone ⑤ of #290).
//
// The suite boots `harmostes-ui -fixture` — the deterministic in-memory
// world — and drives the real HTTP surface in a real browser. The contract
// under test is the same one the goquery component tests pin, but through
// actual browser behavior: SSE re-renders, hover delegation, navigation.
//
// Rules (from the #290 design round):
//   - thin: a handful of scenarios, no page-object ceremony
//   - getByRole / data-testid first — never styling classes
//   - wait on DOM state, never sleep
//   - the fixture server is started here (webServer) so CI and local runs
//     exercise identical boot paths
import { defineConfig, devices } from '@playwright/test';

const PORT = 8813;
const BASE = `http://127.0.0.1:${PORT}`;

export default defineConfig({
  testDir: './tests',
  fullyParallel: false, // one shared fixture world; scenarios run in order
  workers: 1,
  retries: 0, // deterministic world — a flake is a bug, not weather
  reporter: [['list']],
  use: {
    baseURL: BASE,
    trace: 'retain-on-failure',
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
  webServer: {
    command: `go run ./cmd/harmostes-ui -fixture -addr 127.0.0.1:${PORT}`,
    url: `${BASE}/healthz`,
    reuseExistingServer: !process.env.CI,
    cwd: new URL('..', import.meta.url).pathname,
    timeout: 60_000,
  },
});
