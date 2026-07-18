package controller

import (
	"context"
	"maps"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/k8s"
	"github.com/tibrezus/harmostes/internal/observability"
)

// TestBuildDaprAnnotations covers the Dapr sidecar-annotation contract for
// worker Jobs: stock daprd (events/state only) vs the rezuscloud/dapr fork
// (OTLP push). It locks the annotation set the Dapr injector reads, so a
// regression that drops dapr.io/sidecar-image or the insecure env is caught.
func TestBuildDaprAnnotations(t *testing.T) {
	wf := &v1alpha1.Workflow{}
	wf.Name = "llm-wiki"

	const forkAMD64 = "ghcr.io/rezuscloud/daprd:otel-metrics-latest-amd64"

	tests := []struct {
		name string
		r    WorkflowReconciler
		want map[string]string
	}{
		{
			name: "dapr disabled yields no annotations",
			r:    WorkflowReconciler{DaprEnabled: false},
			want: map[string]string{},
		},
		{
			name: "stock daprd injects events/state only",
			r:    WorkflowReconciler{DaprEnabled: true},
			want: map[string]string{
				"dapr.io/enabled": "true",
				"dapr.io/app-id":  "harmostes-worker-llm-wiki",
				"dapr.io/config":  "harmostes-config",
			},
		},
		{
			name: "fork daprd insecure adds sidecar-image and OTLP_INSECURE env",
			r: WorkflowReconciler{
				DaprEnabled:  true,
				DaprdImage:   forkAMD64,
				OTLPInsecure: true,
			},
			want: map[string]string{
				"dapr.io/enabled":       "true",
				"dapr.io/app-id":        "harmostes-worker-llm-wiki",
				"dapr.io/config":        "harmostes-config",
				"dapr.io/sidecar-image": forkAMD64,
				"dapr.io/env":           "OTEL_EXPORTER_OTLP_INSECURE=true",
			},
		},
		{
			name: "fork daprd secure omits the insecure env",
			r: WorkflowReconciler{
				DaprEnabled: true,
				DaprdImage:  forkAMD64,
			},
			want: map[string]string{
				"dapr.io/enabled":       "true",
				"dapr.io/app-id":        "harmostes-worker-llm-wiki",
				"dapr.io/config":        "harmostes-config",
				"dapr.io/sidecar-image": forkAMD64,
			},
		},
		{
			name: "daprd-image flag ignored when dapr disabled",
			r: WorkflowReconciler{
				DaprEnabled: false,
				DaprdImage:  forkAMD64,
			},
			want: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.r.buildDaprAnnotations(wf)
			if !maps.Equal(got, tt.want) {
				t.Errorf("buildDaprAnnotations() =\n  %v\nwant\n  %v", got, tt.want)
			}
		})
	}
}

// envMap folds an EnvVar slice into a name→value map (for assertions; ValueFrom
// vars show an empty value, which the presence check tolerates).
func envMap(envs []corev1.EnvVar) map[string]string {
	m := make(map[string]string, len(envs))
	for _, e := range envs {
		m[e.Name] = e.Value
	}
	return m
}

// TestWorkerEnv locks the Phase 2 contract: the OTel exporter config is stamped on
// every worker Job so the worker's own pipeline spans + traceparent join are
// enabled (endpoint set) or cleanly disabled (empty), alongside the unchanged
// identity + token env.
func TestWorkerEnv(t *testing.T) {
	wf := &v1alpha1.Workflow{}
	wf.Name = "llm-wiki"
	wf.Namespace = "harmostes"
	const ep = "signoz-otel-collector.signoz.svc.cluster.local:4317"

	t.Run("otel enabled stamps endpoint + insecure", func(t *testing.T) {
		env := envMap(WorkflowReconciler{OTLPEndpoint: ep, OTLPInsecure: true}.workerEnv(wf))
		if env["OTEL_EXPORTER_OTLP_ENDPOINT"] != ep {
			t.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT = %q, want %q", env["OTEL_EXPORTER_OTLP_ENDPOINT"], ep)
		}
		if env["OTEL_EXPORTER_OTLP_INSECURE"] != "true" {
			t.Errorf("OTEL_EXPORTER_OTLP_INSECURE = %q, want true", env["OTEL_EXPORTER_OTLP_INSECURE"])
		}
	})

	t.Run("otel disabled when endpoint empty", func(t *testing.T) {
		env := envMap(WorkflowReconciler{}.workerEnv(wf))
		if env["OTEL_EXPORTER_OTLP_ENDPOINT"] != "" {
			t.Errorf("expected empty endpoint (disabled), got %q", env["OTEL_EXPORTER_OTLP_ENDPOINT"])
		}
		if env["OTEL_EXPORTER_OTLP_INSECURE"] != "false" {
			t.Errorf("OTEL_EXPORTER_OTLP_INSECURE = %q, want false", env["OTEL_EXPORTER_OTLP_INSECURE"])
		}
	})

	// identity + tokens are always present regardless of observability config.
	env := envMap(WorkflowReconciler{}.workerEnv(wf))
	for _, k := range []string{"HARMOSTES_WORKFLOW", "HARMOSTES_NAMESPACE", "HARMOSTES_WORKDIR", "DAPR_HTTP_ENDPOINT", "HARMOSTES_GIT_TOKEN", "LITELLM_API_KEY"} {
		if _, ok := env[k]; !ok {
			t.Errorf("missing required env var %q", k)
		}
	}
	if env["HARMOSTES_WORKFLOW"] != "llm-wiki" || env["HARMOSTES_NAMESPACE"] != "harmostes" {
		t.Errorf("identity env = workflow=%q ns=%q", env["HARMOSTES_WORKFLOW"], env["HARMOSTES_NAMESPACE"])
	}
}

