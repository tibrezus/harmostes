// Package controller implements the harmostes Deterministic Orchestration
// Kernel: a controller-runtime Reconciler that watches Workflow CRs and, when
// a run is due (spec changed or schedule elapsed), publishes a TriggerEvent to
// the Dapr pub/sub topic. The worker pool consumes the event and executes the
// workflow. The controller owns scheduling + observedGeneration; the worker
// owns the run outcome.
package controller

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/attempt"
	"github.com/tibrezus/harmostes/internal/dapr"
	"github.com/tibrezus/harmostes/internal/observability"
)

// WorkflowReconciler publishes trigger events for due Workflows.
type WorkflowReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	PollInterval time.Duration
	OTLPEndpoint string // OTLP collector endpoint (stamped on trigger events for the worker)
	OTLPInsecure bool   // set OTEL_EXPORTER_OTLP_INSECURE on workers (plain gRPC)

	// Pub/Sub trigger publishing.
	DaprClient   dapr.Client // the Dapr HTTP client for publishing trigger events
	TriggerTopic string      // pub/sub topic (default "harmostes-triggers")
}

// +kubebuilder:rbac:groups=harmostes.dev,resources=workflows,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=harmostes.dev,resources=workflows/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=harmostes.dev,resources=workflows/finalizers,verbs=update
// +kubebuilder:rbac:groups=harmostes.dev,resources=attempts,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=harmostes.dev,resources=attempts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=harmostes.dev,resources=attempts/finalizers,verbs=update

func (r *WorkflowReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx, span := observability.Tracer().Start(ctx, "harmostes.controller.reconcile",
		trace.WithAttributes(attribute.String("harmostes.workflow", req.Name)))
	defer span.End()
	start := time.Now()
	defer func() { recordReconcileSeconds(ctx, req.Name, time.Since(start)) }()

	logger := log.FromContext(ctx)

	var wf v1alpha1.Workflow
	if err := r.Get(ctx, req.NamespacedName, &wf); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if wf.Spec.Disabled {
		return ctrl.Result{RequeueAfter: r.PollInterval}, nil
	}

	due, requeueAfter := r.isDue(&wf)
	span.SetAttributes(
		attribute.Bool("harmostes.due", due),
		attribute.String("harmostes.reason", dueReason(&wf)),
	)
	if !due {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	logger.Info("scheduling worker", "workflow", wf.Name, "reason", dueReason(&wf))
	// Canonical Orchestration History (ADR-0005): resolve or create the Attempt
	// this run belongs to, so its history is recorded. Best-effort — an empty
	// attemptName means the worker records nothing (CRD absent / error).
	attemptName := r.resolveAttempt(ctx, &wf)
	// Trace handoff: stamp the reconcile span's W3C context onto the trigger
	// event so the worker's root harmostes.worker.run span is a child of this
	// reconcile span — one trace from "controller noticed a change" through
	// "worker ran".
	tp := observability.TraceparentFromContext(ctx)

	// For webhook wakes the revision under review is the trigger annotation
	// (the PR head SHA), not the last processed one.
	wakeRev := wf.Annotations["harmostes.dev/trigger-revision"]
	if wakeRev == "" {
		wakeRev = wf.Status.LastProcessedRevision
	}
	if err := r.publishTrigger(ctx, &wf, dueReason(&wf), wakeRev, tp, attemptName); err != nil {
		logger.Error(err, "publish trigger to pub/sub")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetAttributes(attribute.Bool("harmostes.pubsub_trigger", true))
	}

	// If this run was triggered by a webhook, clear the trigger annotation now
	// that a worker has been scheduled. Without this, the status patch from
	// observeGeneration (below) triggers another reconcile, which sees the
	// annotation again and schedules another worker — an infinite rapid-fire
	// loop.
	if triggerRev := wf.Annotations["harmostes.dev/trigger-revision"]; triggerRev != "" {
		base := wf.DeepCopy()
		delete(wf.Annotations, "harmostes.dev/trigger-revision")
		// The PR pointer rode the TriggerEvent payload (Pr/Action); clearing
		// here too prevents a stale wake from re-arming every poll cycle.
		delete(wf.Annotations, "harmostes.dev/trigger-pr")
		delete(wf.Annotations, "harmostes.dev/trigger-action")
		if err := r.Patch(ctx, &wf, client.MergeFrom(base)); err != nil {
			logger.Error(err, "clear webhook trigger annotation")
		}
	}

	// Mark this generation as observed (scheduling happened); the worker records
	// the run outcome (gateStatus, lastRunAt, …) via its status patcher.
	if err := r.observeGeneration(ctx, &wf); err != nil {
		logger.Error(err, "observe generation")
	}
	return ctrl.Result{RequeueAfter: r.PollInterval}, nil
}

// isDue decides whether a run should start now. Due if the spec generation
// changed since last observed, the poll interval elapsed since the last run,
// or a webhook trigger annotation is present.
func (r *WorkflowReconciler) isDue(wf *v1alpha1.Workflow) (bool, time.Duration) {
	// Webhook trigger: check for trigger-revision annotation
	if triggerRev := wf.Annotations["harmostes.dev/trigger-revision"]; triggerRev != "" {
		// Trigger if revision changed from last processed
		if triggerRev != wf.Status.LastProcessedRevision {
			return true, 0 // Trigger immediately
		}
	}

	// Spec changed
	if wf.Status.ObservedGeneration != wf.Generation {
		return true, r.PollInterval
	}

	// An ARMED Review-Ready gate (ADR-0006) must re-evaluate on schedule:
	// neither host sends a CI-completion wake (Forgejo has no status
	// webhooks; GitHub status/check_run events carry no `action`), so the
	// poll is the only thing that moves waiting → proceed. This carve-out
	// comes BEFORE the webhook-only guard: an armed gate on a kind:webhook
	// workflow would otherwise starve and die at the horizon.
	if wf.Status.ReviewReady != nil && wf.Status.ReviewReady.ArmedPR != 0 {
		return true, r.PollInterval
	}

	// Webhook-only workflows do not trigger on schedule — they wait for
	// push events. Without this guard, isDue falls through to the schedule
	// path and fires every PollInterval, defeating the event-driven model.
	if wf.Spec.Source.Kind == "webhook" {
		return false, r.PollInterval
	}

	// Schedule elapsed
	if !wf.Status.LastRunAt.IsZero() {
		elapsed := time.Since(wf.Status.LastRunAt.Time)
		if elapsed < r.PollInterval {
			return false, r.PollInterval - elapsed
		}
	}
	return true, r.PollInterval
}

func dueReason(wf *v1alpha1.Workflow) string {
	// Webhook trigger
	if triggerRev := wf.Annotations["harmostes.dev/trigger-revision"]; triggerRev != "" {
		return "webhook"
	}
	// Spec changed
	if wf.Status.ObservedGeneration != wf.Generation {
		return "spec changed"
	}
	return "schedule"
}

// observeGeneration patches status.observedGeneration + a Scheduling condition.
func (r *WorkflowReconciler) observeGeneration(ctx context.Context, wf *v1alpha1.Workflow) error {
	base := wf.DeepCopy()
	wf.Status.ObservedGeneration = wf.Generation
	// Cooldown anchor: stamp LastRunAt at SCHEDULE time (not just on worker
	// completion). isDue guards re-scheduling with time.Since(LastRunAt) <
	// PollInterval. LastRunAt was only ever set by the worker; a worker that
	// never runs (init-container crash, DNS failure, image pull error) left it
	// frozen, so isDue returned true every reconcile → rapid-fire triggers
	// (#118). The worker overwrites this with the actual completion time.
	wf.Status.LastRunAt = metav1.Now()
	wf.Status.Conditions = setCondition(wf.Status.Conditions, metav1.Condition{
		Type: "Scheduled", Status: metav1.ConditionTrue, Reason: "TriggerPublished",
		Message: "monitor controller published a trigger event", ObservedGeneration: wf.Generation,
	})
	return r.Status().Patch(ctx, wf, client.MergeFrom(base))
}

// SetupWithManager registers the reconciler + its watches.
func (r *WorkflowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.registerActiveJobsGauge()
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Workflow{}).
		Complete(r)
}

