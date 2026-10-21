package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tibrezus/harmostes/internal/attempt"
	"github.com/tibrezus/harmostes/internal/k8s"
	"github.com/tibrezus/harmostes/internal/review"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
			NewReviewAPI: func() review.API {
				if testReviewAPI != nil {
					return testReviewAPI
				}
				return nil
			},
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

	got := jobCredentialEnv(nil)
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
		{"HARMOSTES_FORGEJO_BOT_TOKEN", "the whitelisted bot review identity (#480) — without it native APPROVED/REQUEST_CHANGES reviews 422 as self-reviews on author-owned PRs"},
		{"HARMOSTES_FORGEJO_BOT_HOST", "the other half of the #480 gate pair (#480 r3 t5) — token without host means the Job-side script compares against a compiled-in default and a non-default botHost silently never matches"},
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
	for _, kv := range jobCredentialEnv(nil) {
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
	for _, kv := range jobCredentialEnv(nil) {
		if kv == "FORGEJO_TOKEN=" {
			forwardedEmpty = true
		}
	}
	if forwardedEmpty {
		t.Error("empty FORGEJO_TOKEN must not be forwarded — set-but-unauthorized blocks the CLI's resolution chain")
	}
}

// The seam the allowlist tests cannot see: jobCredentialEnv's output reaches
// the Job only through append(jobCredentialEnv(nil), dispatchEnv(...)) at the
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

// #666 intent-before-side-effect: when Job creation FAILS (here: forced),
// the dispatch INTENT (DispatchedAt) must already be on the attempt —
// record-then-act, never act-without-record. The ownership EDGE stays
// empty: GenerateName assigns the Job's name at Create, so no artifact
// name ever existed — "intent without artifact" is the honest state.
// Under the pre-#666 order (Create-then-record) this test fails: a
// failed Create left NO record at all.
func TestDispatchIntentPrecedesJobCreation(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gatedDispatchWorkflow()

	scheme := dispatchScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Attempt{}, &v1alpha1.Workflow{}).
		WithRuntimeObjects(wf).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, isJob := obj.(*batchv1.Job); isJob {
					return apierrors.NewInternalError(fmt.Errorf("forced spawn failure"))
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
	d := &Dispatcher{
		cl:        cl,
		scheme:    scheme,
		namespace: "default",
		logf:      t.Logf,
		cfg: DispatchConfig{
			FleetMaxConcurrent: 3,
			JobImage:           "harmostes-worker:test",
			NewReviewAPI: func() review.API {
				if testReviewAPI != nil {
					return testReviewAPI
				}
				return nil
			},
		},
	}
	ctx := context.Background()
	if err := d.Dispatch(ctx, dispatchRequest()); err == nil {
		t.Fatal("forced Create failure must surface as a dispatch error")
	}
	var attempts v1alpha1.AttemptList
	if err := cl.List(ctx, &attempts); err != nil {
		t.Fatal(err)
	}
	var intent int
	for _, at := range attempts.Items {
		r := at.Status.Review
		if r == nil || r.DispatchedAt == nil {
			t.Fatalf("intent (DispatchedAt) must be recorded BEFORE the spawn attempt: %+v", r)
		}
		if r.DispatchedJob != "" {
			t.Fatalf("edge must stay empty when no artifact name ever existed (GenerateName assigns it at Create): %+v", r)
		}
		intent++
	}
	if intent != 1 {
		t.Fatalf("exactly one attempt must carry the intent record, got %d", intent)
	}
	var jobs batchv1.JobList
	if err := cl.List(ctx, &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("no Job may exist after forced spawn failure, got %d", len(jobs.Items))
	}
}