// TestWorkerEnvWithTraceparent locks the Phase 4 trace-handoff env contract: a
// non-empty traceparent is stamped as HARMOSTES_TRACEPARENT so the worker's root
// span links to the controller's reconcile span; an empty one is omitted (the
// worker's root span is then its own trace root — local-dev path).
func TestWorkerEnvWithTraceparent(t *testing.T) {
	wf := &v1alpha1.Workflow{}
	wf.Name = "llm-wiki"
	const tp = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

	t.Run("traceparent stamped when set", func(t *testing.T) {
		env := envMap(WorkflowReconciler{}.workerEnvWithTraceparent(wf, tp))
		if env[observability.TraceparentCarrierKey] != tp {
			t.Errorf("%s = %q, want %q", observability.TraceparentCarrierKey, env[observability.TraceparentCarrierKey], tp)
		}
	})
	t.Run("traceparent omitted when empty", func(t *testing.T) {
		env := envMap(WorkflowReconciler{}.workerEnvWithTraceparent(wf, ""))
		if _, ok := env[observability.TraceparentCarrierKey]; ok {
			t.Errorf("%s should be absent for an empty traceparent", observability.TraceparentCarrierKey)
		}
	})
}

// TestReconcileEmitsSpanAndHandoff is the Phase 4 acceptance test: a due
// reconcile emits a harmostes.controller.reconcile span (with due/reason attrs)
// + a controller.create_worker_job child, AND stamps the reconcile span's W3C
// traceparent on the spawned worker Job (so the worker's run-span is a child).
func TestReconcileEmitsSpanAndHandoff(t *testing.T) {
	exp := withTestTracer(t)

	wf := &v1alpha1.Workflow{}
	wf.Name = "llm-wiki"
	wf.Namespace = "harmostes"
	wf.Generation = 2 // spec changed since last observed → due
	wf.Status.ObservedGeneration = 1

	cl := fake.NewClientBuilder().
		WithScheme(k8s.Scheme()).
		WithStatusSubresource(&v1alpha1.Workflow{}).
		WithObjects(wf).
		Build()

	r := &WorkflowReconciler{
		Client:             cl,
		Scheme:             k8s.Scheme(),
		WorkerImage:        "ghcr.io/tibrezus/harmostes-worker:dev",
		ServiceAccountName: "harmostes-controller",
		PollInterval:       5 * time.Minute,
		JobNamespace:       "harmostes",
		SkillsRepo:         "https://github.com/tibrezus/agents.git",
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "llm-wiki", Namespace: "harmostes"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// 1. reconcile span + create_worker_job child were emitted.
	spans := exp.GetSpans()
	names := spanNameSet(spans)
	if !names["harmostes.controller.reconcile"] {
		t.Errorf("missing reconcile span; got %v", names)
	}
	if !names["controller.create_worker_job"] {
		t.Errorf("missing create_worker_job child span; got %v", names)
	}

	// 2. the reconcile span carries the due/reason attributes.
	rec := spanByName(spans, "harmostes.controller.reconcile")
	if due, ok := attrBool(rec, "harmostes.due"); !ok || !due {
		t.Errorf("reconcile span harmostes.due = %v(ok=%v), want true", due, ok)
	}
	if reason, _ := attrString(rec, "harmostes.reason"); reason != "spec changed" {
		t.Errorf("reconcile span harmostes.reason = %q, want \"spec changed\"", reason)
	}

	// 3. the spawned worker Job carries the traceparent, referencing the
	//    reconcile span's trace so the worker's root span is its child.
	var jobs batchv1.JobList
	if err := cl.List(context.Background(), &jobs, client.MatchingLabels{"app.kubernetes.io/managed-by": "harmostes"}); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected 1 worker Job, got %d", len(jobs.Items))
	}
	tp := envMap(jobs.Items[0].Spec.Template.Spec.Containers[0].Env)[observability.TraceparentCarrierKey]
	if tp == "" {
		t.Fatal("worker Job missing HARMOSTES_TRACEPARENT env (trace handoff not wired)")
	}
	wantPrefix := "00-" + rec.SpanContext.TraceID().String() + "-"
	if !strings.HasPrefix(tp, wantPrefix) {
		t.Errorf("traceparent %q does not reference the reconcile trace (want prefix %q)", tp, wantPrefix)
	}
}

