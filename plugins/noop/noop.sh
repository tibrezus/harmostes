#!/usr/bin/env sh
# harmostes builtin "noop" prepare/deploy/gate plugin — for smoke tests + the
# deterministic-skip path.
#
# Behavior:
#   prepare: prints {"changed":..., "artifact":"noop"}. Default changed=true
#            (proceed to the agent). Pass arg "skip" (or set
#            HARMOSTES_NOOP_CHANGED=false) for changed=false → deterministic
#            skip (the pipeline short-circuits before the agent).
#   gate:    exit 0 (green).
#   deploy:  prints {"artifact":"noop","status":"ok"}.
changed="${HARMOSTES_NOOP_CHANGED:-true}"
artifact="${HARMOSTES_NOOP_ARTIFACT:-noop}"
if [ "${HARMOSTES_PHASE:-}" = "prepare" ]; then
  [ "${1:-}" = "skip" ] && changed=false
  echo "{\"changed\":$changed,\"artifact\":\"$artifact\",\"status\":\"ok\"}"
else
  echo "{\"artifact\":\"$artifact\",\"status\":\"ok\"}"
fi
exit 0