// TestBudgetPolicy (#680): AgentSpec.ToolBudget renders the per-workflow
// turn-budget decision — nil forwards the fleet trio untouched, 0 drops it
// (uncapped), N>0 overrides the cap. The reviewer declares 0: all pillars,
// all the time needed, bounded only by the run bound.
func TestBudgetPolicy(t *testing.T) {
	fleetEnv := []string{
		"PI_TOOL_BUDGET=32", "PI_TOOL_BUDGET_ALLOW=/workspace/review.json",
		"PI_TOOL_BUDGET_NUDGE=16", "LITELLM_URL=http://x",
	}
	t.Setenv("PI_TOOL_BUDGET", "32")
	t.Setenv("PI_TOOL_BUDGET_ALLOW", "/workspace/review.json")
	t.Setenv("PI_TOOL_BUDGET_NUDGE", "16")
	t.Setenv("LITELLM_URL", "http://x")

	mk := func(tb *int) []string {
		wf := &v1alpha1.Workflow{}
		wf.Spec.Agent.ToolBudget = tb
		return jobCredentialEnv(&budgetPolicy{workflow: wf})
	}

	// nil → fleet default: trio forwarded.
	for _, kv := range [3]string{"PI_TOOL_BUDGET=32", "PI_TOOL_BUDGET_ALLOW=/workspace/review.json", "PI_TOOL_BUDGET_NUDGE=16"} {
		if !slices.Contains(mk(nil), kv) {
			t.Fatalf("nil ToolBudget must forward the fleet trio: %v missing", kv)
		}
	}
	// 0 → uncapped: the trio is absent, everything else forwards.
	zero := mk(new(int)) // *0
	for _, kv := range fleetEnv[:3] {
		if slices.Contains(zero, kv) {
			t.Fatalf("ToolBudget=0 must drop %s (uncapped)", kv)
		}
	}
	if !slices.Contains(zero, "LITELLM_URL=http://x") {
		t.Fatal("ToolBudget=0 must not touch unrelated allowlisted env")
	}
	// N>0 → the cap is overridden and the nudge derives N/2 (#689: the pool
	// always forwards an explicit PI_TOOL_BUDGET_NUDGE, so leaving it alone
	// would pin the checkpoint at the fleet's 16 calls under a raised cap).
	five := 5
	over := mk(&five)
	if !slices.Contains(over, "PI_TOOL_BUDGET=5") {
		t.Fatal("ToolBudget=5 must override PI_TOOL_BUDGET=5")
	}
	if !slices.Contains(over, "PI_TOOL_BUDGET_NUDGE=2") {
		t.Fatal("ToolBudget=5 must derive the nudge (cap/2) — the pool's explicit 16 would otherwise stick")
	}
	// nil policy (tests calling without a workflow): fleet default intact.
	if !slices.Contains(jobCredentialEnv(nil), "PI_TOOL_BUDGET=32") {
		t.Fatal("nil policy must forward the fleet trio")
	}
}

// TestPostTokenForwardsOnlyToReviewAttempts (#680 r5 P1): the posting
// credential's PRESENCE in a session env is the arming signal — it must
// reach review attempts and review attempts ONLY. A fleet-wide forward
// would re-bind every workflow's CLI canonicals to a comment-scoped token
// and strip their real credentials.
func TestPostTokenForwardsOnlyToReviewAttempts(t *testing.T) {
	t.Setenv("HARMOSTES_REVIEW_POST_TOKEN", "scoped")
	mk := func(kind string) []string {
		at := &v1alpha1.Attempt{}
		at.Spec.Objective.Kind = kind
		return jobCredentialEnv(&budgetPolicy{attempt: at})
	}
	review := mk(v1alpha1.ObjectiveKindPRReview)
	if !slices.Contains(review, "HARMOSTES_REVIEW_POST_TOKEN=scoped") {
		t.Fatal("review attempts must receive the posting credential")
	}
	other := mk("fork-maintenance")
	if slices.Contains(other, "HARMOSTES_REVIEW_POST_TOKEN=scoped") {
		t.Fatal("non-review attempts must NOT receive the posting credential (arming is review-scoped)")
	}
	// The review-scoped extras are the post token AND the loop breaker
	// (#695 r3) — exactly two, nothing else.
	if len(review) != len(other)+2 {
		t.Fatalf("review env must carry exactly two review-scoped extras (post token + PI_LOOP_BREAKER): %d vs %d", len(review), len(other))
	}
}

