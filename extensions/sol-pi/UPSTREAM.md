# Upstream provenance

Vendored from https://github.com/NVlabs/SoL-Pi at commit `219568034fa54ed121ede80961655e33dfb9d0e3` (main, 2026-10-18 — the Pi-1.x compat line, #711), pruned to
the files the fleet needs (source, tests, schema + compat checkers, tsconfig,
license/notices, docs). Not a subtree/pin tool — bump by replacing this
directory from the upstream commit and recording the new SHA here.

Bump history: `22277b7` (2026-09-11) → `219568034fa54ed121ede80961655e33dfb9d0e3` (2026-10-18). The
22277b7 prune had accidentally dropped upstream's `tsconfig.json` and
`scripts/check-pi-compat.mjs` while KEEPING package.json's
`typecheck`/`check` scripts and the docs that call them — a documented
validation gate that could not run (#711). Both are back (verbatim from
upstream); the type + public-API gates are armed in `make test-sol-pi`.

## Validation on bump (protocol: upstream agents-install.md)

1. `npm ci --ignore-scripts && npm run check` (upstream suite; recorded
   env/prune failures under plain `npm test` in the vendored copy:
   `package.test.ts` pack-output parsing vs npm 12 — 2 tests,
   environmental; `install-guide.test.ts` — validates the pruned
   agent-entry files; `star-history.test.js` — tests the pruned
   star-count script. The fleet tier realizes check via the overlay
   (`sol-pi.fleet.vitest.mjs`) plus the type and public-API gates).
2. `npx vitest run tests/all-mechanisms.test.ts` — 4/4.
3. Load probe under the image's `PI_VERSION` (the Dockerfile gate repeats it).
4. `sol-pi.json` (the shipped profile, this directory) against
   `scripts/check-sol-pi-config.mjs`.

## Pi pairing

Upstream tests against pi 1.x (peer `*`; the Pi-1.x line carries the
Pi-1.x compat fixes — `bash` structured exit codes, codemode). The fleet
pins `PI_VERSION` (1.3.0) — `make test-sol-pi` runs the upstream type +
public-API + suite gates against the fleet's pi packages; treat any
failure there as a compatibility change, not noise.

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
