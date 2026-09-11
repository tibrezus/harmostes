package v1alpha1

// PullRequestWakeActions are the consolidated pull_request webhook actions
// that wake the Review-Ready Gate (ADR-0006). Everything else (assigned,
// review_requested, …) is a no-op: 200, no annotations. This is the single
// source for the vocabulary; the webhook package normalizes host aliases
// (Forgejo "synchronized" → "synchronize") BEFORE consulting it, so the set
// stays GitHub-shaped (do not add aliases here).
var PullRequestWakeActions = map[string]bool{
	"labeled":          true, // a human or the skill set a label — arm
	"unlabeled":        true, // label removed (also the post-review consume) — re-evaluate
	"synchronize":      true, // new push — head moved, re-arm at new SHA
	"opened":           true,
	"reopened":         true,
	"closed":           true, // disarm path — the gate stands down promptly
	"ready_for_review": true,
	"label_updated":    true, // Forgejo granular-event name; the gate re-verifies state
	"synchronized":     true, // Forgejo alias of synchronize — normalized at the edge
	// ci_completed: the repo's OWN CI pipeline notifies harmostes when a
	// pipeline run finishes (Forgejo Actions emits no run-completion
	// webhook, so until the fork ships one, a final CI step POSTs a
	// pull_request-shaped payload with this action). The gate wakes and
	// re-verifies label ∧ CI at the annotated head — the notification
	// itself carries no authority (r33: the handler stays dumb, the gate
	// is the only evaluator). Kills the up-to-5-min dispatch-poll tail
	// after the last check goes green; the sweep stays as the safety net.
	"ci_completed": true,
}

// requestShapedActions is DERIVED from PullRequestWakeActions, not copied:
// the label-touching subset — the actions that may supersede a live claim.
// Derivation is the drift guard (#357 r18 P2: a hand-copied subset let a
// new wake action silently lose supersede/override power).
var requestShapedActions = deriveRequestShaped()

func deriveRequestShaped() map[string]bool {
	m := map[string]bool{}
	for action := range PullRequestWakeActions {
		switch action {
		case "labeled", "unlabeled", "label_updated":
			m[action] = true
		}
	}
	return m
}

// RequestShaped reports whether a webhook action is label-touching — the
// request-shaped class that may supersede a live review claim. Of the
// request-shaped actions, "labeled" is the breaker's human override
// directly (#328); Forgejo's granular "label_updated" becomes one through
// resolution — the gate resolves add-vs-remove against the label presence
// the evaluator establishes (Result.LabelPresent / humanOverride):
// present = (re-)request, absent or unknown ⇒ no override (#423). This set
// does NOT enforce that — it only makes the action supersede-eligible; the
// direction check lives in the gate (internal/worker/review_gate.go).
func RequestShaped(action string) bool {
	return requestShapedActions[action]
}
