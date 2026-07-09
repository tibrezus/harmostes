# Harmostes plugin interface

A plugin is the unit of deterministic logic in a workflow. There are three
plugin **roles**, all sharing one invocation contract:

| role | phase | contract |
|---|---|---|
| `prepare` | before the agent | produce an artifact; report `changed` |
| `gate` | inside the agent loop | **exit 0 = green**; stderr = feedback to the agent |
| `deploy` | after a green gate | publish the result; record what shipped |

The **agent** step is *not* a plugin — it is the framework core (the harmostes
RPC primitive).

## Distribution

A plugin is referenced from a Workflow CR in one of two ways:

```yaml
prepare:
  plugin:
    name: rig-emit
    image: ghcr.io/tibrezus/harmostes-plugin-rig-emit:latest   # plugin = its own image
    args: ["--language=go"]
# or
    configMap: harmostes-plugins                                # plugin = script in a ConfigMap
```

- **image** — a container image whose entrypoint is the plugin command. Most
  isolatable; can carry its own toolchain (Go, Zig, Chromium…). Recommended for
  heavy/stateful plugins.
- **configMap** — a script mounted into the shared worker image. Fastest to
  iterate; recommended for lightweight plugins. The worker image must already
  contain the script's runtime.

If neither is given, the framework looks up a **built-in** plugin by `name`
(see `plugins/`).

## Invocation contract

The worker invokes the plugin command with this environment:

| Var | Meaning |
|---|---|
| `HARMOSTES_WORKFLOW` | the Workflow CR name |
| `HARMOSTES_NAMESPACE` | namespace |
| `HARMOSTES_PHASE` | `prepare` \| `gate` \| `deploy` |
| `HARMOSTES_SPEC` | JSON: this phase's config from the CR (incl. `args`) |
| `HARMOSTES_SOURCE` | the resolved source (artifact path / git ref / revision) |
| `HARMOSTES_WORKDIR` | shared working directory (PVC-cached across runs) |
| `HARMOSTES_STATE` | Dapr state key prefix for this workflow |
| `HARMOSTES_OUTPUT` | (prepare) where to write the artifact the next phase reads |

Positional args come from `spec.<phase>.plugin.args`.

## Result contract

The **last line of stdout** MUST be a JSON object:

```json
{ "artifact": "raw/arch/platform-website/rig.json",
  "changed": true,
  "event": { "components": 11 },
  "status": "ok" }
```

- `artifact` — path/branch/ref produced (prepare) or published (deploy).
- `changed` — (prepare) whether the agent should run. `false` → framework may
  skip straight to deploy (deterministic fast-path) or stop.
- `event` — optional payload merged into the next Dapr event.
- `status` — `ok` | `skipped` | `failed`.

Exit codes:

- `0` → success. For **gate**, this specifically means **green**.
- non-zero → failure. For **gate**, this means **failed**, and **stderr is
  captured verbatim and fed back to the agent** as the next loop turn's prompt.
- A prepare plugin MAY exit `0` with `changed:false` to say "I looked, nothing
  to do" (the framework records state and stops).

## State (optional, for skip/dedup)

Plugins read/write Dapr state through the sidecar to avoid redoing work:

```bash
# read
curl -sf "http://localhost:3500/v1.0/state/${DAPR_STATE_STORE}/${HARMOSTES_STATE}:hash"
# write
curl -sf -X POST "http://localhost:3500/v1.0/state/${DAPR_STATE_STORE}" \
  -H "Content-Type: application/json" \
  -d "[{\"key\":\"${HARMOSTES_STATE}:hash\",\"value\":\"$NEW_HASH\"}]"
```

Dapr is the abstraction — swap Valkey for PostgreSQL by changing the Component
CR; plugins are unchanged.

## Minimal example (a gate plugin)

```bash
#!/usr/bin/env bash
set -euo pipefail
# HARMOSTES_WORKDIR is the clone under validation.
cd "$HARMOSTES_WORKDIR"

# 1. no conflict markers
git grep -l -E '^(<<<<<<<|>>>>>>>|=======) ' -- . && {
  echo "conflict markers remain" >&2; exit 1; } || true

# 2. build
bash .build/validate.sh || { echo "build failed" >&2; exit 1; }

# 3. green
echo '{"status":"ok"}'
exit 0
```

If `validate.sh` fails, the agent receives "build failed" (+ the build log) as
its next prompt, in the same warm session, and fixes it. That is the whole loop.
