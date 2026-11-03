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

## Versions are NOT decided here either

The version moves only through the release machine (ADR-0020, as amended
2026-11-01 — patch-only automatic cuts, milestone-driven minors). For
agent work:

- **Commit types:** `fix:` / `refactor:` / `docs:` / `test:` / `ci:` /
  `chore:` / `feat:` — all of them bump the PATCH and ship continuously.
  The release machine pins the native strategy `always-bump-patch`
  (release-please-config.json): the version NEVER rises by X.Y+1 on its
  own, no matter what a commit is labeled. `feat:` still means feature
  work in the CHANGELOG grouping — it is no longer a version event.
- **Minors are milestone closures.** Only a commit carrying the native
  `Release-As: vX.Y.0` trailer can cut a minor (the strategy base honors
  it above the patch-only strategy), and that commit is reserved for
  closing a milestone — agents never add it. The gate holds the resulting
  release PR until the owner adds `minor-approved`.
- **Never hand-craft a release PR.** Only release-please opens them; the
  gate rejects anything else and the run goes red. Bot stuck? Fix the
  cause (usually an unconsumable title) and re-run *Release Please* —
  #771/#782 ("Manual release PR" → feat shipped as patch, changelog entry
  lost) are the cautionary tales.
- **Titles must be conventional** — the PR title IS the squash commit, and
  a title release-please cannot parse is invisible: the change merges but
  NEVER ships. Enforced by the title-lint workflow.
- Everything merged ships as a patch without approval — including
  `feat:`. v1.11.0 (a wrapper fix cut as a minor via the old default
  mapping) is the cautionary tale; its policy is this section.
