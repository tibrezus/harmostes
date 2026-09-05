---
target: harmostes UI pages (/, /runs, /workflows)
total_score: 18
max_score: 40
na_heuristics: 
p0_count: 2
p1_count: 2
timestamp: 2026-09-03T07-24-11Z
slug: internal-ui-templates-live-harmostes-rezus-cloud
---
# Design Critique — harmostes UI (/, /runs, /workflows, /templates)

Degraded single-context run. Total 18/40 (Poor).

## Heuristics
1 Visibility 2 · 2 Real World 2 · 3 Control 2 · 4 Consistency 2 · 5 Error Prevention 3 · 6 Recognition 1 · 7 Efficiency 1 · 8 Minimalist 1 · 9 Error Recovery 2 · 10 Help 2

## Priority Issues
- [P0] /runs: unscannable wall — no table/columns, every attempt expanded (hashes), ~500 rows/day from 10-min schedules, invisible window filter. → shape
- [P0] Live wall: signal drowned — "dispatch lost ×8" unalarmed, "643 runs" one flat line, identical token counts per row. → shape
- [P1] Machine names as copy (attempt hashes, full host-prefixed subjects). → clarify
- [P1] Operator's two jobs (health at a glance, drill into failure) unanswerable — no summary strip, no failed-first. → shape
- [P2] Inconsistency: /workflows tables vs /runs text walls. → resolved by the /runs rebuild

## Notes
/workflows proves the RezusCloud design system works; data views never adopted it. Observe-only contract clean. Live SSE real.
