package main

// Attempt-scoped resumption (ADR-0008, #334). Runs inside one Attempt (same
// workflow, same trigger identity — for pr-review, the same PR head) inherit
// what earlier runs of the attempt established instead of starting from zero:
//
//   - The executor is seeded with green node results from the attempt's
//     envelope ledger; nodes marked resume:"green" skip re-execution.
//   - The agent receives a handoff brief. Two modes, per the design
//     directive: a predecessor that was INTERRUPTED (killed at the bound,
//     dropped — no verdict, automatic re-arm) yields CONTINUE — its work is
//     presented as the successor's own work in progress. A predecessor that
//     ended deliberately or under a changed premise (verdict consumed, human
//     re-label, standdown) yields SUMMARY — prior facts as background, not a
//     continuation.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/agent"
	"github.com/tibrezus/harmostes/internal/graph"
	"github.com/tibrezus/harmostes/internal/worker"
)

// handoffResponseTail caps how much of a predecessor's final assistant
// message rides the brief. The full transcript stays in Dapr state; the brief
// carries orientation, not the whole conversation.
const handoffResponseTail = 1200

// buildAttemptResume fetches the attempt and returns the executor seed plus
// the handoff brief. Best-effort by design: any failure degrades to a fresh
// run (empty seed, empty brief), never to a failed run.
func buildAttemptResume(ctx context.Context, cl client.Client, namespace string, deps worker.Deps, workflow, runID string) (map[string]graph.PriorResult, string) {
	attemptName := os.Getenv("HARMOSTES_ATTEMPT")
	if attemptName == "" {
		return nil, "" // non-Job context: no attempt to resume within
	}
	var at v1alpha1.Attempt
	if err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: attemptName}, &at); err != nil {
		logf("resume: fetch attempt %s: %v — running fresh", attemptName, err)
		return nil, ""
	}
	seed := graph.PriorResultsFromEnvelopes(at.Status.NodeResults, runID)
	if len(seed) > 0 {
		logf("resume: %d node result(s) available from earlier runs of %s", len(seed), attemptName)
	}
	return seed, buildHandoffBrief(&at, deps, workflow, runID)
}

// buildHandoffBrief composes the agent-facing brief. Empty when the attempt
// has no prior runs.
func buildHandoffBrief(at *v1alpha1.Attempt, deps worker.Deps, workflow, runID string) string {
	if at == nil {
		return ""
	}
	prior := priorRuns(at, runID)
	if len(prior) == 0 {
		return ""
	}
	mode := classifyHandoff(at, os.Getenv("HARMOSTES_TRIGGER_ACTION"), prior)

	var b strings.Builder
	if mode == "continue" {
		b.WriteString("## Handoff — CONTINUE interrupted work\n\n")
		b.WriteString("The previous run of this attempt was INTERRUPTED before finishing (killed at the time bound or lost) — no verdict or outcome was posted. Its work is yours to continue: treat what is recorded below as your own work in progress. Do NOT redo reading, analysis, or checks it already completed; verify cheaply only where continuation requires it.\n\n")
	} else {
		b.WriteString("## Handoff — summary of previous runs (deliberate restart)\n\n")
		b.WriteString("Previous runs of this attempt ended intentionally or under a changed premise. The facts below are background: re-verify anything you build on, and do not treat them as your own unfinished work.\n\n")
	}

	// Run facts.
	b.WriteString(fmt.Sprintf("Prior runs: %d", len(prior)))
	if last := prior[len(prior)-1]; last.Phase != "" {
		b.WriteString(fmt.Sprintf(", last ended %q", last.Phase))
		if !last.EndedAt.Time.IsZero() {
			b.WriteString(fmt.Sprintf(" at %s", last.EndedAt.UTC().Format("15:04:05Z")))
		}
	}
	b.WriteString("\n\n")

	// Node facts from the envelope ledger (excluding this run's own).
	ok, failed := nodeFacts(at.Status.NodeResults, runID)
	if len(ok) > 0 {
		b.WriteString("Completed nodes (do not redo):\n")
		for _, n := range ok {
			b.WriteString(fmt.Sprintf("- %s: %s\n", n.NodeID, n.Summary))
		}
	}
	if len(failed) > 0 {
		b.WriteString("Nodes that failed (understand why before retrying):\n")
		for _, n := range failed {
			b.WriteString(fmt.Sprintf("- %s: %s\n", n.NodeID, n.Summary))
		}
	}

	// Run clock (#336): the agent paces itself against the bound instead of
	// discovering it at kill time. Unlimited runs get the soft target.
	if bound := at.Spec.RunBound; bound != "" {
		b.WriteString(fmt.Sprintf("\nRun clock: hard bound %s from dispatch — the review MUST be written before it expires.\n", bound))
	} else {
		b.WriteString("\nRun clock: no hard bound, but the review is expected within ~10 minutes — write it while the findings are fresh.\n")
	}

	// Transcript orientation from the most recent predecessor's session
	// (best-effort: missing/corrupt session degrades to the facts above).
	if sess := priorSession(deps, workflow, prior); sess != nil && len(sess.Turns) > 0 {
		last := sess.Turns[len(sess.Turns)-1]
		b.WriteString(fmt.Sprintf("\nTranscript: %d turn(s), %d in / %d out tokens total. Last turn: %q.\n",
			len(sess.Turns), sess.TotalUsage.Input, sess.TotalUsage.Output, last.Label))
		if tail := tailText(last.Response, handoffResponseTail); tail != "" {
			b.WriteString(fmt.Sprintf("\nWhere it stopped (last response, tail):\n\n%s\n", tail))
		}
	}
	return b.String()
}