// TestLoopBreakerArmsReviewAttemptsOnly (#695 r3): the conceptual budget
// layers assume a STATIC workspace — true for reviews, false for
// fork-maintenance (retrying an identical command after an upstream fix is
// legitimate there). PI_LOOP_BREAKER=1 reaches review attempts only.
func TestLoopBreakerArmsReviewAttemptsOnly(t *testing.T) {
	t.Setenv("PI_TOOL_BUDGET", "32")
	t.Setenv("PI_TOOL_BUDGET_ALLOW", "/workspace/review.json")
	t.Setenv("PI_TOOL_BUDGET_NUDGE", "16")
	mk := func(kind string) []string {
		at := &v1alpha1.Attempt{}
		at.Spec.Objective.Kind = kind
		return jobCredentialEnv(&budgetPolicy{attempt: at})
	}
	if !slices.Contains(mk(v1alpha1.ObjectiveKindPRReview), "PI_LOOP_BREAKER=1") {
		t.Fatal("review attempts must carry PI_LOOP_BREAKER=1 (conceptual layers)")
	}
	if slices.Contains(mk("fork-maintenance"), "PI_LOOP_BREAKER=1") {
		t.Fatal("fork-maintenance retries identical commands legitimately — no loop breaker there")
	}
}

// ── #704 review r1 on the inherit PR: the declaration became fleet-wide,
// so the consumer seam must honor it HONESTLY — malformed declarations
// fail dispatch (never a post-intent Create rejection), and the
// effective mounts log with drop reasons (the #311/#314 doctrine: a
// config fact must not drop silently). ──
func TestValidateAttachments(t *testing.T) {
	valid := []v1alpha1.SharedAttachment{{Name: "sol-pi", Scope: "repository", PVC: "harmostes-worker-sessions"}}
	if err := ValidateAttachments(valid); err != nil {
		t.Fatalf("valid declaration rejected: %v", err)
	}

	for _, tc := range []struct {
		name string
		att  v1alpha1.SharedAttachment
		want string
	}{
		{"bad name (volume label)", v1alpha1.SharedAttachment{Name: "Bad/Name", PVC: "c"}, "DNS-1123"},
		{"empty name", v1alpha1.SharedAttachment{PVC: "c"}, "DNS-1123"},
		// r2: the composed volume name is what the apiserver validates —
		// a 57-char name yields a 64-char "attach-<name>" and must be
		// rejected HERE, not at Create-after-intent.
		{"57-char name (volume name over 63)", v1alpha1.SharedAttachment{Name: strings.Repeat("a", 57), PVC: "c"}, "DNS-1123"},
		{"56-char name (volume name at 63) is legal", v1alpha1.SharedAttachment{Name: strings.Repeat("a", 56), PVC: "c"}, ""},
		{"missing pvc", v1alpha1.SharedAttachment{Name: "ok"}, "pvc is required"},
		{"relative mountPath", v1alpha1.SharedAttachment{Name: "ok", PVC: "c", MountPath: "attachments/x"}, "must be absolute"},
		{"root mountPath", v1alpha1.SharedAttachment{Name: "ok", PVC: "c", MountPath: "/"}, "container root"},
	} {
		err := ValidateAttachments([]v1alpha1.SharedAttachment{tc.att})
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected err %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want containing %q", tc.name, err, tc.want)
		}
	}

	// Default mountPath ("" → the builder derives /attachments/<name>) is legal.
	if err := ValidateAttachments([]v1alpha1.SharedAttachment{{Name: "ok", PVC: "c", MountPath: ""}}); err != nil {
		t.Errorf("empty mountPath must stay legal (builder default), got %v", err)
	}

	// r4: name is identity — a duplicate (the copy-paste typo) collides
	// on volume name, mount path, and store env, and would be an
	// apiserver Create rejection after the intent.
	dup := []v1alpha1.SharedAttachment{
		{Name: "sol-pi", Scope: "repository", PVC: "claim-a"},
		{Name: "sol-pi", Scope: "repository", PVC: "claim-b"},
	}
	if err := ValidateAttachments(dup); err == nil || !strings.Contains(err.Error(), "duplicate name") {
		t.Errorf("duplicate names must be rejected naming the collision, got %v", err)
	}

	// r5: pvc is a claimName — validated server-side as a label; the
	// validator is already imported (the r2 class one field over).
	for _, pvc := range []string{"harmostes worker-sessions", "Harmostes/Worker"} {
		err := ValidateAttachments([]v1alpha1.SharedAttachment{{Name: "ok", PVC: pvc}})
		if err == nil || !strings.Contains(err.Error(), "pvc") {
			t.Errorf("pvc %q must be rejected as a non-label, got %v", pvc, err)
		}
	}

	// r5: the EFFECTIVE path (builder default when unset, cleaned) is
	// unique across entries — explicit-vs-default and trailing-slash
	// shapes collide on one mount path in the PodSpec.
	for name, atts := range map[string][]v1alpha1.SharedAttachment{
		"default-vs-explicit": {
			{Name: "sol-pi", Scope: "repository", PVC: "c1"},
			{Name: "other", Scope: "workflow", PVC: "c2", MountPath: "/attachments/sol-pi"},
		},
		"trailing-slash": {
			{Name: "a", PVC: "c1", MountPath: "/data"},
			{Name: "b", PVC: "c2", MountPath: "/data/"},
		},
	} {
		err := ValidateAttachments(atts)
		if err == nil || !strings.Contains(err.Error(), "collides") {
			t.Errorf("%s: effective-path collision must be rejected, got %v", name, err)
		}
	}

	// r6: traversal shapes clean to the container root — the root rule
	// must be evaluated on the CLEANED path, or "/workspace/.." passes
	// the raw check and the reserved check and mounts the shared claim
	// over the whole filesystem.
	for _, p := range []string{"/workspace/..", "/data/../", "/x/y/../.."} {
		err := ValidateAttachments([]v1alpha1.SharedAttachment{{Name: "ok", PVC: "c", MountPath: p}})
		if err == nil || !strings.Contains(err.Error(), "resolves to the container root") {
			t.Errorf("mountPath %q must be rejected as root-resolving, got %v", p, err)
		}
	}

	// r5: reserved builder mounts — the shared claim must never shadow
	// them (data integrity: /workspace is the attempt's working dir).
	// r6: /plugins is the fifth fixed home (plugin ConfigMaps, when a
	// deployment sets pluginConfigMaps).
	for _, p := range append([]string{"/workspace", "/skills", "/cache", "/sessions", "/workspace/sub", "/plugins", "/plugins/harmostes-tasks"}, k8s.ReservedAttemptMountPaths()...) {
		err := ValidateAttachments([]v1alpha1.SharedAttachment{{Name: "ok", PVC: "c", MountPath: p}})
		if err == nil || !strings.Contains(err.Error(), "shadows") {
			t.Errorf("mountPath %q must be rejected as reserved, got %v", p, err)
		}
	}
}

