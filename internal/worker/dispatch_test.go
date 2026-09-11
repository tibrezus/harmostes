package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tibrezus/harmostes/internal/attempt"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

func dispatchScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := batchv1.AddToScheme(s); err != nil {
		t.Fatalf("batchv1 scheme: %v", err)
	}
	return s
}

func newTestDispatcher(t *testing.T, objects ...runtime.Object) (*Dispatcher, context.Context) {
	t.Helper()
	scheme := dispatchScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Attempt{}, &v1alpha1.Workflow{}).
		WithRuntimeObjects(objects...).Build()
	d := &Dispatcher{
		cl:        cl,
		scheme:    scheme,
		namespace: "default",
		logf:      t.Logf,
		cfg: DispatchConfig{
			FleetMaxConcurrent: 3,
			JobImage:           "harmostes-worker:test",
		},
	}
	return d, context.Background()
}

// gatedDispatchWorkflow: the gate fixture. The wake does NOT ride the CR —
// the controller clears the trigger annotations at schedule time and the
// env vars land on dispatched Job pods, so in the worker-pool topology the
// event reaches the gate only through GateDeps.Wake* (#349); the fixture's
// annotations are gone on purpose. The request pointer must stay in the
// workflow's configured scope — an out-of-scope wake arms nothing.
func gatedDispatchWorkflow() *v1alpha1.Workflow {
	wf := gateWorkflow()
	wf.Name = "pr-review-harmostes"
	wf.Namespace = "default"
	return wf
}

func dispatchRequest() RunRequest {
	return RunRequest{
		Workflow: "pr-review-harmostes", Namespace: "default",
		Pr: "git.rezus.cloud/tibrez/rhesadox#99", Action: "labeled",
		Revision: "deadbeef123", PrTitle: "t",
	}
}

// #272: proceed → Attempt resolved + Job created with the claim env — the
// graph runs in the Job pod, never in the dispatcher.
func TestDispatchProceedsToJobAndAttempt(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gatedDispatchWorkflow()
	d, ctx := newTestDispatcher(t, wf)

	if err := d.Dispatch(ctx, dispatchRequest()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	var jobs batchv1.JobList
	if err := d.cl.List(ctx, &jobs); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("exactly one Job must be dispatched, got %d", len(jobs.Items))
	}
	job := jobs.Items[0]
	if job.Labels["harmostes.dev/workflow"] != "pr-review-harmostes" {
		t.Fatalf("job must carry the workflow label: %+v", job.Labels)
	}
	env := map[string]string{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["HARMOSTES_DISPATCHED_ATTEMPT"] == "" {
		t.Fatalf("dispatched-claim marker missing: %v", env)
	}
	if env["HARMOSTES_TRIGGER_REPO"] != "git.rezus.cloud/tibrez/rhesadox" {
		t.Fatalf("gate envelope not bridged to job env: %v", env)
	}

	var attempts v1alpha1.AttemptList
	if err := d.cl.List(ctx, &attempts); err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	if len(attempts.Items) != 1 {
		t.Fatalf("exactly one Attempt must exist, got %d", len(attempts.Items))
	}
}

// #272/#274: at capacity the dispatcher ACKs without dispatching — capacity
// counts live DISPATCHED CLAIMS (Attempts); the armed marker is the durable
// queue and sweeps retry as slots free.
func TestDispatchCapacityRefusalStaysQueued(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gatedDispatchWorkflow()
	wf.Spec.ReviewReady.MaxConcurrent = 1
	now := time.Now()
	inFlight := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", now.Add(-time.Minute), &now)
	other := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#100", "otherhead00", now.Add(-time.Minute), &now)
	d, ctx := newTestDispatcher(t, wf, inFlight, other)

	if err := d.Dispatch(ctx, dispatchRequest()); err != nil {
		t.Fatalf("capacity refusal must ACK, got error: %v", err)
	}
	var jobs batchv1.JobList
	if err := d.cl.List(ctx, &jobs); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("no Job may be created at capacity, got %d", len(jobs.Items))
	}
}

// #272: a gate waiting decision dispatches nothing (label absent on the
// reviewed PR — the gate stays armed and the sweep retries).
func TestDispatchWaitingCreatesNothing(t *testing.T) {
	clearTriggerEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(req.URL.Path, "/pulls/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "deadbeef123"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{}, // label absent
			})
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gatedDispatchWorkflow()
	d, ctx := newTestDispatcher(t, wf)

	if err := d.Dispatch(ctx, dispatchRequest()); err != nil {
		t.Fatalf("waiting dispatch must ACK: %v", err)
	}
	var jobs batchv1.JobList
	if err := d.cl.List(ctx, &jobs); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("waiting must not dispatch, got %d jobs", len(jobs.Items))
	}
}

