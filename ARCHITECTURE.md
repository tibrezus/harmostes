# Harmostes — a Kubernetes framework for LLM-driven automation

> ἁρμοστής — *one who fits things together.*

Harmostes is a Kubernetes-native framework for automation pipelines that are
**mostly deterministic** and **validated by an LLM in a loop**. You declare a
pipeline as a `Workflow` custom resource. The framework **monitors** a source,
runs **deterministic pluggable components** to do everything that can be done
without intelligence, and — only where interpretation is needed — **drives an
LLM agent** that must pass a **deterministic gate** before its work is
published. **Dapr is the event + state fabric** connecting every stage.

The LLM is never the first resort and never the last word: deterministic
components do everything they can before and after it, and a deterministic gate
must pass before anything ships.

---

## The loop

Every workflow is one instance of this cycle:

```
        ┌────────────────────────────────────────────────────────────────┐
        │                                                                ▼
  monitor ──▶ prepare ──▶ (changed?) ──▶ agent ──▶ gate ──▶ deploy ──▶ state
  (source)   deterministic  no: skip     LLM +     fail:     deterministic
             plugin                      feedback  feed back  plugin
                                         loop      to agent
```

1. **monitor** — watch a source (a git revision via Flux, a schedule, an
   upstream fork, an inbound event). Decide there is work.
2. **prepare** — a *deterministic plugin* transforms inputs into an artifact
   (a RIG, a cherry-picked sync branch). Cheap, repeatable, no LLM.
3. **detect** — compare against stored state. Nothing changed → stop (the
   common, boring, cheap case).
4. **agent** — the LLM step. Framework-native (the harmostes RPC primitive):
   the agent does the interpretive work and commits.
5. **gate** — a *deterministic plugin* validates (lint, build, signatures). On
   failure, the gate's stderr is fed back to the **same warm agent session**;
   the agent fixes and the gate re-runs — up to `maxFixes` times.
6. **deploy** — a *deterministic plugin* publishes the green result (push to a
   wiki, replace a release branch + tag).
7. **state** — record what was processed (Dapr) so the next monitor skips it.

---

## Abstractions

| Abstraction | What it is | Provided by |
|---|---|---|
| **Workflow** | A declarative pipeline instance (CR) | the user, via GitOps |
| **Source** | What to monitor — `git` / `schedule` / `event` / `webhook` | CR `spec.source` |
| **Phase** | One of `prepare`, `agent`, `deploy` (fixed set) | the framework |
| **Plugin** | A pluggable deterministic component (image or ConfigMap script) | workflow author |
| **Gate** | A plugin whose contract is *exit 0 = green* | workflow author |
| **AgentStep** | model + skill + tools + task-template + gate + maxFixes | CR `spec.agent` (framework runs it) |
| **Event** | A Dapr pub/sub message between phases | the framework |
| **State** | Per-workflow processed-revision / commit / gate-status | the framework (Dapr state) |

The **agent step is not a plugin** — it is the framework's core (today's
`harmostes.py task` → `pi --mode rpc` → task → gate → feedback-as-session-
continuation). The CR declares *what*; the framework runs *how*.

---

## Components

A small set of independent components, connected by the Dapr fabric. Each
scales independently; the expensive LLM parts scale to zero.

```
   ┌──────────── Dapr fabric (Valkey) ────────────┐
   │   pub/sub (events)        state (skip/dedup) │
   └──────────────────────────────────────────────┘

   ┌──────────┐  work.prepared   ┌──────────┐  work.needs-agent  ┌──────────┐
   │ monitor  │ ───────────────▶ │ prepare  │ ─────────────────▶ │ agent    │
   │ always-on│   (source/chg'd) │ worker   │   (+ work item)    │ worker   │
   │ controller│                  │ KEDA →0  │                    │ KEDA →0  │
   │ (kopf)   │                   │ prepare  │                    │ harmostes│
   │ watches  │                   │ plugin   │                    │ RPC      │
   │ CRs      │                   └──────────┘                    └──────────┘
   └──────────┘                                                         │
                                                            work.resolved (gate green)
                                                                        ▼
                                                                 ┌──────────┐
                                                                 │ deploy   │
                                                                 │ worker   │
                                                                 │ KEDA →0  │
                                                                 │ deploy   │
                                                                 │ plugin   │
                                                                 └──────────┘
                                                                        │ work.deployed
                                                                        ▼
                                                                 monitor (state)
```

- **Monitor (controller)** — a lightweight always-on process (kopf) that watches
  `Workflow` CRs + their sources, decides there's work, and emits the first
  event. It also reconciles CR `status`. This is the only always-on component
  besides Dapr.
- **Prepare / Agent / Deploy workers** — one generic worker image (the harmostes
  worker). Each subscribes to a topic, runs the phase's plugin (or the harmostes
  RPC for the agent phase), and emits the next event. Each is a **KEDA ScaledJob**
  (scale-to-zero), triggered by a redis-streams/pub-sub scaler on the Dapr
  topics.
