# Architecture

The detailed architecture lives in the [GitHub wiki](https://github.com/tibrezus/harmostes/wiki).

- [ADR-0001 — Graph-native orchestration kernel](https://github.com/tibrezus/harmostes/wiki/ADR-0001-graph-native-orchestration-kernel)
- [ADR-0005 — Canonical Orchestration History](https://github.com/tibrezus/harmostes/wiki/ADR-0005-canonical-orchestration-history)
- [Legacy phase-loop architecture (superseded)](https://github.com/tibrezus/harmostes/wiki/Legacy-Architecture--Phase-Loop)

The canonical glossary is [`CONTEXT.md`](./CONTEXT.md). This file is kept only
as a conventional pointer for readers who look for an `ARCHITECTURE.md`.

## Agent navigation subsystem (ADR-0009)

`extensions/rig-query` — a pi extension exposing the project graph (`rig.db`,
generated SHA-exact at review time by the ops repo's `workspace.sh` prepare
(using the vendored `plugins/rig-emit` emitter); the in-repo `rig-emit.sh`
plugin writes the *wiki's* synced copy — a different artifact reviews must
not consume) as one structured tool
(`overview | component | search | files | deps`) to every workflow agent.
Owns the "no grep expeditions" contract; the agent's pi invocation shape
(`-e` extensions, `--tools` allowlist) lives in `internal/piargs` — a LEAF
package, so the stdlib-only `cmd/harmostes-agent` need not sit on the
worker's Dapr/k8s/otel closure to assemble four flags (r26 ARCH-1;
`TestPiargsIsLeaf`) — and is consumed by both the worker and
`harmostes.py`, pinned by `TestExtensionsSingleSource`. Truncation markers and query
telemetry (`details.{command,target,chars,truncated,rig_sha}`) are the
measurement surface for review-session efficiency (#336, #337). The emitting
half of the review-time graph lives in the ops repo (`workspace.sh`).

Freshness contract (both halves required for the rule to hold): the only
sanctioned graph location is `/workspace/rig.db`, produced by the ops
prepare (`operations/k8s-config/platform/harmostes/scripts/workspace.sh` —
the contractual pair of `piargs.RigGraphPath`; a rename there is a
fleet-wide `graph: absent` here) together with its provenance stamp
`/workspace/rig.db.sha` (the reviewed HEAD SHA, written as an atomic pair
with the graph — the wrapper checks the graph's inode identity while the
stamp is read beside it). Strictness as implemented (r28 restatement):

- expectation armed + **stamped** graph → the worker injects
  `RIG_EXPECTED_SHA` + `RIG_REQUIRE_SHA=1`; `mismatch`, `malformed` and
  `unchecked` (incl. short stamps) are REFUSED;
- expectation armed + **unstamped** graph → served with an
  unverified-graph caveat (`sha_state=absent-refusal`, still countable
  telemetry) — the stamp's producer is the ops repo, and a fleet-wide
  refusal over its emission failure would make the tool strictly
  negative-value;
- expectation armed + **unreachable** graph (nothing resolves, e.g. a
  suppressed walk or mis-mounted path) → REFUSED (`sha_state=unreachable`);
- run-level degradation (`graph: absent` / `graph: unstamped`) is logged at
  worker startup so degraded runs stay countable from pod logs alone.
