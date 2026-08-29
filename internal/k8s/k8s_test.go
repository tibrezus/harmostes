package k8s

import (
	"context"
	"errors"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func newTestClient(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(Scheme()).
		WithStatusSubresource(&v1alpha1.Workflow{}).
		WithObjects(objs...).
		Build()
}

func testWorkflow(name string) *v1alpha1.Workflow {
	wf := &v1alpha1.Workflow{}
	wf.Name = name
	wf.Namespace = "harmostes"
	return wf
}

// TestPatchStatusMutatesLiveState: the mutate closure receives the CURRENT
// status and the write lands (#257 — closures must derive from live state).
func TestPatchStatusMutatesLiveState(t *testing.T) {
	wf := testWorkflow("w")
	wf.Status.ReviewReady = &v1alpha1.ReviewReadyStatus{
		ArmedPR:      42,
		DispatchedAt: &metav1.Time{Time: metav1.Now().Time},
	}
	cl := newTestClient(t, wf)
	sp := StatusPatcher{Client: cl, Namespace: "harmostes"}

	if err := sp.PatchStatus(context.Background(), "w", func(s *v1alpha1.WorkflowStatus) {
		// The closure sees LIVE state (the marker above), not a snapshot.
		if s.ReviewReady == nil || s.ReviewReady.DispatchedAt == nil {
			t.Fatal("closure did not receive live status — stale read")
		}
		s.ReviewReady.LastDecision = "waiting"
	}); err != nil {
		t.Fatal(err)
	}

	var got v1alpha1.Workflow
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "w", Namespace: "harmostes"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.ReviewReady.LastDecision != "waiting" || got.Status.ReviewReady.DispatchedAt == nil {
		t.Fatalf("mutation lost or marker dropped: %+v", got.Status.ReviewReady)
	}
}

// TestPatchStatusRetriesOnConflict: a concurrent writer bumps the
// resourceVersion between our Get and Patch; the optimistic-lock precondition
// must surface as a conflict and the retry must land the mutation on the
// fresh state (#257 — no silent lost updates).
func TestPatchStatusRetriesOnConflict(t *testing.T) {
	wf := testWorkflow("w")
	wf.Status.ReviewReady = &v1alpha1.ReviewReadyStatus{ArmedPR: 42}
	cl := newTestClient(t, wf)

	attempts := 0
	cl = fake.NewClientBuilder().
		WithScheme(Scheme()).
		WithStatusSubresource(&v1alpha1.Workflow{}).
		WithObjects(wf).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				attempts++
				if attempts == 1 {
					return apierrors.NewConflict(
						schema.GroupResource{Group: "harmostes.dev", Resource: "workflows"},
						"w", errors.New("the object has been modified; please apply your changes to the latest version"))
				}
				return c.Status().Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	sp := StatusPatcher{Client: cl, Namespace: "harmostes"}

	if err := sp.PatchStatus(context.Background(), "w", func(s *v1alpha1.WorkflowStatus) {
		s.ReviewReady.LastDecision = "waiting"
	}); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("expected exactly one conflict then success, got %d attempts", attempts)
	}
	var got v1alpha1.Workflow
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "w", Namespace: "harmostes"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.ReviewReady.LastDecision != "waiting" {
		t.Fatalf("mutation not landed after retry: %+v", got.Status.ReviewReady)
	}
}

// TestGetStatusReturnsFresh mirrors the cluster state; a stale snapshot must
// never leak into decisions.
func TestGetStatusReturnsFresh(t *testing.T) {
	wf := testWorkflow("w")
	wf.Status.ReviewReady = &v1alpha1.ReviewReadyStatus{ArmedPR: 7}
	cl := newTestClient(t, wf)
	sp := StatusPatcher{Client: cl, Namespace: "harmostes"}

	st, err := sp.GetStatus(context.Background(), "w")
	if err != nil {
		t.Fatal(err)
	}
	if st.ReviewReady == nil || st.ReviewReady.ArmedPR != 7 {
		t.Fatalf("GetStatus returned stale/empty status: %+v", st.ReviewReady)
	}

	// A write is visible to the next GetStatus (freshness contract).
	if err := sp.PatchStatus(context.Background(), "w", func(s *v1alpha1.WorkflowStatus) {
		s.ReviewReady.ArmedPR = 8
	}); err != nil {
		t.Fatal(err)
	}
	if st, err = sp.GetStatus(context.Background(), "w"); err != nil {
		t.Fatal(err)
	}
	if st.ReviewReady.ArmedPR != 8 {
		t.Fatalf("GetStatus not fresh after write: %+v", st.ReviewReady)
	}
}