- **Dapr fabric** — state store (skip/dedup) + pub/sub (choreography). The
  **deterministic event-system application layer** surrounding the framework.
  Backed by Valkey today; abstracted by Dapr (swap to NATS/PostgreSQL by
  changing Component CRs).

Why the split: the deterministic reconcile is batchy and cheap (scale-to-zero
suits it); the event fabric is fire-and-forget (consumers must exist — but the
*subscriber* is generic now, not per-workflow); the LLM worker is heavy and
intermittent (isolate + scale-to-zero). One always-on controller reconciles all
workflows; everything else is on-demand.

---

## The plugin interface

A plugin is a command (a script in an image, or a ConfigMap-mounted script)
invoked by a worker with a fixed contract:

```bash
env:
  HARMOSTES_WORKFLOW   = <CR name>
  HARMOSTES_NAMESPACE  = <ns>
  HARMOSTES_PHASE      = prepare | gate | deploy
  HARMOSTES_SPEC       = <JSON: this phase's config from the CR>
  HARMOSTES_SOURCE     = <source artifact / ref / revision>
  HARMOSTES_WORKDIR    = <shared working dir, PVC-cached>
  HARMOSTES_STATE      = <Dapr state key prefix for this workflow>
args: <spec.pluginArgs>

stdout:  last line MUST be a JSON object:
         { "artifact": "<path>", "changed": bool,
           "event": <optional payload>, "status": "ok" }
exit 0 = success   ·   exit nonzero = failure (reported on the CR status)
```

- A **gate** plugin uses the same contract; **exit 0 = green**, nonzero = failed.
  Its **stderr** becomes the feedback the agent receives in the next loop turn.
- Plugins may read/write Dapr state via the sidecar (`localhost:3500`) to skip
  work they have done before (prepare checks the last artifact hash; deploy
  records the published commit).

Reference plugins shipped in this repo (see `plugins/`):

| Plugin | phase | Used by | Today's script |
|---|---|---|---|
| `rig-emit` | prepare | llm-wiki (lc4) | `emit-rig.py` |
| `raw-copy` | prepare | llm-wiki (generic) | `rsync` in reconcile.sh |
| `cherry-pick-sync` | prepare | fork-maintenance | `sync-fork.sh` |
| `wiki-lint` | gate | llm-wiki | `gate-lint.sh` (ci-lint.sh) |
| `fork-resolved` | gate | fork-maintenance | `gate-resolved.sh` |
| `git-push` | deploy | llm-wiki | `git push` in agent-sync.sh |
| `fork-replace-deploy` | deploy | fork-maintenance | `host_pr_merge` + tag |

New workflows = new plugins (or reuse existing ones) + a Workflow CR. No
framework code changes.

---

## The agent step (framework-native)

```
                 ┌──────────────── harmostes worker (one warm pi RPC session) ───────────────┐
   work item ──▶ │ pi --mode rpc  ──▶  agent does task, commits                              │
                 │                       │                                                    │
                 │                  run gate plugin  ──▶ green? ──yes──▶ emit work.resolved  │
                 │                       │ no                                                  │
                 │                  feed gate stderr back (followUp, SAME session)           │
                 │                       │ (up to maxFixes)                                    │
                 │                  no fixes left ──▶ emit work.failed                        │
                 └───────────────────────────────────────────────────────────────────────────┘
```

This is exactly today's `harmostes.py task --task-file … --gate … --max-fixes …`,
now invoked by the agent worker from a Workflow CR's `spec.agent`. Unchanged in
spirit; the win is that the *surrounding* choreography (monitor → prepare →
event → here → deploy) is no longer bespoke per workflow.

---

## CRD sketch (Workflow)

See `config/crd/workflows.harmostes.dev.yaml`. Core shape:

```yaml
apiVersion: harmostes.dev/v1alpha1
kind: Workflow
metadata: { name: platform-website, namespace: harmostes }
spec:
  source:      { kind: git, repo: platform-website, branch: master, language: go }
  prepare:     { plugin: rig-emit, output: raw/arch/platform-website/rig.json, detect: changed }
  agent:
    model: zai/glm-5.2
    skill: /skills/wiki/SKILL.md
    tools: [read, bash, edit, grep]
    taskTemplate: arch-sync-lc4
    gate:    { plugin: wiki-lint }
    maxFixes: 3
  deploy:      { plugin: git-push, args: [git@github.com:rezuscloud/llm-wiki.git, main] }
  events:      { onPrepare: wiki.rig.generated, onResolved: wiki.docs.updated }
  cache:       { pvc: harmostes-cache, git: true, go: true, npm: true }
  scaling:     { kind: keda-scaledjob, schedule: "*/30 * * * *" }
status:
  lastProcessedRevision: master@sha1:…
  lastAgentCommit: 42d13c3…
  gateStatus: green
  lastRunAt: "2026-07-09T18:20:58Z"
```

---

## The two workflows, mapped

Every bespoke piece maps to a framework slot. This table is the argument that
the framework fits both.

