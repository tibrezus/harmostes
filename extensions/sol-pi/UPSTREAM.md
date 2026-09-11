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

## Ownership constraints (r5 review)

- **No in-tree edits to this directory.** The fleet owns upstream's runtime
  behavior through it (the extension replaces built-in edit/write and
  rewrites prompt context per request) — that ownership is only safe while
  the tree is byte-pristine: a needed patch is an upstream PR first, and
  the fleet waits. `make test-sol-pi`'s "treat any failure as a
  compatibility change" depends on this.
- **Known upstream hazard (recorded, not fixable here):**
  `action-fusion/file-queue.ts` serialises fused mutations on SoL-Pi's own
  queue and intentionally does not nest pi's built-in mutation queue;
  `then-run.ts`'s pre-command hash guard yields via `setImmediate`, so the
  check and the command are not atomic against a non-fused mutation of the
  same file from pi's queue. Shipped profile never lets a model call
  happen, so the exposure is upstream's to close.
