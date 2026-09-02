//go:build integration

// The integration tier: the attempt ledger and review-claim lifecycles
// against a REAL API server (envtest — controller-runtime's bundled
// kube-apiserver + etcd) with the shipped chart CRDs applied.
//
// Why this tier exists (#315): fake clients cannot see CRD validation,
// status-subresource semantics, or — the load-bearing one — CRD schema
// pruning. A stale CRD silently zeroes unlisted status fields in production
// (#277, #289) while every fake-based test stays green. These tests assert
// field survival across a real API-server round-trip: if the chart CRDs lag
// the Go types, the compaction counters and claim fields read back empty and
// the tier fails.
//
// Run: make test-integration (needs envtest binaries — see the Makefile).
package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	envtest "sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/attempt"
	"github.com/tibrezus/harmostes/internal/k8s"
)

// startEnv boots envtest with the chart CRDs and returns a controller-runtime
// client speaking to the real API server. Fails loudly (with the setup hint)
// when the envtest binaries are absent — a skipped integration tier looks
// like coverage.
func startEnv(t *testing.T) client.Client {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Fatal("KUBEBUILDER_ASSETS not set — install envtest binaries: " +
			"go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.19 use 1.31.0 --bin-dir /tmp/envtest-bins && " +
			"export KUBEBUILDER_ASSETS=/tmp/envtest-bins/k8s/1.31.0-linux-amd64")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "chart", "crds")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: assets,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	c, err := client.New(cfg, client.Options{Scheme: k8s.Scheme()})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return c
}

// fixtureWorkflow creates the driving Workflow CR the attempt ledger needs
// (owner reference target + trigger context).
func fixtureWorkflow(t *testing.T, c client.Client, name string) *v1alpha1.Workflow {
	t.Helper()
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{v1alpha1.OwnerLabel: "tibrez"}},
		Spec: v1alpha1.WorkflowSpec{
			Source: v1alpha1.SourceSpec{Repo: "tibrezus/harmostes", Kind: "schedule"},
			Agent:  v1alpha1.AgentSpec{Gate: v1alpha1.GateRef{Plugin: v1alpha1.PluginRef{Name: "pr-review"}}},
		},
	}
	if err := c.Create(context.Background(), wf); err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	return wf
}

