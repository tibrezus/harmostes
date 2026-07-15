# harmostes

A **Kubernetes framework for LLM-driven automation**: pipelines that are
*mostly deterministic* and *validated by an LLM in a loop*. You declare a
pipeline as a `Workflow` custom resource; the framework monitors a source, runs
**deterministic pluggable components** to do everything that can be done without
intelligence, and — only where interpretation is needed — drives an **LLM agent**
that must pass a **deterministic gate** before its work is published. **Dapr is
the event + state fabric** connecting every stage.

> ἁρμοστής — *one who fits things together.* Deterministic parts and LLM parts,
> fitted into one reconciling loop.

**Status:** shaping. The core primitive (`harmostes.py`, the task→gate→feedback
RPC loop) is proven in production by
[`tibrezus/llm-wiki`](https://github.com/tibrezus/llm-wiki) and the
fork-maintenance platform. The framework (operator + CRD + plugin model) is the
generalization of what those two already share. See
[**ARCHITECTURE.md**](ARCHITECTURE.md).

---

## The loop

```
  monitor ──▶ prepare ──▶ (changed?) ──▶ agent ──▶ gate ──▶ deploy ──▶ state
  (source)   deterministic  no: skip     LLM +     fail:     deterministic
             plugin                      feedback  feed back  plugin
                                         loop      to agent
```

- **monitor** watches a source (git revision, schedule, upstream fork, event).
- **prepare** is a deterministic plugin (emit a RIG, cherry-pick a sync branch).
- **agent** is the framework core — the harmostes RPC primitive: the agent does
  the interpretive work, commits.
- **gate** is a deterministic plugin (lint, build, signatures). On failure its
  stderr is fed back to the **same warm agent session**, up to `maxFixes`.
- **deploy** is a deterministic plugin (push to a wiki, replace a release
  branch). **state** (Dapr) lets the next monitor skip.

The LLM is never the first resort and never the last word.

## What's here

```
harmostes.py                          # the RPC primitive: task → gate → feedback (proven)
ARCHITECTURE.md                       # the framework design (read this first)
config/crd/workflows.harmostes.dev.yaml   # the Workflow CRD
examples/workflow-llm-wiki.yaml       # llm-wiki as a Workflow (lc4 + generic)
examples/workflow-fork-maintenance.yaml   # fork-maintenance as a Workflow
docs/plugin-interface.md              # the plugin contract (env, stdout JSON, exit codes)
plugins/README.md                     # reference plugins (rig-emit, cherry-pick-sync, …)
controller/                           # the k8s operator (planned — see ARCHITECTURE §migration)
chart/                                # the Helm chart (planned)
```

## The primitive (today, standalone)

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
[RPC JSONL protocol](https://pi.dev/docs/latest/rpc), reusing pi's own
`--skill/--model/--tools` handling (provider auth, skill loading, **tool
allowlist**). pi has **no per-tool approval and no sandbox** — the tool allowlist
+ external sandboxing are the safety levers; see
[security](https://pi.dev/docs/latest/security).

This primitive is what the framework's **agent worker** runs internally, driven
from a Workflow CR's `spec.agent` instead of CLI flags.

## Why a framework (not a script)

llm-wiki and fork-maintenance turned out to be ~90% the same system — the same
CR → deterministic-prepare → Dapr event → always-on subscriber → agent → gate →
deploy → Dapr state/scale skeleton — with only the *plugin contents* differing
(RIG-emit vs cherry-pick; wiki-lint vs fork-resolved; git-push vs
replace-deploy). harmostes absorbs the skeleton (run once) and leaves the
plugins per workflow. The mapping is in [ARCHITECTURE.md](ARCHITECTURE.md#the-two-workflows-mapped).

The next workflows (anything "watch a thing, mostly do it deterministically,
have an agent finish + validate it") become a Workflow CR + a plugin, not a new
bespoke controller.
