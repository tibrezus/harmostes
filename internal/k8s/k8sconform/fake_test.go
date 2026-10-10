package k8sconform

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/k8s"
)

// The unit-tier leg: the whole conformance table against the fake
// client, on every `go test`. Known divergences assert their documented
// fake-side semantics (see CreateTimeStatusIsDropped).
func TestConformanceFake(t *testing.T) {
	cl := fake.NewClientBuilder().
		WithScheme(k8s.Scheme()).
		WithStatusSubresource(&v1alpha1.Attempt{}, &v1alpha1.Workflow{}).
		Build()
	Run(t, cl, Fake)
}
