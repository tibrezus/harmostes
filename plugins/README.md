# Harmostes plugins

Reference plugins shipped with the framework. Each is the deterministic logic
extracted from a real workflow; new workflows reuse these or add their own.
See [`docs/plugin-interface.md`](../docs/plugin-interface.md) for the contract.

| Plugin | role | Used by | Provenance (today's script) |
|---|---|---|---|
| `rig-emit` | prepare | llm-wiki (lc4) | `emit-rig.py` (universal multi-language RIG generator) |
| `raw-copy` | prepare | llm-wiki (generic) | `rsync` of source into `raw/<project>/` |
| `cherry-pick-sync` | prepare | fork-maintenance | `sync-fork.sh` (fresh upstream + replay customizations) |
| `wiki-lint` | gate | llm-wiki | `gate-lint.sh` → full `ci-lint.sh` (markdownlint, mdlint, remark, mermaid, likec4, health, RIG compliance) |
| `fork-resolved` | gate | fork-maintenance | `gate-resolved.sh` (markers + `validate-fork.sh` + patch signatures) |
| `git-push` | deploy | llm-wiki | rebase onto FETCH_HEAD + union-merge changelog + `git push HEAD:main` |
| `fork-replace-deploy` | deploy | fork-maintenance | replace release branch (force) + tag `v…-rezus.N` |

## Adding a plugin

1. Write a command that follows the [contract](../docs/plugin-interface.md)
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
