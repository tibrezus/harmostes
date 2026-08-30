package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/attempt"
	"github.com/tibrezus/harmostes/internal/dapr"
	"github.com/tibrezus/harmostes/internal/k8s"
	"github.com/tibrezus/harmostes/internal/review"
	"github.com/tibrezus/harmostes/internal/timeline"
)

// Dispatcher is the consumer's RunFunc implementation (ADR-0007 phase 3):
// it validates the trigger through the Review-Ready Gate, applies the
// workflow's concurrency capacity, and dispatches the run as an Attempt +
// Job — in milliseconds. It never runs graphs: the Job pod's
// `harmostes-worker run` does, with HARMOSTES_DISPATCHED_ATTEMPT marking the
// gate as already satisfied.
type Dispatcher struct {
	// createMu serializes the Attempt+Job create section: two racing wakes
	// for the same PR must not double-dispatch. Correctness comes from the
	// live-Job check inside the lock (deterministic Attempt identity alone
	// would not dedupe — GenerateName makes Job names unique per run).
	createMu sync.Mutex

	cl        client.Client
	scheme    *runtime.Scheme
	namespace string
	logf      func(string, ...any)

	fleetMaxConcurrent int
	jobImage           string
	serviceAccount     string
	jobTTLSeconds      *int32
	daprdImage         string
	pluginConfigMaps   []string
}

// DispatchConfig carries the dispatcher's fleet-level knobs (chart env).
type DispatchConfig struct {
	FleetMaxConcurrent int
	JobImage           string
	ServiceAccount     string
	JobTTLSeconds      *int32
	DaprdImage         string
	PluginConfigMaps   []string
}

// NewDispatcher builds the dispatcher, connecting the in-cluster client.
func NewDispatcher(ctx context.Context, cfg DispatchConfig, logf func(string, ...any)) (*Dispatcher, error) {
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("k8s config: %w", err)
	}
	// k8s.Scheme() registers harmostes + corev1 + batchv1. The dispatcher
	// lists batchv1.Jobs (live-Job dedupe, capacity) — a v1alpha1-only
	// scheme fails every List with "no kind is registered" (#277).
	scheme := k8s.Scheme()
	cl, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("k8s client: %w", err)
	}
	ns := os.Getenv("HARMOSTES_NAMESPACE")
	if ns == "" {
		ns = "harmostes"
	}
	return &Dispatcher{
		cl:                 cl,
		scheme:             scheme,
		namespace:          ns,
		logf:               logf,
		fleetMaxConcurrent: cfg.FleetMaxConcurrent,
		jobImage:           cfg.JobImage,
		serviceAccount:     cfg.ServiceAccount,
		jobTTLSeconds:      cfg.JobTTLSeconds,
		daprdImage:         cfg.DaprdImage,
		pluginConfigMaps:   cfg.PluginConfigMaps,
	}, nil
}

// DispatcherFromEnv resolves the fleet-level dispatch knobs from the chart
// environment: HARMOSTES_MAX_CONCURRENT (default 3, ADR-0007),
// HARMOSTES_JOB_TTL_SECONDS (default 3600), the worker image (the Job runs
// the same image as the pool), the plugin ConfigMaps, and the optional daprd
// pin.
func DispatcherFromEnv(pluginConfigMaps []string, logf func(string, ...any)) (*Dispatcher, error) {
	cfg := DispatchConfig{
		JobImage:         os.Getenv("HARMOSTES_WORKER_IMAGE"),
		ServiceAccount:   os.Getenv("HARMOSTES_SERVICE_ACCOUNT"),
		DaprdImage:       os.Getenv("HARMOSTES_DAPRD_IMAGE"),
		PluginConfigMaps: pluginConfigMaps,
	}
	cfg.FleetMaxConcurrent = 3
	if v := os.Getenv("HARMOSTES_MAX_CONCURRENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.FleetMaxConcurrent = n
		}
	}
	ttl := int32(3600)
	if v := os.Getenv("HARMOSTES_JOB_TTL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			ttl = int32(n)
		}
	}
	cfg.JobTTLSeconds = &ttl
	return NewDispatcher(context.Background(), cfg, logf)
}