// #272: every class is Job-per-run — a workflow without reviewReady
// dispatches straight through (no gate).
func TestDispatchNonGatedWorkflowDispatches(t *testing.T) {
	clearTriggerEnv(t)
	wf := gatedDispatchWorkflow()
	wf.Spec.ReviewReady = nil
	d, ctx := newTestDispatcher(t, wf)

	req := dispatchRequest()
	req.Action = "" // schedule-shaped: no PR, no gate — plain run
	req.Pr = ""
	if err := d.Dispatch(ctx, req); err != nil {
		t.Fatalf("non-gated dispatch: %v", err)
	}
	var jobs batchv1.JobList
	if err := d.cl.List(ctx, &jobs); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("non-gated workflow must dispatch on trigger, got %d jobs", len(jobs.Items))
	}
}

// #272: two racing wakes for the same PR dedupe on the live attempt Job.
func TestDispatchDedupesLiveAttemptJob(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gatedDispatchWorkflow()
	d, ctx := newTestDispatcher(t, wf)

	if err := d.Dispatch(ctx, dispatchRequest()); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	if err := d.Dispatch(ctx, dispatchRequest()); err != nil {
		t.Fatalf("second dispatch (redelivery): %v", err)
	}
	var jobs batchv1.JobList
	if err := d.cl.List(ctx, &jobs); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("racing wakes must dedupe to one Job, got %d", len(jobs.Items))
	}
}

// #272: effective capacity — spec override wins, nil/0 takes the fleet
// default.
func TestEffectiveMaxConcurrent(t *testing.T) {
	if got := (*v1alpha1.ReviewReadySpec)(nil).EffectiveMaxConcurrent(3); got != 3 {
		t.Fatalf("nil spec must take fleet default, got %d", got)
	}
	if got := (&v1alpha1.ReviewReadySpec{}).EffectiveMaxConcurrent(3); got != 3 {
		t.Fatalf("zero override must take fleet default, got %d", got)
	}
	if got := (&v1alpha1.ReviewReadySpec{MaxConcurrent: 5}).EffectiveMaxConcurrent(3); got != 5 {
		t.Fatalf("spec override must win, got %d", got)
	}
}

// #287: the Job boundary must forward deployment-level credentials —
// pre-ADR runs inherited the pool env via buildChildEnv; without the
// forward the workspace plugin hits the private Forgejo anonymously (404)
// and the agent node lacks its LLM endpoint.
func TestJobCredentialEnv(t *testing.T) {
	t.Setenv("HARMOSTES_FORGEJO_TOKEN", "fj-token")
	t.Setenv("LITELLM_API_KEY", "llm-key")
	t.Setenv("DAPR_HTTP_PORT", "3500")
	t.Setenv("POD_NAME", "pool-pod")
	t.Setenv("HARMOSTES_VALKEY_PORT_6379_TCP_ADDR", "noise")

	got := jobCredentialEnv()
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "HARMOSTES_FORGEJO_TOKEN=fj-token") || !strings.Contains(joined, "LITELLM_API_KEY=llm-key") {
		t.Fatalf("credential env missing: %q", joined)
	}
	if strings.Contains(joined, "DAPR_") || strings.Contains(joined, "POD_NAME") || strings.Contains(joined, "VALKEY") {
		t.Fatalf("pod-scoped noise must not cross the Job boundary: %q", joined)
	}
}

// TestDispatchWakeSurvivesTheHop (#357 P2, r18 P8 rework): the
// RunRequest → GateDeps.Wake hop is three values in one struct; a forgotten
// member compiles clean and kills exactly one hop. The fixture is a
// MOVED-HEAD claim (armed at oldhead000, PR now green at deadbeef123) so
// the wake's Revision decides which attempt the Job runs: with Revision,
// the supersede arms at deadbeef123's derived name; with Revision lost,
// candSha collapses to "" and the arm derives a foreign objective — a
// different attempt name on the Job. Dropping Action kills the override
// outright (0 jobs). Both mutations verified red on this test.
func TestDispatchWakeSurvivesTheHop(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gatedDispatchWorkflow()
	d, ctx := newTestDispatcher(t, wf)

	req := dispatchRequest()
	// A LIVE claim at the OLD head: the moved-head labeled wake (request-
	// shaped) supersedes it and arms at the NEW head's identity.
	armed := time.Now().Add(-5 * time.Minute)
	claim := attemptAttemptFixture(t, ctx, d, wf, "git.rezus.cloud/tibrez/rhesadox#99", "oldhead000", armed)

	if err := d.Dispatch(ctx, req); err != nil {
		t.Fatalf("dispatch over a moved-head claim: %v", err)
	}
	var jobs batchv1.JobList
	if err := d.cl.List(ctx, &jobs); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("the threaded wake must supersede the moved-head claim and dispatch, got %d jobs", len(jobs.Items))
	}
	obj := attempt.DeriveObjective(wf, attempt.TriggerContext{Revision: "deadbeef123", Source: "webhook"})
	want := attempt.AttemptName(wf.Name, attempt.Identity(obj))
	if got := jobs.Items[0].Labels["harmostes.dev/attempt"]; got != want {
		t.Fatalf("the Job must run the deadbeef123 identity (Revision decided it), got %q want %q", got, want)
	}
	_ = claim
}

