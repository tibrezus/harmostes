# harmostes

<!-- pi-session download verification placeholder (#251) -->

> ἁρμοστής — *one who fits things together.*

A **Kubernetes-native orchestration platform** for workflows that combine
deterministic operations with explicitly non-deterministic operations such as
agentic reasoning. Its core is a **graph-native workflow kernel**: a
deterministic orchestrator schedules typed nodes and advances state from their
recorded outputs, while all interpretation happens behind explicit node
boundaries and must be deterministically validated before it becomes
authoritative.

## Where the documentation lives

Detailed documentation is **not** in this repository — only this index and the
canonical glossary. The rest lives in the places below.

| For | Go to |
|---|---|
| Canonical glossary (domain language) | [`CONTEXT.md`](./CONTEXT.md) (this repo) |
| Architecture decisions (ADRs) | [GitHub wiki → ADRs](https://github.com/tibrezus/harmostes/wiki) |
| Legacy phase-loop architecture (design history) | [wiki → Legacy Architecture](https://github.com/tibrezus/harmostes/wiki/Legacy-Architecture--Phase-Loop) |
| Plugin contract (legacy executor) | [wiki → Plugin Interface](https://github.com/tibrezus/harmostes/wiki/Plugin-Interface--legacy) |
| Webhook triggers (design note) | [wiki → Event-Driven Triggers](https://github.com/tibrezus/harmostes/wiki/Design-Note--Event-Driven-Triggers) |

The ADRs are the source of truth for *why* the architecture is shaped this way.
If something here and an ADR disagree, the ADR wins.

## PR Review (event-armed)

PR-review workflows are event-armed (ADR-0006): a git host sends the
consolidated `pull_request` event to `POST /webhook/{workflow-name}`
(one webhook per watched repo, HMAC-signed). The Review-Ready Gate —
native Go in the worker — proceeds only when the trigger label is
present AND every merge-rule required context is green at the PR head
SHA; red CI is a silent non-event; a moved head re-arms at the new SHA.
The verdict posts as an issue comment carrying the trailer
`<!-- pr-review: DECISION @ sha -->` and consumes the label.

## The model in one paragraph

A **Workflow** is a graph of typed **Nodes**. The kernel is deterministic: it
may schedule nodes, record their inputs/outputs, evaluate rules, and advance
state — never interpret. Non-determinism (agents, approvals, external judgment)
lives only inside explicit nodes and must emit a **Node Result Envelope**; claims
from non-deterministic nodes are **not authoritative until deterministic
validation** promotes them. Harmostes is the **canonical orchestration
historian**, tracking **Implementation Attempts** reconciling toward **Targeted
States** across bounded external system surfaces. See `CONTEXT.md` and the ADRs
for the precise terms.

## Repository layout

```
harmostes.py            # the RPC primitive: task → gate → feedback (proven, standalone)
CONTEXT.md              # canonical glossary — read first
api/v1alpha1/           # Workflow + Pipeline CRD Go types
chart/                  # the Helm chart (controller, worker, ui)
cmd/                    # harmostes-{controller,worker,agent,ui} entrypoints
internal/               # controller, worker, agent, graph executor, ui, webhook, dapr, observability
plugins/                # reference plugins (rig-emit, merge-sync, …) + their README
examples/               # task-template + platform notes (workflow creation lives in the UI)
```

## The primitive (standalone)

`harmostes.py` is usable on its own — the task→gate→feedback loop over a
`pi --mode rpc` subprocess:

```bash
harmostes task \
  --skill /skills/wiki/SKILL.md --model zai/glm-5.2 --tools read,bash,edit,grep \
  --workdir /repo --task-file task.txt \
  --gate "bash gate.sh /repo" \
  --max-fixes 3 --log /tmp/events.jsonl
```

Exit `0` gate green · `1` failed after `--max-fixes` · `2` pi error. The agent's
API key (`LITELLM_API_KEY`) is passed through. It speaks pi's
[RPC JSONL protocol](https://pi.dev/docs/latest/rpc). pi has **no per-tool
approval and no sandbox** — the tool allowlist + external sandboxing are the
safety levers (see [security](https://pi.dev/docs/latest/security)).

This primitive is what the agent worker runs internally, driven from a Workflow
CR instead of CLI flags.

<!-- test trigger for adversarial pr-review 1784298180 -->
