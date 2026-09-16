package gate

// Test fixtures shared by the gate sweep tests — local copies of the
// worker-package builders (pipeline_test.go) so the gate package does not
// import the worker (C3: the kernel/workflow seam runs through types, not
// test helpers).

import (
	"context"

	"github.com/tibrezus/harmostes/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type fakeStatus struct {
	last    v1alpha1.WorkflowStatus
	patches int
}

func (f *fakeStatus) GetStatus(_ context.Context, _ string) (*v1alpha1.WorkflowStatus, error) {
	st := f.last
	return &st, nil
}

func (f *fakeStatus) PatchStatus(_ context.Context, _ string, mutate func(*v1alpha1.WorkflowStatus)) error {
	f.patches++
	mutate(&f.last)
	return nil
}

func newWorkflow() *v1alpha1.Workflow {
	return &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "ns"},
		Spec: v1alpha1.WorkflowSpec{
			Prepare: v1alpha1.PrepareSpec{Plugin: v1alpha1.PluginRef{Name: "prepare"}},
			Agent: v1alpha1.AgentSpec{
				TaskTemplate: v1alpha1.TaskTemplate{Name: "t"},
				Gate:         v1alpha1.GateRef{Plugin: v1alpha1.PluginRef{Name: "gate"}},
			},
			Deploy: v1alpha1.DeploySpec{Plugin: v1alpha1.PluginRef{Name: "deploy"}},
			Events: &v1alpha1.EventsSpec{OnPrepare: "p", OnResolved: "r", OnFailed: "f"},
		},
	}
}
