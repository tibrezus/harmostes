package attempt

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// ---- workflow builders ------------------------------------------------------

func wikiWorkflow() *v1alpha1.Workflow {
	return &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "harmostes", Namespace: "harmostes", Labels: map[string]string{v1alpha1.OwnerLabel: "tibrez"}},
		Spec: v1alpha1.WorkflowSpec{
			Source: v1alpha1.SourceSpec{Repo: "tibrezus/harmostes"},
			Agent:  v1alpha1.AgentSpec{Gate: v1alpha1.GateRef{Plugin: v1alpha1.PluginRef{Name: "wiki-lint"}}},
		},
	}
}

func prReviewWorkflow() *v1alpha1.Workflow {
	wf := wikiWorkflow()
	wf.Name = "pr-review-harmostes"
	wf.Spec.Agent.Gate.Plugin.Name = "review-validate"
	return wf
}

func forkWorkflow() *v1alpha1.Workflow {
	wf := wikiWorkflow()
	wf.Name = "signoz"
	wf.Spec.Source = v1alpha1.SourceSpec{
		Repo: "signoz-upstream",
		Fork: &v1alpha1.ForkSource{URL: "git@github.com:rezuscloud/signoz.git", Branch: "rezus/main"},
	}
	wf.Spec.Agent.Gate.Plugin.Name = "fork-resolved"
	return wf
}

// newFakeClient builds a controller-runtime fake client with the harmostes
// scheme and the Attempt status subresource enabled (so Status().Patch targets
// status only, mirroring the real API server).
func newFakeClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Attempt{}).
		Build()
}

func wfScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return s
}

func getAttempt(t *testing.T, ctx context.Context, c client.Client, a *v1alpha1.Attempt) v1alpha1.Attempt {
	t.Helper()
	var got v1alpha1.Attempt
	if err := c.Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: a.Name}, &got); err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	return got
}

func runPhase(runs []v1alpha1.RunRecord, name string) string {
	for _, r := range runs {
		if r.Name == name {
			return r.Phase
		}
	}
	return ""
}

// ===========================================================================
// DeriveKind
// ===========================================================================