// attemptAttemptFixture arms a live claim through the real path (the
// dispatcher will supersede it).
func attemptAttemptFixture(t *testing.T, ctx context.Context, d *Dispatcher, wf *v1alpha1.Workflow, pr, sha string, armed time.Time) string {
	t.Helper()
	at, err := attempt.ArmClaim(ctx, d.cl, d.scheme, wf, pr, sha, "needs-review", false)
	if err != nil {
		t.Fatalf("arm %s: %v", sha, err)
	}
	return at.Name
}

// r#406-review blocking finding, folded per r2 P4 (table-driven so the
// next alias costs one row): every CLI-canonical alias the chart injects
// (worker-pool.yaml "CLI aliases" block) must be allowlisted AND forwarded
// by jobCredentialEnv — an allowlist entry that never reaches the Job is
// the same silent failure one indirection later, and a missing entry means
// the agent's CLI posts unauthenticated (inline threads die to prose).
func TestJobEnvAllowlistCarriesCLIAliases(t *testing.T) {
	for _, k := range jobEnvAllowlist {
		if strings.HasSuffix(k, "_API_BASE") {
			t.Errorf("%s must never be in jobEnvAllowlist: an API-base override in a Job env redirects a privileged token to an arbitrary origin (r3 P6 of #430)", k)
		}
	}
	rows := []struct{ name, why string }{
		{"FORGEJO_TOKEN", "the fj CLI's env fallback — without it the inline-thread protocol's Forgejo leg dies to prose"},
		{"GH_TOKEN", "gh's native env — without it the protocol's GitHub leg dies to prose"},
		{"LITELLM_FALLBACKS", "the extension's override knob is a pool-pod-only no-op without it (#359 r4 P4.1)"},
	}
	for _, row := range rows {
		found := false
		for _, k := range jobEnvAllowlist {
			if k == row.name {
				found = true
			}
		}
		if !found {
			t.Errorf("%s must be in jobEnvAllowlist: %s", row.name, row.why)
		}
	}
	// jobCredentialEnv must actually FORWARD the names from the process env
	// (the pool pod carries them) — an allowlist entry with a broken forward
	// is the same silent failure one indirection later.
	t.Setenv("FORGEJO_TOKEN", "probe-forge")
	t.Setenv("GH_TOKEN", "probe-gh")
	forwarded := map[string]bool{}
	for _, kv := range jobCredentialEnv() {
		for _, name := range []string{"FORGEJO_TOKEN", "GH_TOKEN"} {
			if kv == name+"=probe-"+map[string]string{"FORGEJO_TOKEN": "forge", "GH_TOKEN": "gh"}[name] {
				forwarded[name] = true
			}
		}
	}
	for _, name := range []string{"FORGEJO_TOKEN", "GH_TOKEN"} {
		if !forwarded[name] {
			t.Errorf("jobCredentialEnv does not forward %s from the process env", name)
		}
	}
	// Negative row (r2 P5): an EMPTY alias must be dropped — a CLI seeing
	// "set but unauthorized" stops resolving instead of falling through its
	// chain, which is worse than absent.
	t.Setenv("FORGEJO_TOKEN", "")
	forwardedEmpty := false
	for _, kv := range jobCredentialEnv() {
		if kv == "FORGEJO_TOKEN=" {
			forwardedEmpty = true
		}
	}
	if forwardedEmpty {
		t.Error("empty FORGEJO_TOKEN must not be forwarded — set-but-unauthorized blocks the CLI's resolution chain")
	}
}