// r2: the log reads the COMPILED JOB — the artifact, not the intent. A
// Job without the volume must log ABSENT, and a mounted sol-pi
// attachment must show the store env.
func TestLogEffectiveAttachments(t *testing.T) {
	mkJob := func(withMount bool) *batchv1.Job {
		j := &batchv1.Job{}
		// Production shape: BuildJob sets GenerateName, never Name — the
		// server assigns it at Create, after the log line. The r3 defect
		// (an empty key) was invisible because this fixture fabricated a
		// name.
		j.GenerateName = "attempt-x-"
		vols := []corev1.Volume{}
		mounts := []corev1.VolumeMount{}
		env := []corev1.EnvVar{{Name: "HARMOSTES_WALL_SECONDS", Value: "60"}}
		if withMount {
			vols = append(vols, corev1.Volume{Name: "attach-sol-pi",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "harmostes-worker-sessions"}}})
			mounts = append(mounts, corev1.VolumeMount{Name: "attach-sol-pi", MountPath: "/attachments/sol-pi", SubPath: "repo-hash"})
			env = append(env, corev1.EnvVar{Name: "HARMOSTES_SOL_PI_STORE", Value: "/attachments/sol-pi"})
		}
		j.Spec.Template.Spec = corev1.PodSpec{Volumes: vols, Containers: []corev1.Container{{Name: "run", VolumeMounts: mounts, Env: env}}}
		return j
	}
	atts := []v1alpha1.SharedAttachment{{Name: "sol-pi", Scope: "repository", PVC: "harmostes-worker-sessions"}}
	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }

	logEffectiveAttachments(logf, "attempt-x-abcde", mkJob(true), atts)
	if len(lines) != 1 || !strings.Contains(lines[0], "dispatch: job attempt-x-abcde") ||
		!strings.Contains(lines[0], "sol-pi@repository=/attachments/sol-pi sub=repo-hash") ||
		!strings.Contains(lines[0], "claim=harmostes-worker-sessions +store-env") {
		t.Fatalf("mounted artifact must be reported from the Job, keyed on the attempt, got: %v", lines)
	}

	lines = nil
	logEffectiveAttachments(logf, "attempt-x-abcde", mkJob(false), atts) // the mount never made the Job
	if len(lines) != 1 || !strings.Contains(lines[0], "sol-pi@repository=ABSENT") {
		t.Fatalf("an absent mount must be reported, never a resolved path, got: %v", lines)
	}

	// A Job fresh from BuildJob has NO name (GenerateName) — the line
	// must key on the attempt, never print an empty identity.
	lines = nil
	logEffectiveAttachments(logf, "attempt-x-abcde", mkJob(true), atts)
	if strings.Contains(lines[0], "dispatch: job  ") || !strings.Contains(lines[0], "attempt-x-abcde") {
		t.Fatalf("the key must be the attempt name pre-Create, got: %q", lines[0])
	}

	lines = nil
	logEffectiveAttachments(logf, "attempt-x-abcde", mkJob(true), nil)
	if len(lines) != 0 {
		t.Fatalf("no attachments → no log line, got: %v", lines)
	}
}