// --- span helpers (hermetic OTel for the controller package) ---

// withTestTracer installs an in-memory span exporter (synchronous) as the global
// tracer for the test, plus the W3C TraceContext propagator that observability.Init
// sets in production (the trace-handoff path reads it). Both restored on cleanup.
func withTestTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	prev := otel.GetTracerProvider()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	prevProp := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		otel.SetTextMapPropagator(prevProp)
		_ = tp.Shutdown(context.Background())
	})
	return exp
}

func spanNameSet(spans []tracetest.SpanStub) map[string]bool {
	m := make(map[string]bool, len(spans))
	for _, s := range spans {
		m[s.Name] = true
	}
	return m
}

func spanByName(spans []tracetest.SpanStub, name string) tracetest.SpanStub {
	for _, s := range spans {
		if s.Name == name {
			return s
		}
	}
	return tracetest.SpanStub{}
}

func attrBool(s tracetest.SpanStub, key string) (bool, bool) {
	for _, a := range s.Attributes {
		if string(a.Key) == key {
			return a.Value.AsBool(), true
		}
	}
	return false, false
}

func attrString(s tracetest.SpanStub, key string) (string, bool) {
	for _, a := range s.Attributes {
		if string(a.Key) == key {
			return a.Value.AsString(), true
		}
	}
	return "", false
}

func TestIsDue_WebhookTrigger(t *testing.T) {
	tests := []struct {
		name             string
		wf               *v1alpha1.Workflow
		wantDue          bool
		wantRequeueAfter time.Duration
	}{
		{
			name: "webhook trigger with new revision",
			wf: &v1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"harmostes.dev/trigger-revision": "abc123",
					},
				},
				Status: v1alpha1.WorkflowStatus{
					LastProcessedRevision: "def456",
				},
			},
			wantDue:          true,
			wantRequeueAfter: 0,
		},
		{
			name: "webhook trigger with same revision",
			wf: &v1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"harmostes.dev/trigger-revision": "abc123",
					},
				},
				Status: v1alpha1.WorkflowStatus{
					LastProcessedRevision: "abc123",
				},
			},
			wantDue:          false,
			wantRequeueAfter: 10 * time.Second,
		},
		{
			name: "no webhook trigger, schedule elapsed",
			wf: &v1alpha1.Workflow{
				Status: v1alpha1.WorkflowStatus{
					LastRunAt:          metav1.Time{Time: time.Now().Add(-10 * time.Minute)},
					ObservedGeneration: 1,
				},
			},
			wantDue:          true,
			wantRequeueAfter: 5 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &WorkflowReconciler{PollInterval: 5 * time.Minute}
			due, requeueAfter := r.isDue(tt.wf)
			if due != tt.wantDue {
				t.Errorf("isDue() due = %v, want %v", due, tt.wantDue)
			}
			// Allow small tolerance for time-based tests
			if tt.wantRequeueAfter > 0 && (requeueAfter > tt.wantRequeueAfter+time.Second || requeueAfter < tt.wantRequeueAfter-time.Second) {
				t.Errorf("isDue() requeueAfter = %v, want ~%v", requeueAfter, tt.wantRequeueAfter)
			}
		})
	}
}

// TestLabelsFor locks the Phase B tenant-model contract: labelsFor produces the
// standard managed-by + workflow labels, and propagates the harmostes.dev/owner
// label from the Workflow to the Job when present. A Workflow without an owner
// label (GitOps-created system workflow) produces a Job without one.
func TestLabelsFor(t *testing.T) {
	t.Run("with owner label propagates it", func(t *testing.T) {
		wf := &v1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "llm-wiki",
				Labels: map[string]string{v1alpha1.OwnerLabel: "alice"},
			},
		}
		got := labelsFor(wf)
		want := map[string]string{
			"app.kubernetes.io/managed-by": "harmostes",
			v1alpha1.WorkflowLabel:         "llm-wiki",
			v1alpha1.OwnerLabel:            "alice",
		}
		if !maps.Equal(got, want) {
			t.Errorf("labelsFor() = %v, want %v", got, want)
		}
	})

	t.Run("without owner label omits it", func(t *testing.T) {
		wf := &v1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "llm-wiki"},
		}
		got := labelsFor(wf)
		if _, ok := got[v1alpha1.OwnerLabel]; ok {
			t.Errorf("labelsFor() should not include owner label for unmanaged workflow, got %v", got)
		}
		if got[v1alpha1.WorkflowLabel] != "llm-wiki" {
			t.Errorf("workflow label = %q, want llm-wiki", got[v1alpha1.WorkflowLabel])
		}
	})
}