// The seam the allowlist tests cannot see: jobCredentialEnv's output reaches
// the Job only through append(jobCredentialEnv(), dispatchEnv(...)) at the
// Dispatch call site. Deleting that append left every allowlist test green
// (r#406 review pillar 8) while the Job lost the credential — so pin the
// DELIVERED env through the real dispatch path, with the pool env the chart
// actually aliases.
func TestDispatchedJobCarriesCLIAliasTokens(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	t.Setenv("FORGEJO_TOKEN", "probe-forge")
	t.Setenv("GH_TOKEN", "probe-gh")
	wf := gatedDispatchWorkflow()

	d, ctx := newTestDispatcher(t, wf)
	if err := d.Dispatch(ctx, dispatchRequest()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	var jobs batchv1.JobList
	if err := d.cl.List(ctx, &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("exactly one Job must be dispatched, got %d", len(jobs.Items))
	}
	env := jobs.Items[0].Spec.Template.Spec.Containers[0].Env
	for alias, probe := range map[string]string{"FORGEJO_TOKEN": "probe-forge", "GH_TOKEN": "probe-gh"} {
		found := false
		var names []string
		for _, kv := range env {
			names = append(names, kv.Name+"="+kv.Value)
			if kv.Name == alias && kv.Value == probe {
				found = true
			}
		}
		if !found {
			t.Errorf("dispatched Job must carry %s=%s — env has: %v", alias, probe, names)
		}
	}
}

// r33 blocking finding (judge-required test): the per-run wall clock must
// read the MERGED spec. A templateRef workflow whose TEMPLATE sets
// runBound: 45m (the ops-prescribed shape — CRs put gate config on the
// template; ApplyTemplateDefaults merges in memory and never materializes)
// must dispatch a Job with ActiveDeadlineSeconds == 2700. A raw second Get
// of the workflow CR — the bug this pins — would see the unmerged spec,
// keep the 30m wall, and disagree with the gate's margin validation.
func TestDispatchRunBoundFlowsFromTemplateDefaults(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)

	// The template carries the gate spec AND the raised wall.
	tmpl := &v1alpha1.WorkflowTemplate{ObjectMeta: metav1.ObjectMeta{Name: "pr-review-tmpl", Namespace: "default"}}
	tmpl.Spec.ReviewReady = &v1alpha1.ReviewReadySpec{
		Label: "needs-review", Horizon: "6h",
		RunBound: "45m", // the raised wall — TEMPLATE-side only
	}
	wf := gatedDispatchWorkflow()
	wf.Spec.ReviewReady = nil // the instance: everything flows from the template
	wf.Spec.TemplateRef = "pr-review-tmpl"

	d, ctx := newTestDispatcher(t, wf, tmpl)
	if err := d.Dispatch(ctx, dispatchRequest()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	var jobs batchv1.JobList
	if err := d.cl.List(ctx, &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("exactly one Job must be dispatched, got %d", len(jobs.Items))
	}
	if got := *jobs.Items[0].Spec.ActiveDeadlineSeconds; got != 2700 {
		t.Fatalf("template-set runBound must reach the Job wall: ActiveDeadlineSeconds = %d, want 2700 (45m)", got)
	}
}

// #336 (r33 lesson applied): template-set cache reaches the dispatched Job
// through the SAME merged-spec seam as runBound — a raw second Get would see
// the unmerged spec and silently mount nothing.
func TestDispatchCacheFlowsFromTemplateDefaults(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)

	tmpl := &v1alpha1.WorkflowTemplate{ObjectMeta: metav1.ObjectMeta{Name: "pr-review-tmpl", Namespace: "default"}}
	tmpl.Spec.ReviewReady = &v1alpha1.ReviewReadySpec{Label: "needs-review", Horizon: "6h"}
	tmpl.Spec.Cache = &v1alpha1.CacheSpec{PVC: "harmostes-worker-cache", Go: true} // TEMPLATE-side only

	wf := gatedDispatchWorkflow()
	wf.Spec.ReviewReady = nil
	wf.Spec.TemplateRef = "pr-review-tmpl"

	d, ctx := newTestDispatcher(t, wf, tmpl)
	if err := d.Dispatch(ctx, dispatchRequest()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	var jobs batchv1.JobList
	if err := d.cl.List(ctx, &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("exactly one Job must be dispatched, got %d", len(jobs.Items))
	}
	job := jobs.Items[0]
	var mount *corev1.VolumeMount
	for i := range job.Spec.Template.Spec.Containers[0].VolumeMounts {
		if job.Spec.Template.Spec.Containers[0].VolumeMounts[i].Name == "cache" {
			mount = &job.Spec.Template.Spec.Containers[0].VolumeMounts[i]
		}
	}
	if mount == nil {
		t.Fatal("template-set cache must reach the Job volume")
	}
	if mount.SubPath != "pr-review-harmostes" {
		t.Fatalf("cache SubPath = %q, want the workflow name", mount.SubPath)
	}
	env := map[string]string{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["GOCACHE"] != "/cache/go/build" {
		t.Fatalf("GOCACHE must flow from the template cache flags: %v", env)
	}
	if env["HARMOSTES_WALL_SECONDS"] == "" {
		t.Fatal("HARMOSTES_WALL_SECONDS must be visible to the run")
	}
}
