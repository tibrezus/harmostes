package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// The sessions header owns the workflow filter inline (#245 — the global
// topbar is gone). Active option marked, all-workflows default present.
func TestSessionsHeaderInlineWorkflowSelect(t *testing.T) {
	s := newAttemptTestServer(t,
		&v1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "pr-review-x", Namespace: "test-ns", Labels: map[string]string{v1alpha1.OwnerLabel: "alice"}},
		},
		&v1alpha1.Attempt{
			ObjectMeta: metav1.ObjectMeta{
				Name: "attempt-pr-review-x-1", Namespace: "test-ns",
				Labels: map[string]string{v1alpha1.OwnerLabel: "alice", "harmostes.dev/workflow": "pr-review-x"},
			},
			Status: v1alpha1.AttemptStatus{Runs: []v1alpha1.RunRecord{{Name: "run-1", Phase: "succeeded"}}},
		},
	)
	req := httptest.NewRequest(http.MethodGet, "/sessions?workflow=pr-review-x", nil)
	req = req.WithContext(withTestIdentity(req.Context()))
	rec := httptest.NewRecorder()
	s.handleSessionsView(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `navParam('workflow'`) {
		t.Error("sessions header missing inline workflow select")
	}
	if !strings.Contains(body, `— all workflows —`) {
		t.Error("all-workflows default option missing")
	}
	if !strings.Contains(body, `value="pr-review-x" selected`) {
		t.Error("active workflow option not marked selected")
	}
}