// Dispatch processes one trigger event: gate → capacity → Attempt+Job.
// A nil return ACKs the event (the armed state is the durable queue —
// waiting/standdown/capacity all re-evaluate on the next wake or sweep).
func (d *Dispatcher) Dispatch(ctx context.Context, req RunRequest) error {
	wf, err := FetchWorkflow(ctx, d.cl, req.Namespace, req.Workflow)
	if err != nil {
		return fmt.Errorf("fetch workflow: %w", err)
	}

	// ── Review-Ready Gate (ADR-0006): validate before dispatching. ──────
	// Only workflows with reviewReady gate; every other class dispatches
	// straight through (every class is Job-per-run, ADR-0007). The gate
	// drains to capacity: one sweep accepts every free slot.
	gateDeps := GateDeps{
		Status:             k8s.StatusPatcher{Client: d.cl, Namespace: req.Namespace},
		Client:             d.cl,
		Scheme:             d.scheme,
		FleetMaxConcurrent: d.fleetMaxConcurrent,
		Log:                d.logf,
		TL: timeline.NewGateWriter(dapr.Tracing(dapr.New(os.Getenv("DAPR_HTTP_ENDPOINT"))),
			envOr("HARMOSTES_STATE_STORE", "statestore"), wf.Name, "", triggerSubject(req)),
	}

	var dispatches []GateDispatch
	if wf.Spec.ReviewReady != nil {
		dispatches, err = RunReviewGateSweep(ctx, gateDeps, wf)
		if err != nil {
			return fmt.Errorf("review gate: %w", err)
		}
		if len(dispatches) == 0 {
			return nil // waiting/standdown/at-capacity — recorded in aggregates
		}
	} else {
		// Non-gated class: dispatch straight through under a claimless
		// attempt (deterministic objective identity).
		obj := attempt.DeriveObjective(wf, attempt.TriggerContext{Revision: req.Revision, Source: "webhook"})
		at, _, err := attempt.ResolveOrCreate(ctx, d.cl, obj, attempt.ResolveOptions{
			Namespace:   req.Namespace,
			WorkflowRef: req.Namespace + "/" + req.Workflow,
			Owner:       wf,
			Scheme:      d.scheme,
		})
		if err != nil {
			return fmt.Errorf("resolve attempt: %w", err)
		}
		dispatches = append(dispatches, GateDispatch{Attempt: at.Name})
	}

	// ── Create the Jobs (serialized: dedupe racing wakes). ──────────────
	d.createMu.Lock()
	defer d.createMu.Unlock()

	live, err := k8s.ListActiveJobs(ctx, d.cl, req.Namespace, req.Workflow)
	if err != nil {
		return fmt.Errorf("list active jobs: %w", err)
	}
	for _, g := range dispatches {
		duplicate := false
		for _, j := range live {
			if j.Labels["harmostes.dev/attempt"] == g.Attempt {
				duplicate = true
				break
			}
		}
		if duplicate {
			d.logf("dispatch: attempt %s already has a live job — deduped", g.Attempt)
			continue
		}
		var at v1alpha1.Attempt
		if err := d.cl.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: g.Attempt}, &at); err != nil {
			return fmt.Errorf("get claim attempt %s: %w", g.Attempt, err)
		}
		job := k8s.BuildJob(k8s.AttemptJobParams{
			Attempt:                 &at,
			WorkflowName:            req.Workflow,
			Namespace:               req.Namespace,
			Image:                   d.jobImage,
			ServiceAccount:          d.serviceAccount,
			TTLSecondsAfterFinished: d.jobTTLSeconds,
			DaprdImage:              d.daprdImage,
			PluginConfigMaps:        d.pluginConfigMaps,
			ExtraEnv:                dispatchEnv(req, &at, g.Envelope),
		})
		if err := d.cl.Create(ctx, job); err != nil {
			if errors.IsAlreadyExists(err) {
				d.logf("dispatch: job for attempt %s already exists — deduped", at.Name)
				continue
			}
			return fmt.Errorf("create job: %w", err)
		}
		if err := attempt.MarkClaimDispatched(ctx, d.cl, req.Namespace, at.Name); err != nil {
			return fmt.Errorf("mark dispatched %s: %w", at.Name, err)
		}
		d.logf("dispatch: job %s created for attempt %s (workflow %s)", job.Name, at.Name, req.Workflow)
	}
	return nil
}

