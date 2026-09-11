#!/usr/bin/env bash
# git-host.sh — the single bash mapping of git host → (API base, Forgejo?,
# token chain, clone URL). This file is the bash translation of the kernel's
# canonical resolver, internal/review/review.go (ResolveHost + TokenEnvNames);
# cmd/harmostes-worker's test suite enforces the mirror (a table drift between
# Go and bash fails CI). Sourced by the image builtins that talk to forges
# (workspace, post-review) — never executed.
#
# The fork-maintenance engine's chart/fork-maintenance/scripts/git-host.sh is
# a deliberately separate copy: different delivery (engine ConfigMap rendered
# by the chart, not the worker image) and different release cadence. Collapsing
# it into this file would couple image releases to chart releases — recorded
# in ADR-0011 and out of scope for #368.
#
# Function contract: pure, echo to stdout, no globals exported, set -u safe.
host::api_base() { # <host>
  # Test seams (#429): the HARMOSTES_TEST_* vars redirect a base at a stub
  # server so the plugins' bash can be integration-tested in-process.
  # LOOPBACK-ONLY, URL-PARSED (r6 P2): plugin nodes inherit the entire
  # worker process env (cmd/harmostes-worker main.go uses os.Environ()), so
  # a hostile value here would carry HARMOSTES_GITHUB_TOKEN /
  # HARMOSTES_FORGEJO_TOKEN bearer auth to an arbitrary origin from the
  # deploy plugin. A glob is NOT a URL check — http://127.0.0.1:80@evil
  # matches every loopback glob and parses 127.0.0.1:80 as USERINFO — so
  # the value is regex-parsed (scheme, loopback host, numeric port, no
  # userinfo) and anything else is refused loudly to the canonical base.
  local __seam
  case "$1" in
    github.com)      __seam="${HARMOSTES_TEST_GITHUB_API_BASE:-}";;
    git.rezus.cloud) __seam="${HARMOSTES_TEST_FORGEJO_API_BASE:-}";;
  esac
  if [ -n "$__seam" ]; then
    # A glob is not a URL check: `http://127.0.0.1:80@evil.example.com`
    # matches the loopback patterns (curl parses 127.0.0.1:80 as USERINFO)
    # and would carry the bearer token to the remote host (r6 P2).
    if echo "$__seam" | grep -qE '^http://(127\.0\.0\.1|localhost):[0-9]+$'; then
      echo "$__seam"
      return
    fi
    echo "WARN: refusing invalid/non-loopback test seam for $1 ($__seam) — using the canonical base" >&2
  fi
  case "$1" in
    github.com)      echo "https://api.github.com";;
    codeberg.org)    echo "https://codeberg.org/api/v1";;
    git.rezus.cloud) echo "https://git.rezus.cloud/api/v1";;
    *)               echo "https://$1/api/v1";;
  esac
}

host::is_fj() { # <host> → "true"|"false" (Forgejo-shaped API?)
  case "$1" in
    github.com) echo "false";;
    *)          echo "true";;
  esac
}

host::token() { # <host> [required] → first non-empty of the host's token chain
  local required="${2:-}" chain name val=""
  case "$1" in
    github.com)      chain="HARMOSTES_GIT_TOKEN HARMOSTES_GITHUB_TOKEN";;
    codeberg.org)    chain="HARMOSTES_CODEBERG_TOKEN LLM_WIKI_CODEBERG_TOKEN";;
    git.rezus.cloud) chain="HARMOSTES_FORGEJO_TOKEN HARMOSTES_RZC_PASSWORD";;
    *)               chain="HARMOSTES_FORGEJO_TOKEN HARMOSTES_GIT_TOKEN";;
  esac
  for name in $chain; do
    val="${!name:-}"
    [ -n "$val" ] && break
  done
  if [ "$required" = "required" ] && [ -z "$val" ]; then
    echo "ERROR: no token for $1 (tried: $chain)" >&2
    return 1
  fi
  echo "$val"
}

host::clone_url() { # <host> <owner/name> → authenticated clone URL
  local tok
  case "$1" in
    github.com)      tok=$(host::token github.com);      echo "https://x-access-token:${tok}@github.com/$2.git";;
    git.rezus.cloud) tok=$(host::token git.rezus.cloud); echo "https://${HARMOSTES_RZC_USERNAME:-tibrez}:${tok}@git.rezus.cloud/$2.git";;
    codeberg.org)    tok=$(host::token codeberg.org);    echo "https://${tok}@codeberg.org/$2.git";;
    *)               echo "https://$1/$2.git";;
  esac
}

host::is_gitlab() { # <host> → "true"|"false" (GitLab-shaped API?)
  case "$1" in
    gitlab.com|*.gitlab.com) echo "true";;
    *)                       echo "false";;
  esac
}
