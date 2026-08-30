package worker

import (
	"testing"

	"github.com/tibrezus/harmostes/internal/k8s"
	batchv1 "k8s.io/api/batch/v1"
)

// #277: the dispatcher's client scheme MUST know batchv1 — ListActiveJobs
// (live-Job dedupe, capacity) dies with "no kind is registered" otherwise,
// killing every gated and non-gated dispatch. This bit in production
// because the fake-client tests built their own scheme with batchv1 while
// DispatcherFromEnv hand-rolled a v1alpha1-only one.
func TestDispatcherSchemeKnowsJobs(t *testing.T) {
	d := &Dispatcher{scheme: k8s.Scheme()}
	kinds, _, err := d.scheme.ObjectKinds(&batchv1.Job{})
	if err != nil {
		t.Fatalf("dispatcher scheme must register batchv1.Job: %v", err)
	}
	if len(kinds) == 0 {
		t.Fatal("no kinds registered for batchv1.Job")
	}
	if _, _, err := d.scheme.ObjectKinds(&batchv1.JobList{}); err != nil {
		t.Fatalf("dispatcher scheme must register batchv1.JobList: %v", err)
	}
}
