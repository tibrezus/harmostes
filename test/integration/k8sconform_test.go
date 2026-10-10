//go:build integration

// #669: the envtest leg of the fake-vs-real conformance suite — the
// SAME table (internal/k8s/k8sconform) against a REAL API server. A
// wrapper that works only on the fake client fails HERE, never silently
// in the unit tier. Runs under `make test-integration` and CI's
// integration tier.
package integration

import (
	"testing"

	"github.com/tibrezus/harmostes/internal/k8s/k8sconform"
)

func TestConformanceEnvtest(t *testing.T) {
	cl := startEnv(t)
	k8sconform.Run(t, cl, k8sconform.Envtest)
}