| Framework | llm-wiki today | fork-maintenance today | Harmostes |
|---|---|---|---|
| source | WikiMap source repo (Flux artifact) | upstream repo + fork | `source.git` |
| monitor | `reconcile.sh` loop (KEDA ScaledJob, cron) | `sync-fork.sh` (CronJob) | monitor controller |
| prepare | `emit-rig.py` (source→RIG) + copy raw/ | cherry-pick customizations | `rig-emit` / `cherry-pick-sync` |
| detect | Dapr revision-hash skip | cherry-pick clean vs conflict | prepare `changed` / `conflict` |
| event | `wiki.docs.updated` | `fork.conflict.needs-resolution` | `work.needs-agent` |
| subscriber | `event-subscriber.py` (always-on) | `conflict-subscriber.py` (always-on) | **one** generic subscriber |
| agent | `agent-sync.sh` → harmostes (arch-sync) | `resolve-conflict.sh` → harmostes | harmostes RPC + `taskTemplate` |
| gate | `gate-lint.sh` | `gate-resolved.sh` | gate plugin |
| deploy | `git push` to wiki main | replace release branch + tag | `git-push` / `fork-replace-deploy` |
| state | Dapr (revision, commit, ci) | Dapr (revision) | Dapr state |
| scale | KEDA ScaledJob | CronJob + always-on resolver | KEDA per worker type |

The bespoke scripts do not disappear — they become **plugins**, reused or
forked. The bespoke *loops* (reconcile.sh, sync-fork.sh, both subscribers) are
absorbed into the framework and exist exactly once.

---

## Packaging (Helm)

The framework ships as one Helm chart (`chart/`):

```
chart/
  Chart.yaml                      # harmostes vX
  crds/workflows.harmostes.dev.yaml
  templates/
    controller.yaml               # always-on kopf controller (Deployment)
    worker.yaml                   # the generic prepare/agent/deploy worker (ScaledJob templates)
    subscriber.yaml               # the generic event subscriber (Deployment)
    dapr-state.yaml               # Component: state.redis (Valkey)
    dapr-pubsub.yaml              # Component: pubsub.redis
    rbac.yaml                     # CRUD on workflows.harmostes.dev + Jobs/ConfigMaps
    cache-pvc.yaml                # shared PVC StorageClass (optional)
  values.yaml                     # controller/worker resources, Dapr refs, KEDA, cache
```

Installing a workflow = applying a `Workflow` CR (+ any new plugin image). The
chart is installed once per cluster.

---

## Migration path (incremental, non-breaking)

The live system stays green throughout. No big-bang.

1. **Framework skeleton.** CRD + chart + generic controller/worker/subscriber,
   with the harmostes RPC primitive embedded. Deploy in a new `harmostes`
   namespace, parallel to both existing platforms.
2. **Port llm-wiki first** (smaller blast radius, we just hardened it). Wrap
   `emit-rig` / `gate-lint` / `git-push` as plugins; express the 5 WikiMaps as
   `Workflow` CRs. Run parallel to the old controller; diff the outputs.
3. **Port fork-maintenance.** `cherry-pick-sync` / `fork-resolved` / `fork-
   replace-deploy` as plugins; forks as `Workflow` CRs. Run parallel; trigger a
   real conflict to compare against the proven resolver.
4. **Cut over.** Flip the WikiMap/Fork CRs to point at harmostes; retire the
   bespoke controllers + per-platform subscribers. The Dapr fabric + Valkey +
   cache PVC are reused, not rebuilt.

Each step is independently revertible (the old path keeps running until step 4).

---

## Open decisions (need a call before building)

1. **Controller language.** **kopf (Python)** — consistent with `harmostes.py`
   + the Python/bash stack, fastest to ship, good for this event rate. vs
   **controller-runtime (Go)** — industry standard, type-safe, more robust at
   scale. *Recommendation: kopf for v1; the CRD + plugin contract + Dapr fabric
   are language-agnostic, so a Go rewrite of just the controller is possible
   later without touching workflows.*
2. **Generic `Workflow` CRD vs domain CRDs (WikiMap/ForkMap).** Generic =
   framework purity, one reconciler. Domain = self-documenting, typed. 
   *Recommendation: generic `Workflow` as the framework primitive; keep
   WikiMap/ForkMap as optional thin CRDs that *project* onto a Workflow if the
   domain wants typed ergonomics later.*
3. **Plugin distribution.** **Container images** (a plugin = an image with the
   standard entrypoint) = most extensible, isolatable. **ConfigMap scripts** =
   lightweight, fast iteration (today's fork-maintenance pattern). *Recommendation:
   support both; the worker resolves `plugin: {image: …}` or `plugin: {configMap: …}`.*
4. **Always-on controller vs event-only.** A tiny always-on controller gives
   reactive CR watches + status reconciliation. The alternative (no always-on;
   Flux notifications + cron trigger the monitor) saves the one always-on pod.
   *Recommendation: always-on controller — the cost is trivial and reactivity +
   status are worth it.*
