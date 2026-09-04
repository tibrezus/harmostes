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
Owns the "no grep expeditions" contract; truncation markers and query
telemetry (`details.{command,target,chars,truncated,rig_sha}`) are the
measurement surface for review-session efficiency (#336, #337). The emitting
half of the review-time graph lives in the ops repo (`workspace.sh`).
