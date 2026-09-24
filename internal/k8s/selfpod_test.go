package k8s

import (
	"context"
	"errors"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Mutation probe: these tests went red when DeleteOwnPod read the wrong env
// var (HOSTNAME-only) and when the namespace was hardcoded — restored after.

func TestDeleteOwnPod(t *testing.T) {
	newPod := func(name, ns string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	}

	t.Run("deletes POD_NAME in the given namespace", func(t *testing.T) {
		t.Setenv("POD_NAME", "harmostes-worker-pool-abc12")
		t.Setenv("HOSTNAME", "")
		var cl client.Client = fake.NewClientBuilder().WithScheme(Scheme()).Build()
		// Pod exists in-cluster:
		if err := cl.Create(context.Background(), newPod("harmostes-worker-pool-abc12", "harmostes")); err != nil {
			t.Fatal(err)
		}
		if err := DeleteOwnPod(context.Background(), cl, "harmostes"); err != nil {
			t.Fatalf("self-delete should succeed: %v", err)
		}
		got := &corev1.Pod{}
		err := cl.Get(context.Background(), types.NamespacedName{Name: "harmostes-worker-pool-abc12", Namespace: "harmostes"}, got)
		if err == nil {
			t.Fatal("pod should be deleted")
		}
	})
	t.Run("falls back to HOSTNAME", func(t *testing.T) {
		t.Setenv("POD_NAME", "")
		t.Setenv("HOSTNAME", "harmostes-controller-xyz99")
		cl := fake.NewClientBuilder().WithScheme(Scheme()).Build()
		if err := cl.Create(context.Background(), newPod("harmostes-controller-xyz99", "harmostes")); err != nil {
			t.Fatal(err)
		}
		if err := DeleteOwnPod(context.Background(), cl, "harmostes"); err != nil {
			t.Fatalf("HOSTNAME fallback should drive the delete: %v", err)
		}
	})
	t.Run("no identity — ErrNoPodIdentity", func(t *testing.T) {
		t.Setenv("POD_NAME", "")
		t.Setenv("HOSTNAME", "")
		cl := fake.NewClientBuilder().WithScheme(Scheme()).Build()
		err := DeleteOwnPod(context.Background(), cl, "harmostes")
		if !errors.Is(err, ErrNoPodIdentity) {
			t.Fatalf("identity-less process must refuse to guess, got: %v", err)
		}
	})
	t.Run("already gone — not an error", func(t *testing.T) {
		t.Setenv("POD_NAME", "vanished-pod")
		t.Setenv("HOSTNAME", "")
		cl := fake.NewClientBuilder().WithScheme(Scheme()).Build()
		if err := DeleteOwnPod(context.Background(), cl, "harmostes"); err != nil {
			t.Fatalf("a replaced pod must not fail the self-heal: %v", err)
		}
	})
}
