package worker

// Test helpers the dispatch tests share with the (moved) review-gate sweep
// tests — local copies, since test files cannot be imported across
// packages (C3 moved the sweep to internal/gate).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/attempt"
	"github.com/tibrezus/harmostes/internal/gate"
	"github.com/tibrezus/harmostes/internal/review"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// testReviewAPI pins the review REST API the dispatcher's gate sweep uses;
// set by pinReviewAPI, nil = the sweep's own default (live forge).
var testReviewAPI *review.RESTAPI

func pinReviewAPI(t *testing.T, srv *httptest.Server, forgejo bool) {
	t.Helper()
	base := srv.URL
	if forgejo {
		base += "/api/v1"
	}
	testReviewAPI = &review.RESTAPI{Client: srv.Client(), TokenLookup: func(string) string { return "tok" }, BaseOverride: base}
	t.Cleanup(func() { testReviewAPI = nil })
}

func gateWorkflow() *v1alpha1.Workflow {
	wf := newWorkflow()
	wf.Spec.ReviewReady = &v1alpha1.ReviewReadySpec{Label: "needs-review", Horizon: "6h"}
	wf.Spec.Config = []byte(`{"repos": ["git.rezus.cloud/tibrez/rhesadox"]}`)
	return wf
}

func clearTriggerEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"HARMOSTES_TRIGGER_PR", "HARMOSTES_TRIGGER_ACTION", "HARMOSTES_TRIGGER_REVISION"} {
		t.Setenv(k, "")
	}
}

func greenPRServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(req.URL.Path, "/pulls/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "deadbeef123"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{{"name": "needs-review"}},
			})
		case strings.Contains(req.URL.Path, "/comments"):
			_ = json.NewEncoder(w).Encode([]any{})
		case strings.Contains(req.URL.Path, "/branch_protections/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"status_check_contexts": []string{"ci / build-test (push)"}})
		case strings.HasSuffix(req.URL.Path, "/statuses"):
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{"context": "ci / build-test (push)", "state": "success"},
			})
		case strings.HasSuffix(req.URL.Path, "/status"):
			_ = json.NewEncoder(w).Encode(map[string]any{"statuses": []map[string]string{
				{"context": "ci / build-test (push)", "status": "success"},
			}})
		case strings.HasSuffix(req.URL.Path, "/check-runs"):
			_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": []any{}})
		default:
			http.NotFound(w, req)
		}
	}))
}

// claimFixture: the rich fixture (objective-kind label + ReviewClaimStatus
// the live-claim selector requires).
func claimFixture(wf *v1alpha1.Workflow, pr, sha string, armedSince time.Time, dispatchedAt *time.Time) *v1alpha1.Attempt {
	obj := attempt.DeriveObjective(wf, attempt.TriggerContext{Revision: sha, Source: "webhook"})
	name := attempt.AttemptName(wf.Name, attempt.Identity(obj))
	at := &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: wf.Namespace,
			CreationTimestamp: metav1.NewTime(armedSince.Add(-time.Minute)),
			Labels: map[string]string{
				"harmostes.dev/workflow":       wf.Name,
				"harmostes.dev/objective-kind": attempt.DeriveKind(wf),
			},
		},
		Spec: v1alpha1.AttemptSpec{WorkflowRef: wf.Namespace + "/" + wf.Name},
	}
	r := &v1alpha1.ReviewClaimStatus{PR: pr, HeadSHA: sha, Label: "needs-review"}
	ts := metav1.NewTime(armedSince)
	r.ArmedSince = &ts
	if dispatchedAt != nil {
		d := metav1.NewTime(*dispatchedAt)
		r.DispatchedAt = &d
	}
	at.Status.Review = r
	return at
}

var (
	_ = gate.GateDeps{}     // the sweep's deps type (wired through DispatchConfig)
	_ = gate.GateDispatch{} // the sweep's dispatch record
	_ = gate.GateWake{}     // the wake event type
)
