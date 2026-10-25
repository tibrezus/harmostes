# AGENTS.md — harmostes kernel

Routing for agents working in this repository. The full operating manual is
`.agents/skills/harmostes/SKILL.md` (workflow machinery) and the wiki
(one hop from Home).

## Configuration is NOT decided here

The kernel ships **logic and documentation only** — decision values (models,
time windows, thinking, timeouts, budgets, fix bounds) live in the GitOps
deployment repo (`rezusnet/operations/k8s-config`, GitLab) and flow in via
HelmRelease values. `chart/values.yaml` carries empty defaults + `required`
enforcement on purpose.

- **Change a model / window / timeout / budget?** → k8s-config MR. Runbook:
  wiki → *Change a Configuration Value* (one hop from Home). Do NOT edit
  `chart/values.yaml` for this — and do NOT "fix" a failing `required()` by
  re-adding a default: the boundary guard fails that PR (#777 is the
  cautionary tale).
- **Production model ids** appear only in the logic allowlist
  (litellm-provider fallbacks/sampling, sol-pi reducer profile, their gates).
  Everywhere else use obviously-test ids (`litellm/test/...`). Enforced by
  `scripts/boundary-guard.sh` (in `make test` and CI).
- **Adding a NEW decision field** (logic grew a knob): document it on the
  wiki's Configuration Contract page, set it in k8s-config (prod + dev +
  preview), then ship the kernel change — the render fails loudly until the
  deployment decides. Land the k8s-config values FIRST.

Design rationale: `ADR-0011` in the wiki (Amendment 2026-10-25).