// TestReconcilePropagatesOwnerLabel is the Phase B acceptance test: when a
// Workflow with harmostes.dev/owner=alice triggers a reconcile, the spawned
// worker Job carries the same owner label — so the UI's owner-filtered Job
// queries return alice's run history.
func TestReconcilePropagatesOwnerLabel(t *testing.T) {
	_ = withTestTracer(t)

	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "llm-wiki",
			Labels: map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec:   v1alpha1.WorkflowSpec{},
		Status: v1alpha1.WorkflowStatus{},
	}
	wf.Namespace = "harmostes"
	wf.Generation = 2 // spec changed → due
	wf.Status.ObservedGeneration = 1

	cl := fake.NewClientBuilder().
		WithScheme(k8s.Scheme()).
		WithStatusSubresource(&v1alpha1.Workflow{}).
		WithObjects(wf).
		Build()

	r := &WorkflowReconciler{
		Client:             cl,
		Scheme:             k8s.Scheme(),
		WorkerImage:        "ghcr.io/tibrezus/harmostes-worker:dev",
		ServiceAccountName: "harmostes-controller",
		PollInterval:       5 * time.Minute,
		JobNamespace:       "harmostes",
		SkillsRepo:         "https://github.com/tibrezus/agents.git",
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "llm-wiki", Namespace: "harmostes"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var jobs batchv1.JobList
	if err := cl.List(context.Background(), &jobs, client.MatchingLabels{v1alpha1.OwnerLabel: "alice"}); err != nil {
		t.Fatalf("list jobs by owner: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected 1 worker Job with owner=alice, got %d", len(jobs.Items))
	}

	job := jobs.Items[0]
	// Both the Job and its Pod template carry the owner label.
	if job.Labels[v1alpha1.OwnerLabel] != "alice" {
		t.Errorf("Job ObjectMeta owner label = %q, want alice", job.Labels[v1alpha1.OwnerLabel])
	}
	if job.Spec.Template.Labels[v1alpha1.OwnerLabel] != "alice" {
		t.Errorf("Pod template owner label = %q, want alice", job.Spec.Template.Labels[v1alpha1.OwnerLabel])
	}
}

// TestHasActiveJobWithOwner verifies that hasActiveJob finds a job using the
// full label set (workflow + owner), not just the workflow name.
func TestHasActiveJobWithOwner(t *testing.T) {
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llm-wiki",
			Namespace: "harmostes",
			Labels:    map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
	}

	activeJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "harmostes-llm-wiki-1",
			Namespace: "harmostes",
			Labels:    labelsFor(wf),
		},
		Status: batchv1.JobStatus{}, // Succeeded=0, Failed=0 → active
	}

	cl := fake.NewClientBuilder().
		WithScheme(k8s.Scheme()).
		WithObjects(activeJob).
		Build()

	r := &WorkflowReconciler{Client: cl}
	active, err := r.hasActiveJob(context.Background(), wf)
	if err != nil {
		t.Fatalf("hasActiveJob: %v", err)
	}
	if !active {
		t.Error("expected active job to be found with owner+workflow labels")
	}
}

// TestHasActiveJobIgnoresOtherOwner verifies that a job for the same workflow
// name but a DIFFERENT owner (cross-tenant) is not counted as active — the
// label filter is scoped by owner.
func TestHasActiveJobIgnoresOtherOwner(t *testing.T) {
	wfMine := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llm-wiki",
			Namespace: "harmostes",
			Labels:    map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
	}

	// A job owned by bob (different tenant)
	wfBob := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "llm-wiki",
			Labels: map[string]string{v1alpha1.OwnerLabel: "bob"},
		},
	}
	bobJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "harmostes-llm-wiki-bob",
			Namespace: "harmostes",
			Labels:    labelsFor(wfBob),
		},
		Status: batchv1.JobStatus{},
	}

	cl := fake.NewClientBuilder().
		WithScheme(k8s.Scheme()).
		WithObjects(bobJob).
		Build()

	r := &WorkflowReconciler{Client: cl}
	active, err := r.hasActiveJob(context.Background(), wfMine)
	if err != nil {
		t.Fatalf("hasActiveJob: %v", err)
	}
	if active {
		t.Error("should not find bob's job when querying for alice's workflow")
	}
}