// classifyHandoff applies the deterministic mode rule: a human label wake or
// a claim history of deliberate endings (consumed/closed) forces summary;
// otherwise the predecessor's terminal phase decides — finished ⇒ summary,
// anything else (failed, still "running" = superseded/killed) ⇒ continue.
func classifyHandoff(at *v1alpha1.Attempt, wakeAction string, prior []v1alpha1.RunRecord) string {
	if wakeAction == "labeled" {
		return "summary"
	}
	if rv := at.Status.Review; rv != nil && rv.Released {
		switch rv.ReleaseReason {
		case "consumed", "closed":
			return "summary"
		}
	}
	last := prior[len(prior)-1]
	if last.Phase == "succeeded" {
		return "summary"
	}
	return "continue"
}

// priorRuns returns the attempt's run records excluding the current run, in
// recorded order (oldest first).
func priorRuns(at *v1alpha1.Attempt, runID string) []v1alpha1.RunRecord {
	var out []v1alpha1.RunRecord
	for _, r := range at.Status.Runs {
		if r.Name != runID {
			out = append(out, r)
		}
	}
	return out
}

// nodeFacts splits the ledger into green and failed facts, latest per node,
// excluding envelopes produced by the current run.
func nodeFacts(envelopes []v1alpha1.NodeResultEnvelope, runID string) (ok, failed []v1alpha1.NodeResultEnvelope) {
	latest := map[string]v1alpha1.NodeResultEnvelope{}
	for _, env := range envelopes {
		if env.RunID == runID {
			continue
		}
		latest[env.NodeID] = env // append order: later overwrites
	}
	for id, env := range latest {
		e := env
		if e.NodeID == "" {
			e.NodeID = id
		}
		switch e.Status {
		case v1alpha1.NodeResultStatusOK:
			ok = append(ok, e)
		case v1alpha1.NodeResultStatusFailed:
			failed = append(failed, e)
		}
	}
	return ok, failed
}

// priorSession loads the most recent predecessor's structured session record
// (the `<wf>:<run>:session` Dapr key written by the session writer). Nil on
// any miss — the brief is additive, never load-bearing.
func priorSession(deps worker.Deps, workflow string, prior []v1alpha1.RunRecord) *agent.SessionRecord {
	if deps.Dapr == nil || len(prior) == 0 {
		return nil
	}
	for i := len(prior) - 1; i >= 0; i-- {
		raw, err := deps.Dapr.GetState(context.Background(), deps.DaprStateStore,
			fmt.Sprintf("%s:%s:session", workflow, prior[i].Name))
		if err != nil || raw == "" {
			continue
		}
		var sess agent.SessionRecord
		if err := json.Unmarshal([]byte(raw), &sess); err != nil {
			continue
		}
		return &sess
	}
	return nil
}

// tailText returns the last ≤max runes of s (whole string when shorter).
func tailText(s string, max int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) <= max {
		return s
	}
	// Rune-safe tail.
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return "…" + string(r[len(r)-max:])
}
