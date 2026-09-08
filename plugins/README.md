# Harmostes plugins

Reference plugins shipped with the framework. Each is the deterministic logic
extracted from a real workflow; new workflows reuse these or add their own.
ADR-0011: every production plugin is a built-in — it ships in the worker image
via `Dockerfile.worker` + `builtinPlugins()` and resolves by bare name.
See the [Plugin Interface](https://github.com/tibrezus/harmostes/wiki/Plugin-Interface--legacy) page on the wiki for the contract.

| Plugin | role | Used by | Provenance (today's script) |
|---|---|---|---|
| `rig-emit` | prepare | llm-wiki (lc4) | `emit-rig.py` (universal multi-language RIG generator) |
| `raw-copy` | prepare | llm-wiki (generic) | `rsync` of source into `raw/<project>/` |
| `fork-sync` | prepare | fork-maintenance | `sync-fork.sh` entry point (merge/subtree/mapping modes; built-in since ADR-0011) |
| `workspace` | prepare | pr-review | PR workspace provisioning (context, diff, CI, head-SHA clone) |
| `pr-review` | gate | pr-review | review.json output contract (decision + reviewed_sha + verdict trailer) |
| `post-review` | deploy | pr-review | verdict comment + label consume (moved-head guard) |
| `wiki-lint` | gate | llm-wiki | `gate-lint.sh` → full `ci-lint.sh` (markdownlint, mdlint, remark, mermaid, likec4, health, RIG compliance) |
| `fork-maintenance` | gate | fork-maintenance | `gate-resolved.sh` (markers + `validate-fork.sh` + patch signatures) |
| `git-push` | deploy | llm-wiki | rebase onto FETCH_HEAD + union-merge changelog + `git push HEAD:main` |
| `fork-merge-deploy` | deploy | fork-maintenance | PR-merge sync branch into release (append-only) + tag `v…-rezus.N` |

**Built-in plugins** (worker image, registered in `builtinPlugins()`, resolvable
by bare name): `noop`, `rig-emit`, `wiki-lint`, `git-push`, `workspace`,
`pr-review`, `post-review`, `fork-sync`. The remaining rows (`raw-copy`,
`fork-maintenance`, `fork-merge-deploy`) and `divergence-track` (above) are
**not** built-ins — they are delivered via the fork-maintenance engine
ConfigMap mount (`/workspace/…`) or the legacy `pluginConfigMaps` path; a
`{name: …}` plugin ref to them fails at resolve time.

## Adding a plugin

1. Write a command that follows the [contract](https://github.com/tibrezus/harmostes/wiki/Plugin-Interface--legacy)
   (env in, JSON-on-stdout out, exit codes).
2. Ship it as an **image** (`FROM` a base with your toolchain, `ENTRYPOINT`
   your command) **or** a **ConfigMap script** (if the worker image already has
   the runtime).
3. Reference it from a Workflow CR: `spec.<phase>.plugin: { name, image|configMap, args }`.

No framework code changes. The framework discovers the plugin from the CR and
invokes it with the standard environment.

## Plugin layout (image form)

```
my-plugin/
  Dockerfile        # FROM ghcr.io/tibrezus/harmostes-worker-base; ENTRYPOINT ["my-plugin.sh"]
  my-plugin.sh
  README.md
```

The worker base image provides: git, the harmostes RPC primitive, Dapr
sidecar client, the PVC cache mounts. Plugins only add their domain toolchain
(Go, Zig, Chromium, language SDKs, …).

## divergence-track — fork-divergence integrity (capture / reapply / verify)

Deterministic fork-divergence tracking for fork-maintenance syncs. Catches a
sync that drops an added fork feature (a helm chart, licensing code, a workflow)
that the build gate cannot see. Three passes over the git *trees* (no LLM,
immune to squash/merge-base drift):

- `capture <wd> <upstream> <fork> <out.json> [additive-file] [deletions-file]`
- `reapply <wd> <fork> <baseline.json>`  (self-heal dropped roots from the release)
- `verify  <wd> <baseline.json>`         (gate: exit 0 green / 1 lost)

Deliberately **not** a worker-image built-in (ADR-0011 #368-6: the built-in
copy had drifted stale from the engine source). The engine's mounted copy at
`/workspace/scripts/divergence-track.sh` (rendered by the harmostes chart into
the `fork-maintenance-scripts` ConfigMap) is the **only** path — invoked by
`sync-fork.sh` via `$SCRIPT_DIR`; a graph-native workflow reaches it through
that mount, not by bare plugin name (a `{name: divergence-track}` plugin ref
fails at resolve time by design). It serves as the sync's `prepare` baseline +
a `verify` gate.
