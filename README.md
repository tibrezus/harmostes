# harmostes

Shared **pi.dev RPC orchestration** for automated agent workflows. Used by
[`tibrezus/llm-wiki`](https://github.com/tibrezus/llm-wiki) (the docs agent) and
the fork-maintenance platform — both run an agent, validate the result, and fix
failures. harmostes is that loop, factored out.

## The primitive

```
agent task  →  GATE  →  (on failure) feed the gate's error back to the SAME
                          pi session  →  re-run the gate, up to N fixes
```

This replaces the cold `pi --print` re-invocations both pipelines used: instead
of restarting the agent from scratch on each validation failure, the fix is a
**continuation of one warm RPC session** (the agent remembers what it just did),
and the orchestrator gets **full event observability** (every tool call) instead
of only final text.

It speaks pi's [RPC JSONL protocol](https://pi.dev/docs/latest/rpc) over a
`pi --mode rpc` subprocess — reusing pi's own `--skill/--model/--tools` handling
(provider, auth, skill loading, **tool allowlist**) rather than re-implementing
them. The pi [SDK](https://pi.dev/docs/latest/sdk) (`createAgentSession`) is the
in-process Node equivalent; RPC is the language-agnostic choice for a CLI
callable from bash + Python workflows. (pi has **no per-tool approval and no
sandbox** — the tool allowlist + external sandboxing are the safety levers; see
[security](https://pi.dev/docs/latest/security).)

## Usage

```bash
harmostes task \
  --skill /path/to/SKILL.md --model zai/glm-5.2 --tools read,bash,edit \
  --workdir /repo --task-file task.txt \
  --gate "bash validate.sh && git grep -q '<<<<<<' -- . || true" \
  --max-fixes 3 --log /tmp/events.jsonl
```

- `--gate` is run after each agent turn; exit 0 = pass, non-zero = feed `stderr`
  back to the agent and retry (same session).
- Exit codes: `0` gate green · `1` gate failed after `--max-fixes` · `2` pi error.
- The model's API key (e.g. `ZAI_API_KEY`) is passed through to pi from the env.

## Install

`harmostes.py` is a single file — symlink/symlink it onto `PATH` as `harmostes`,
or run `python3 harmostes.py task ...`. Requires `pi` on `PATH`.

## Why

Before harmostes, each pipeline did `pi --print ...` and, on CI/gate failure,
re-invoked `pi --print` with a fresh prompt — cold restart, no visibility, and
the agent re-read the skill + re-explored every time. harmostes is the one place
that owns "run an agent, validate, fix in context, repeat."