// The hop pin (#704, found live): the template's declaration must reach
// the COMPILED Job — asserted against the artifact. Two hops are pinned:
// the ApplyTemplateDefaults inherit and the JobParams struct-copy —
// deleting either kept the whole unit tier green before this test.
func TestDispatchAttachmentsFlowsFromTemplateDefaults(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)

	tmpl := &v1alpha1.WorkflowTemplate{ObjectMeta: metav1.ObjectMeta{Name: "pr-review-tmpl", Namespace: "default"}}
	tmpl.Spec.ReviewReady = &v1alpha1.ReviewReadySpec{Label: "needs-review", Horizon: "6h"}
	tmpl.Spec.Attachments = []v1alpha1.SharedAttachment{{Name: "sol-pi", Scope: "repository", PVC: "harmostes-worker-sessions"}} // TEMPLATE-side only

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
	spec := jobs.Items[0].Spec.Template.Spec

	var vol *corev1.Volume
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == "attach-sol-pi" {
			vol = &spec.Volumes[i]
		}
	}
	if vol == nil || vol.PersistentVolumeClaim == nil || vol.PersistentVolumeClaim.ClaimName != "harmostes-worker-sessions" {
		t.Fatalf("template attachment must reach the Job volume with its claim, got %+v", vol)
	}

	var mount *corev1.VolumeMount
	for i := range spec.Containers[0].VolumeMounts {
		if spec.Containers[0].VolumeMounts[i].Name == "attach-sol-pi" {
			mount = &spec.Containers[0].VolumeMounts[i]
		}
	}
	wantSub, _ := k8s.AttachmentSubPath(v1alpha1.AttachmentScopeRepository, "pr-review-harmostes", "git.rezus.cloud/tibrez/rhesadox", "x")
	if mount == nil || mount.SubPath != wantSub {
		t.Fatalf("attachment mount must carry the repository SubPath %q, got %+v", wantSub, mount)
	}

	env := map[string]string{}
	for _, e := range spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["HARMOSTES_SOL_PI_STORE"] != mount.MountPath {
		t.Fatalf("HARMOSTES_SOL_PI_STORE = %q, want the mount path %q", env["HARMOSTES_SOL_PI_STORE"], mount.MountPath)
	}
}