// triggerSubject builds the timeline subject from the trigger event.
func triggerSubject(req RunRequest) timeline.Subject {
	s := timeline.Subject{}
	if req.Pr != "" {
		s.Kind = "pr"
		s.Ref = req.Pr
		s.SHA = req.Revision
		s.Title = req.PrTitle
	}
	return s
}

// dispatchEnv renders the Job's trigger env: the envelope (gate output) when
// present, else the raw wake fields — plus the dispatched-claim marker that
// makes `harmostes-worker run` skip the gate.
func dispatchEnv(req RunRequest, at *v1alpha1.Attempt, env *review.Envelope) []string {
	vars := []string{
		"HARMOSTES_ATTEMPT=" + at.Name,
		"HARMOSTES_DISPATCHED_ATTEMPT=" + at.Name,
	}
	if req.Source != "" {
		vars = append(vars, "HARMOSTES_SOURCE="+req.Source)
	}
	if req.Traceparent != "" {
		vars = append(vars, "HARMOSTES_TRACEPARENT="+req.Traceparent)
	}
	if env != nil {
		return append(vars, EnvelopeEnv(env)...)
	}
	if req.Pr != "" {
		vars = append(vars, "HARMOSTES_TRIGGER_PR="+req.Pr)
	}
	if req.Action != "" {
		vars = append(vars, "HARMOSTES_TRIGGER_ACTION="+req.Action)
	}
	if req.Revision != "" {
		vars = append(vars, "HARMOSTES_TRIGGER_REVISION="+req.Revision)
	}
	if req.PrTitle != "" {
		vars = append(vars, "HARMOSTES_TRIGGER_TITLE="+req.PrTitle)
	}
	return vars
}

// EnvelopeEnv exports a Trigger Envelope as the HARMOSTES_TRIGGER_* env form
// the workspace plugin consumes. One bridge for both paths: the dispatcher's
// Job env and the one-shot run's process env.
func EnvelopeEnv(env *review.Envelope) []string {
	envelopeJSON, _ := json.Marshal(env)
	return []string{
		"HARMOSTES_TRIGGER_REPO=" + env.Repo,
		"HARMOSTES_TRIGGER_PR=" + strconv.Itoa(env.PR),
		"HARMOSTES_TRIGGER_SHA=" + env.HeadSHA,
		"HARMOSTES_TRIGGER_BASE=" + env.Base,
		"HARMOSTES_TRIGGER_LABEL=" + env.Label,
		"HARMOSTES_TRIGGER_CONTEXTS=" + string(envelopeJSON),
	}
}

// FetchWorkflow fetches a Workflow CR with its WorkflowTemplate defaults
// applied — the merged spec every trigger path executes. Shared by the
// dispatcher and the one-shot run.
func FetchWorkflow(ctx context.Context, cl client.Client, namespace, name string) (*v1alpha1.Workflow, error) {
	var wf v1alpha1.Workflow
	if err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &wf); err != nil {
		return nil, fmt.Errorf("get workflow %s/%s: %w", namespace, name, err)
	}
	if ref := wf.Spec.TemplateRef; ref != "" {
		var tmpl v1alpha1.WorkflowTemplate
		if err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref}, &tmpl); err != nil {
			return nil, fmt.Errorf("get workflow template %s/%s: %w", namespace, ref, err)
		}
		v1alpha1.ApplyTemplateDefaults(&wf, &tmpl)
	}
	return &wf, nil
}
