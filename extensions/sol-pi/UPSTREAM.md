# Upstream provenance

Vendored from https://github.com/NVlabs/SoL-Pi at commit `22277b7e0c3c46ba1259a6687f31fe39ade421a5` (main, 2026-09-11), pruned to
the files the fleet needs (source, tests, schema checker, license/notices,
docs). Not a subtree/pin tool — bump by replacing this directory from the
upstream commit and recording the new SHA here.

## Validation on bump (protocol: upstream agents-install.md)

1. `npm ci --ignore-scripts && npm run check` (upstream suite; known env
   failure: `tests/package.test.ts` pack-output parsing vs npm 12 — 2 tests,
   environmental).
2. `npx vitest run tests/all-mechanisms.test.ts` — 4/4.
3. Load probe under the image's `PI_VERSION` (the Dockerfile gate repeats it).
4. `sol-pi.json` (the shipped profile, this directory) against
   `scripts/check-sol-pi-config.mjs`.

## Pi pairing

Upstream tests against pi 0.84.2 (peer `*`). The fleet pins `PI_VERSION`
(0.84.4) — `make test-sol-pi` runs the upstream suite against the fleet's
pi packages; treat any failure there as a compatibility change, not noise.