// TestAttemptLedgerSurvivesAPIServerRoundTrip is the pruning canary: the
// compaction counters and clamped message must come back from the REAL API
// server byte-for-byte — a stale chart CRD prunes them to zero and this test
// fails (the #277/#289 failure mode, caught before production).
func TestAttemptLedgerSurvivesAPIServerRoundTrip(t *testing.T) {
	c := startEnv(t)
	ctx := context.Background()

	wf := fixtureWorkflow(t, c, "ledger-roundtrip")
	obj := attempt.DeriveObjective(wf, attempt.TriggerContext{Revision: "abc1234", Source: "webhook"})
	a, created, err := attempt.ResolveOrCreate(ctx, c, obj, attempt.ResolveOptions{
		Namespace: "default", WorkflowRef: "default/ledger-roundtrip", Owner: wf, Scheme: k8s.Scheme(),
	})
	if err != nil || !created {
		t.Fatalf("ResolveOrCreate: created=%v err=%v", created, err)
	}

	if err := attempt.RecordRunStarted(ctx, c, "default", a.Name, "run-1"); err != nil {
		t.Fatalf("RecordRunStarted: %v", err)
	}

	// 250 envelopes force head-compaction inside this very write: the
	// counters and the tail window land through the real patch path.
	base := time.Now()
	outs := make([]v1alpha1.NodeResultEnvelope, 0, attempt.MaxStatusNodeResults+50)
	for i := 0; i < attempt.MaxStatusNodeResults+50; i++ {
		outs = append(outs, v1alpha1.NodeResultEnvelope{
			NodeID:     "prepare",
			RunID:      fmt.Sprintf("run-%d", i),
			Status:     "ok",
			Summary:    fmt.Sprintf("step %d", i),
			ProducedAt: metav1.NewTime(base.Add(time.Duration(i) * time.Minute)),
			Claims: []v1alpha1.Claim{{
				Type: "repository.commit.created", Binding: "github", ExternalID: fmt.Sprintf("sha-%d", i),
				// The CRD enforces the enum — production envelopes are
				// trust-classed by the executor; observed is the honest
				// baseline here.
				TrustClass: "observed", // CRD enum: observed | validated
			}},
		})
	}
	oversize := strings.Repeat("m", attempt.MaxStatusMessageBytes+500)
	if err := attempt.RecordRunOutcome(ctx, c, "default", a.Name, attempt.RunOutcome{
		RunName: "run-1", Phase: "succeeded", Envelopes: outs, Message: oversize,
	}); err != nil {
		t.Fatalf("RecordRunOutcome: %v", err)
	}

	// Read back through the API server — NOT from any local object.
	var got v1alpha1.Attempt
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: a.Name}, &got); err != nil {
		t.Fatalf("get attempt: %v", err)
	}

	if len(got.Status.NodeResults) != attempt.MaxStatusNodeResults {
		t.Errorf("tail = %d envelopes, want %d", len(got.Status.NodeResults), attempt.MaxStatusNodeResults)
	}
	// THE pruning canary: this field exists only if the chart CRD schema is
	// current. A stale CRD prunes it to 0 on every status write.
	if got.Status.CompactedNodeResults != 50 {
		t.Errorf("compactedNodeResults = %d, want 50 — 0 means the API server PRUNED the field (stale chart CRD, the #277/#289 class)", got.Status.CompactedNodeResults)
	}
	if got.Status.TotalNodeResults() != attempt.MaxStatusNodeResults+50 {
		t.Errorf("totals lost history after round-trip: %d", got.Status.TotalNodeResults())
	}
	if len(got.Status.Message) != attempt.MaxStatusMessageBytes {
		t.Errorf("message = %d bytes, want clamped to %d", len(got.Status.Message), attempt.MaxStatusMessageBytes)
	}
	if len(got.Status.Runs) != 1 || got.Status.Runs[0].Phase != "succeeded" {
		t.Errorf("run record wrong after round-trip: %+v", got.Status.Runs)
	}
	// Claims survive with their structure (ADR-0004's decision surface).
	for _, env := range got.Status.NodeResults {
		if len(env.Claims) != 1 || env.Claims[0].ExternalID == "" {
			t.Fatalf("claims did not survive the round-trip: %+v", env.Claims)
		}
		break
	}
}

// TestReviewClaimLifecycleRoundTrip pins the gate's claim fields through the
// real status-subresource patch path: arm → dispatchedAt, release → released
// + reason, all surviving the API server.
func TestReviewClaimLifecycleRoundTrip(t *testing.T) {
	c := startEnv(t)
	ctx := context.Background()

	wf := fixtureWorkflow(t, c, "claim-roundtrip")
	obj := attempt.DeriveObjective(wf, attempt.TriggerContext{Revision: "def5678", Source: "webhook"})
	a, _, err := attempt.ResolveOrCreate(ctx, c, obj, attempt.ResolveOptions{
		Namespace: "default", WorkflowRef: "default/claim-roundtrip", Owner: wf, Scheme: k8s.Scheme(),
	})
	if err != nil {
		t.Fatalf("ResolveOrCreate: %v", err)
	}

	if err := attempt.MarkClaimDispatched(ctx, c, "default", a.Name); err != nil {
		t.Fatalf("MarkClaimDispatched: %v", err)
	}
	var dispatched v1alpha1.Attempt
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: a.Name}, &dispatched); err != nil {
		t.Fatalf("get: %v", err)
	}
	rv := dispatched.Status.Review
	// MarkClaimDispatched stamps the dispatch marker; ArmedSince is the arm
	// path's field (set when the gate armed the claim, before dispatch).
	if rv == nil || rv.DispatchedAt.IsZero() {
		t.Fatalf("dispatch marker pruned or unset: %+v", rv)
	}

	if err := attempt.ReleaseClaim(ctx, c, "default", a.Name, "consumed"); err != nil {
		t.Fatalf("ReleaseClaim: %v", err)
	}
	var released v1alpha1.Attempt
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: a.Name}, &released); err != nil {
		t.Fatalf("get: %v", err)
	}
	if released.Status.Review.Released != true || released.Status.Review.ReleaseReason != "consumed" {
		t.Fatalf("release state did not survive the round-trip: %+v", released.Status.Review)
	}
}