func TestDeriveKind(t *testing.T) {
	cases := []struct {
		name string
		wf   *v1alpha1.Workflow
		want string
	}{
		{"wiki gate → documentation-sync", wikiWorkflow(), v1alpha1.ObjectiveKindDocumentationSync},
		{"review gate → pr-review", prReviewWorkflow(), v1alpha1.ObjectiveKindPRReview},
		{"fork gate → fork-sync", forkWorkflow(), v1alpha1.ObjectiveKindForkSync},
		{"fork source forces fork-sync even with wiki gate", func() *v1alpha1.Workflow {
			wf := wikiWorkflow()
			wf.Spec.Source.Fork = &v1alpha1.ForkSource{URL: "git@x:y/z.git"}
			return wf
		}(), v1alpha1.ObjectiveKindForkSync},
		{"unknown gate → documentation-sync default", func() *v1alpha1.Workflow {
			wf := wikiWorkflow()
			wf.Spec.Agent.Gate.Plugin.Name = "mystery-gate"
			return wf
		}(), v1alpha1.ObjectiveKindDocumentationSync},
	}
	for _, c := range cases {
		if got := DeriveKind(c.wf); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// ===========================================================================
// DeriveTargetedState
// ===========================================================================

func TestDeriveTargetedState(t *testing.T) {
	wf := wikiWorkflow()
	if got := DeriveTargetedState(wf, TriggerContext{Revision: "abc123", Source: "webhook"}); got != "abc123" {
		t.Errorf("webhook: got %q, want abc123", got)
	}
	wf.Spec.Source.Revision = "pinned-rev"
	if got := DeriveTargetedState(wf, TriggerContext{Source: "schedule"}); got != "pinned-rev" {
		t.Errorf("pinned: got %q, want pinned-rev", got)
	}
	if got := DeriveTargetedState(wf, TriggerContext{Revision: "abc123", Source: "webhook"}); got != "abc123" {
		t.Errorf("webhook-over-pin: got %q, want abc123", got)
	}
	wf.Spec.Source.Revision = ""
	if got := DeriveTargetedState(wf, TriggerContext{Source: "schedule"}); got != "head" {
		t.Errorf("none: got %q, want head", got)
	}
}

// ===========================================================================
// DeriveObjective — subjects + targeted state together
// ===========================================================================

func TestDeriveObjective_DocumentationSync(t *testing.T) {
	obj := DeriveObjective(wikiWorkflow(), TriggerContext{Revision: "deadbeef", Source: "webhook"})
	if obj.Kind != v1alpha1.ObjectiveKindDocumentationSync {
		t.Errorf("kind = %q", obj.Kind)
	}
	if obj.PrimarySubject.Binding != "source" || obj.PrimarySubject.Object != "tibrezus/harmostes" {
		t.Errorf("primary subject = %+v", obj.PrimarySubject)
	}
	if obj.TargetedState != "deadbeef" {
		t.Errorf("targeted = %q", obj.TargetedState)
	}
	if len(obj.RelatedSubjects) != 0 || obj.DesiredOutcome == "" {
		t.Errorf("documentation-sync: related=%d outcome=%q", len(obj.RelatedSubjects), obj.DesiredOutcome)
	}
}

func TestDeriveObjective_ForkSync(t *testing.T) {
	obj := DeriveObjective(forkWorkflow(), TriggerContext{Source: "schedule"})
	if obj.Kind != v1alpha1.ObjectiveKindForkSync {
		t.Errorf("kind = %q", obj.Kind)
	}
	// Primary subject is the FORK; upstream is a related subject.
	if obj.PrimarySubject.Binding != "fork" || obj.PrimarySubject.Object != "rezuscloud/signoz" {
		t.Errorf("fork primary subject = %+v, want rezuscloud/signoz", obj.PrimarySubject)
	}
	if len(obj.RelatedSubjects) != 1 || obj.RelatedSubjects[0].Object != "signoz-upstream" {
		t.Errorf("related subjects = %+v, want signoz-upstream", obj.RelatedSubjects)
	}
	if obj.TargetedState != "head" {
		t.Errorf("schedule fork targeted = %q, want head", obj.TargetedState)
	}
}

func TestRepoID(t *testing.T) {
	cases := map[string]string{
		"git@github.com:rezuscloud/signoz.git": "rezuscloud/signoz",
		"https://github.com/x/y":               "x/y",
		"https://github.com/x/y.git":           "x/y",
		"signoz-upstream":                      "signoz-upstream",
		"":                                     "",
	}
	for in, want := range cases {
		if got := RepoID(in); got != want {
			t.Errorf("RepoID(%q) = %q, want %q", in, got, want)
		}
	}
}

// ===========================================================================
// Identity + AttemptName — determinism + DNS-safety
// ===========================================================================

func TestIdentity_Deterministic(t *testing.T) {
	wf := wikiWorkflow()
	o1 := DeriveObjective(wf, TriggerContext{Revision: "abc"})
	o2 := DeriveObjective(wf, TriggerContext{Revision: "abc"})
	if Identity(o1) != Identity(o2) {
		t.Error("same input should yield same identity")
	}
	o3 := DeriveObjective(wf, TriggerContext{Revision: "def"})
	if Identity(o1) == Identity(o3) {
		t.Error("different targeted state should yield different identity")
	}
}

func TestAttemptName_DNSSafeAndDeterministic(t *testing.T) {
	wf := wikiWorkflow()
	obj := DeriveObjective(wf, TriggerContext{Revision: "abc"})
	name := AttemptName(wf.Name, Identity(obj))

	if len(name) > 63 {
		t.Errorf("name too long (%d): %s", len(name), name)
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
			t.Errorf("name %q contains non-DNS char %q", name, r)
		}
	}
	if !strings.HasPrefix(name, "attempt-harmostes-") {
		t.Errorf("name should start with attempt-harmostes-: %s", name)
	}
	if got := AttemptName(wf.Name, Identity(obj)); got != name {
		t.Errorf("AttemptName not deterministic: %q vs %q", got, name)
	}
	obj2 := DeriveObjective(wf, TriggerContext{Revision: "def"})
	if AttemptName(wf.Name, Identity(obj2)) == name {
		t.Error("different identity should yield different name")
	}
}

// ===========================================================================
// ResolveOrCreate — new vs continue (dedup/merge)
// ===========================================================================

func TestResolveOrCreate_NewThenContinue(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	wf := wikiWorkflow()
	obj := DeriveObjective(wf, TriggerContext{Revision: "abc", Source: "webhook"})
	opts := ResolveOptions{Namespace: "harmostes", WorkflowRef: "harmostes/harmostes", Owner: wf, Scheme: wfScheme(t)}

	a1, created, err := ResolveOrCreate(ctx, c, obj, opts)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if !created {
		t.Error("first resolve should create")
	}
	if a1.Status.Phase != v1alpha1.AttemptPhaseReconciling {
		t.Errorf("new attempt phase = %q, want reconciling", a1.Status.Phase)
	}
	// A second trigger for the SAME objective continues the same Attempt.
	a2, created2, err := ResolveOrCreate(ctx, c, obj, opts)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if created2 {
		t.Error("second resolve should CONTINUE (not create)")
	}
	if a2.Name != a1.Name {
		t.Errorf("continue should return same attempt: %q vs %q", a2.Name, a1.Name)
	}
	// A different targeted state → a DISTINCT attempt.
	obj2 := DeriveObjective(wf, TriggerContext{Revision: "def", Source: "webhook"})
	a3, created3, _ := ResolveOrCreate(ctx, c, obj2, opts)
	if !created3 {
		t.Error("different objective should create a new attempt")
	}
	if a3.Name == a1.Name {
		t.Error("distinct objective must have a distinct name")
	}
}

// ===========================================================================
// RecordRunStarted + RecordRunOutcome
// ===========================================================================

func TestRecordRunStarted_UpsertsRun(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	wf := wikiWorkflow()
	obj := DeriveObjective(wf, TriggerContext{Revision: "abc"})
	opts := ResolveOptions{Namespace: "harmostes", Owner: wf, Scheme: wfScheme(t)}
	a, _, _ := ResolveOrCreate(ctx, c, obj, opts)

	if err := RecordRunStarted(ctx, c, "harmostes", a.Name, "harmostes-harmostes-abc"); err != nil {
		t.Fatalf("record started: %v", err)
	}
	got := getAttempt(t, ctx, c, a)
	if len(got.Status.Runs) != 1 || got.Status.Runs[0].Phase != "running" {
		t.Fatalf("expected one running run, got %+v", got.Status.Runs)
	}
	// Idempotent re-schedule of the same run updates, not duplicates.
	_ = RecordRunStarted(ctx, c, "harmostes", a.Name, "harmostes-harmostes-abc")
	got = getAttempt(t, ctx, c, a)
	if len(got.Status.Runs) != 1 {
		t.Errorf("re-schedule should upsert, got %d runs", len(got.Status.Runs))
	}
}

func TestRecordRunOutcome_PhaseMapping(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	wf := wikiWorkflow()
	obj := DeriveObjective(wf, TriggerContext{Revision: "abc"})
	opts := ResolveOptions{Namespace: "harmostes", Owner: wf, Scheme: wfScheme(t)}
	a, _, _ := ResolveOrCreate(ctx, c, obj, opts)
	_ = RecordRunStarted(ctx, c, "harmostes", a.Name, "run-1")

	// Failed run → attempt phase failed.
	_ = RecordRunOutcome(ctx, c, "harmostes", a.Name, RunOutcome{RunName: "run-1", Phase: "failed", Message: "gate red"})
	got := getAttempt(t, ctx, c, a)
	if got.Status.Phase != v1alpha1.AttemptPhaseFailed {
		t.Errorf("failed run: phase = %q, want failed", got.Status.Phase)
	}
	if runPhase(got.Status.Runs, "run-1") != "failed" {
		t.Errorf("failed run record phase wrong: %+v", got.Status.Runs)
	}

	// A later succeeded run → attempt recovers to reconciling (validation is slice 4).
	_ = RecordRunOutcome(ctx, c, "harmostes", a.Name, RunOutcome{RunName: "run-2", Phase: "succeeded"})
	got = getAttempt(t, ctx, c, a)
	if got.Status.Phase != v1alpha1.AttemptPhaseReconciling {
		t.Errorf("succeeded run: phase = %q, want reconciling", got.Status.Phase)
	}
}

func TestRecordRunOutcome_AppendsEnvelopesAndEvidence(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	wf := wikiWorkflow()
	obj := DeriveObjective(wf, TriggerContext{Revision: "abc"})
	opts := ResolveOptions{Namespace: "harmostes", Owner: wf, Scheme: wfScheme(t)}
	a, _, _ := ResolveOrCreate(ctx, c, obj, opts)

	env := v1alpha1.NodeResultEnvelope{
		NodeID: "deploy", Status: v1alpha1.NodeResultStatusOK,
		References: []v1alpha1.EvidenceReference{{Binding: "repo", Kind: "commit", Identifier: "sha1"}},
	}
	_ = RecordRunOutcome(ctx, c, "harmostes", a.Name, RunOutcome{RunName: "run-1", Phase: "succeeded", Envelopes: []v1alpha1.NodeResultEnvelope{env}})

	got := getAttempt(t, ctx, c, a)
	if len(got.Status.NodeResults) != 1 {
		t.Errorf("expected 1 envelope, got %d", len(got.Status.NodeResults))
	}
	if len(got.Status.Evidence) != 1 || got.Status.Evidence[0].Identifier != "sha1" {
		t.Errorf("evidence not appended: %+v", got.Status.Evidence)
	}
	// Re-recording the same evidence should not duplicate.
	_ = RecordRunOutcome(ctx, c, "harmostes", a.Name, RunOutcome{RunName: "run-2", Phase: "succeeded", Envelopes: []v1alpha1.NodeResultEnvelope{env}})
	got = getAttempt(t, ctx, c, a)
	if len(got.Status.Evidence) != 1 {
		t.Errorf("evidence should be deduped, got %d", len(got.Status.Evidence))
	}
}
