package v1alpha1

import "testing"

// TestCICompletedWakeNotRequestShaped pins the two vocabulary properties the
// CI-notify wake depends on (#r33): the action WAKES the gate (the handler
// annotates and the Review-Ready Gate re-evaluates) and is NOT
// request-shaped (a CI notification must never supersede a live review
// claim — only label-touching actions may).
func TestCICompletedWakeNotRequestShaped(t *testing.T) {
	if !PullRequestWakeActions["ci_completed"] {
		t.Fatal("ci_completed is not a wake action — the CI-notify contract is dead")
	}
	if RequestShaped("ci_completed") {
		t.Fatal("ci_completed must not be request-shaped — a CI notification may not supersede a live claim")
	}
	for _, a := range []string{"labeled", "unlabeled", "label_updated"} {
		if !RequestShaped(a) {
			t.Errorf("%s must stay request-shaped", a)
		}
	}
}
