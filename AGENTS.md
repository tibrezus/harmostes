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

The version moves only through the release machine (ADR-0020, Amendment
2026-10-26). For agent work:

- **Commit types:** `fix:` / `refactor:` / `docs:` / `test:` / `ci:` /
  `chore:` for ordinary agent work. `feat:` is **owner-directed feature
  work only** — it bumps the MINOR, and the release gate holds it until
  the owner adds `minor-approved`. One `feat:` per milestone: issues land
  as `fix:`; the capstone merge is the `feat:`; the held release PR
  batches everything into ONE minor when the owner labels it.
- **Never hand-craft a release PR.** Only release-please opens them; the
  gate rejects anything else and the run goes red. Bot stuck? Fix the
  cause (usually an unconsumable title) and re-run *Release Please* —
  #771/#782 ("Manual release PR" → feat shipped as patch, changelog entry
  lost) are the cautionary tales.
- **Titles must be conventional** — the PR title IS the squash commit, and
  a title release-please cannot parse is invisible: the change merges but
  NEVER ships. Enforced by the title-lint workflow.
- Patches need no approval — `fix:` merges ship continuously.