// r3: the CALL SITES are part of the contract — correct functions that
// Dispatch never calls pin nothing. (1) A malformed template
// declaration must fail Dispatch before any artifact or intent; (2) the
// effective-mounts line must flow through the dispatcher's own logf
// keyed on the attempt; (3) the non-gated (nil-envelope) path must
// report ABSENT for a repository-scoped declaration.
func TestDispatchAttachmentsCallSitesWired(t *testing.T) {
	clearTriggerEnv(t)

	// (1) Validator wired at the merged-spec seam: bad declaration →
	// dispatch-time error, zero Jobs, zero dispatch intents.
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	bad := &v1alpha1.WorkflowTemplate{ObjectMeta: metav1.ObjectMeta{Name: "pr-review-tmpl", Namespace: "default"}}
	bad.Spec.ReviewReady = &v1alpha1.ReviewReadySpec{Label: "needs-review", Horizon: "6h"}
	bad.Spec.Attachments = []v1alpha1.SharedAttachment{{Name: "Bad/Name", PVC: "harmostes-worker-sessions"}}
	badWf := gatedDispatchWorkflow()
	badWf.Spec.ReviewReady = nil
	badWf.Spec.TemplateRef = "pr-review-tmpl"
	d, ctx := newTestDispatcher(t, badWf, bad)
	err := d.Dispatch(ctx, dispatchRequest())
	if err == nil || !strings.Contains(err.Error(), "attachments invalid") {
		t.Fatalf("a malformed declaration must fail dispatch naming the seam, got %v", err)
	}
	var jobs batchv1.JobList
	if err := d.cl.List(ctx, &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("no Job may exist for an invalid declaration, got %d", len(jobs.Items))
	}
	var attempts v1alpha1.AttemptList
	if err := d.cl.List(ctx, &attempts); err != nil {
		t.Fatal(err)
	}
	for _, a := range attempts.Items {
		if a.Status.Review != nil && a.Status.Review.DispatchedAt != nil {
			t.Fatalf("no dispatch intent may be recorded for an invalid declaration (the #666 state), attempt %s has one", a.Name)
		}
	}

	// (2)+(3): the log line through the dispatcher's own logf.
	good := &v1alpha1.WorkflowTemplate{ObjectMeta: metav1.ObjectMeta{Name: "pr-review-tmpl", Namespace: "default"}}
	good.Spec.ReviewReady = &v1alpha1.ReviewReadySpec{Label: "needs-review", Horizon: "6h"}
	good.Spec.Attachments = []v1alpha1.SharedAttachment{{Name: "sol-pi", Scope: "repository", PVC: "harmostes-worker-sessions"}}
	goodWf := gatedDispatchWorkflow()
	goodWf.Spec.ReviewReady = nil
	goodWf.Spec.TemplateRef = "pr-review-tmpl"
	var lines []string
	d2, ctx2 := newTestDispatcher(t, goodWf, good)
	d2.logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	if err := d2.Dispatch(ctx2, dispatchRequest()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	found := false
	for _, l := range lines {
		if !strings.Contains(l, "attachments=[") {
			continue
		}
		found = true
		// Keyed on the attempt (never an empty job.Name), resolved mount,
		// claim, and the store env attributed to the mounting attachment.
		if !strings.Contains(l, "dispatch: job attempt-pr-review-harmostes-") ||
			strings.Contains(l, "dispatch: job  ") ||
			!strings.Contains(l, "sol-pi@repository=/attachments/sol-pi sub=") ||
			!strings.Contains(l, "claim=harmostes-worker-sessions +store-env") {
			t.Fatalf("the effective-mounts line must be attempt-keyed and resolved, got: %q", l)
		}
	}
	if !found {
		t.Fatalf("the dispatcher must log the effective mounts through its own logf, got: %v", lines)
	}

	// (3) Non-gated, nil envelope: the repository-scoped declaration
	// cannot resolve → the Job carries no mount and the line says ABSENT.
	ng := &v1alpha1.WorkflowTemplate{ObjectMeta: metav1.ObjectMeta{Name: "ng-tmpl", Namespace: "default"}}
	ng.Spec.Attachments = []v1alpha1.SharedAttachment{{Name: "sol-pi", Scope: "repository", PVC: "harmostes-worker-sessions"}}
	ngWf := gatedDispatchWorkflow()
	ngWf.Spec.ReviewReady = nil
	ngWf.Spec.TemplateRef = "ng-tmpl"
	var ngLines []string
	d3, ctx3 := newTestDispatcher(t, ngWf, ng)
	d3.logf = func(format string, args ...any) { ngLines = append(ngLines, fmt.Sprintf(format, args...)) }
	req := dispatchRequest()
	req.Action = ""
	req.Pr = ""
	if err := d3.Dispatch(ctx3, req); err != nil {
		t.Fatalf("non-gated dispatch: %v", err)
	}
	var ngJobs batchv1.JobList
	if err := d3.cl.List(ctx3, &ngJobs); err != nil {
		t.Fatal(err)
	}
	if len(ngJobs.Items) != 1 {
		t.Fatalf("exactly one Job, got %d", len(ngJobs.Items))
	}
	for _, v := range ngJobs.Items[0].Spec.Template.Spec.Volumes {
		if v.Name == "attach-sol-pi" {
			t.Fatal("a repository-scoped attachment without a trigger repo must not mount")
		}
	}
	absent := false
	for _, l := range ngLines {
		if strings.Contains(l, "sol-pi@repository=ABSENT") {
			absent = true
		}
	}
	if !absent {
		t.Fatalf("the nil-envelope path must log ABSENT for the unresolvable scope, got: %v", ngLines)
	}
}

// #726 r1 (round TODO): config.repoWiki is the one config key consumed
// as a typed scalar — a non-string shape must be a dispatch-time error
// naming the field, never a silent absent or a runtime clone failure.
func TestValidateRepoWikiConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  string
		want string // "" = legal
	}{
		{"absent", ``, ""},
		{"empty object", `{}`, ""},
		{"unrelated keys", `{"label":"needs-review","wiki":"https://x"}`, ""},
		{"string url", `{"repoWiki":"ssh://git@git.rezus.cloud/tibrez/rhesadox.wiki.git"}`, ""},
		{"null", `{"repoWiki":null}`, ""},
		{"structured value (the typo'd nesting survivor)", `{"repoWiki":{"url":"https://x"}}`, "config.repoWiki must be a string"},
		{"number", `{"repoWiki":42}`, "config.repoWiki must be a string"},
	} {
		err := ValidateRepoWikiConfig(json.RawMessage(tc.cfg))
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected err %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want containing %q", tc.name, err, tc.want)
		}
	}
	// non-object config (free-form JSON allows it): no repoWiki key to
	// validate — not this validator's error.
	if err := ValidateRepoWikiConfig(json.RawMessage(`["a","b"]`)); err != nil {
		t.Errorf("non-object config must pass (nothing to validate), got %v", err)
	}
}