// triggerSourceOf derives the trigger channel (webhook | schedule | controller)
// from a Workflow. Shared by the Attempt objective derivation and the worker
// provenance env so the two agree on provenance.
func triggerSourceOf(wf *v1alpha1.Workflow) string {
	if wf.Annotations["harmostes.dev/trigger-revision"] != "" {
		return "webhook"
	}
	if wf.Spec.Source.Schedule != "" {
		return "schedule"
	}
	return "controller"
}

// resolveAttempt derives the Implementation Objective (ADR-0005) for this run,
// resolves or creates the Attempt realizing it, and returns its name. Best-
// effort: a failure (e.g. the Attempt CRD not yet installed in the cluster)
// returns an empty name — scheduling proceeds without canonical history rather
// than blocking the run.
func (r *WorkflowReconciler) resolveAttempt(ctx context.Context, wf *v1alpha1.Workflow) string {
	obj := attempt.DeriveObjective(wf, attempt.TriggerContext{
		Revision: wf.Annotations["harmostes.dev/trigger-revision"],
		Source:   triggerSourceOf(wf),
	})
	att, _, err := attempt.ResolveOrCreate(ctx, r.Client, obj, attempt.ResolveOptions{
		Namespace:   wf.Namespace,
		WorkflowRef: wf.Namespace + "/" + wf.Name,
		Owner:       wf,
		Scheme:      r.Scheme,
	})
	if err != nil {
		log.FromContext(ctx).Error(err, "resolve attempt (canonical history disabled for this run)")
		return ""
	}
	return att.Name
}

func setCondition(conds []metav1.Condition, c metav1.Condition) []metav1.Condition {
	c.LastTransitionTime = metav1.Now()
	for i, existing := range conds {
		if existing.Type == c.Type {
			if existing.Status != c.Status {
				conds[i] = c
			} else {
				conds[i].LastTransitionTime = existing.LastTransitionTime
				conds[i].Reason = c.Reason
				conds[i].Message = c.Message
			}
			return conds
		}
	}
	return append(conds, c)
}
