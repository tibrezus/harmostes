// Package controller implements the harmostes monitor controller: a
// controller-runtime Reconciler that watches Workflow CRs and, when a run is due
// (spec changed or schedule elapsed) and no worker is active, spawns a worker
// Job to execute that Workflow's pipeline. It owns the scheduling decision +
// the Workflow's observedGeneration; the worker owns the run outcome.
package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// WorkflowReconciler schedules worker Jobs for due Workflows.
type WorkflowReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	WorkerImage          string
	WorkerImagePullSecs  string // imagePullSecret name (optional)
	PollInterval         time.Duration
	ServiceAccountName   string
	JobNamespace         string // namespace to create worker Jobs in (default: the workflow's)
	DaprEnabled          bool   // inject the Dapr sidecar into worker Jobs (best-effort events/state)
}

// labelsFor ties a worker Job to its Workflow.
func labelsFor(name string) map[string]string {
	return map[string]string{"app.kubernetes.io/managed-by": "harmostes", "harmostes.dev/workflow": name}
}

// +kubebuilder:rbac:groups=harmostes.dev,resources=workflows,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=harmostes.dev,resources=workflows/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=harmostes.dev,resources=workflows/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete

func (r *WorkflowReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var wf v1alpha1.Workflow
	if err := r.Get(ctx, req.NamespacedName, &wf); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if wf.Spec.Disabled {
		return ctrl.Result{RequeueAfter: r.PollInterval}, nil
	}

	// A worker Job already running for this Workflow? Wait for it.
	if active, err := r.hasActiveJob(ctx, wf.Namespace, wf.Name); err != nil {
		return ctrl.Result{}, err
	} else if active {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	due, requeueAfter := r.isDue(&wf)
	if !due {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	logger.Info("scheduling worker", "workflow", wf.Name, "reason", dueReason(&wf))
	if err := r.createWorkerJob(ctx, &wf); err != nil {
		logger.Error(err, "create worker job")
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	// Mark this generation as observed (scheduling happened); the worker records
	// the run outcome (gateStatus, lastRunAt, …) via its StatusPatcher.
	if err := r.observeGeneration(ctx, &wf); err != nil {
		logger.Error(err, "observe generation")
	}
	return ctrl.Result{RequeueAfter: r.PollInterval}, nil
}

// hasActiveJob reports whether a non-terminal worker Job exists for the workflow.
// With backoffLimit=0 a Job is terminal once it has any succeeded or failed pod;
// a Pending/Running job (Succeeded==0 && Failed==0) counts as active.
func (r *WorkflowReconciler) hasActiveJob(ctx context.Context, ns, name string) (bool, error) {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(ns), client.MatchingLabels(labelsFor(name))); err != nil {
		return false, err
	}
	for _, j := range jobs.Items {
		if j.Status.Succeeded == 0 && j.Status.Failed == 0 {
			return true, nil
		}
	}
	return false, nil
}

// isDue decides whether a run should start now. Due if the spec generation
// changed since last observed, or the poll interval elapsed since the last run.
func (r *WorkflowReconciler) isDue(wf *v1alpha1.Workflow) (bool, time.Duration) {
	if wf.Status.ObservedGeneration != wf.Generation {
		return true, r.PollInterval
	}
	if !wf.Status.LastRunAt.IsZero() {
		elapsed := time.Since(wf.Status.LastRunAt.Time)
		if elapsed < r.PollInterval {
			return false, r.PollInterval - elapsed
		}
	}
	return true, r.PollInterval
}

func dueReason(wf *v1alpha1.Workflow) string {
	if wf.Status.ObservedGeneration != wf.Generation {
		return "spec changed"
	}
	return "schedule"
}

// observeGeneration patches status.observedGeneration + a Scheduling condition.
func (r *WorkflowReconciler) observeGeneration(ctx context.Context, wf *v1alpha1.Workflow) error {
	base := wf.DeepCopy()
	wf.Status.ObservedGeneration = wf.Generation
	wf.Status.Conditions = setCondition(wf.Status.Conditions, metav1.Condition{
		Type: "Scheduled", Status: metav1.ConditionTrue, Reason: "WorkerScheduled",
		Message: "monitor controller scheduled a worker Job", ObservedGeneration: wf.Generation,
	})
	return r.Status().Patch(ctx, wf, client.MergeFrom(base))
}

// createWorkerJob builds + creates the worker Job for one pipeline run.
func (r *WorkflowReconciler) createWorkerJob(ctx context.Context, wf *v1alpha1.Workflow) error {
	ns := r.JobNamespace
	if ns == "" {
		ns = wf.Namespace
	}
	pullPolicy := corev1.PullAlways // dev/mutable tags: always pull the latest digest
	// Dapr sidecar injection is best-effort (events/state). Requires the namespace
	// + service account to be trusted by the Dapr sentry (mTLS); disabled until
	// that trust is wired (the worker proceeds without events/state otherwise).
	daprAnnotations := map[string]string{}
	if r.DaprEnabled {
		daprAnnotations = map[string]string{
			"dapr.io/enabled": "true",
			"dapr.io/app-id":  "harmostes-worker-" + wf.Name,
			"dapr.io/config": "harmostes-config",
		}
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("harmostes-%s-", wf.Name),
			Namespace:    ns,
			Labels:       labelsFor(wf.Name),
			Annotations:  daprAnnotations,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: pointerInt32(0),
			TTLSecondsAfterFinished: pointerInt32(3600),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labelsFor(wf.Name),
					Annotations: daprAnnotations,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: r.ServiceAccountName,
					RestartPolicy:      corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:            "worker",
						Image:           r.WorkerImage,
						ImagePullPolicy: pullPolicy,
						Env: []corev1.EnvVar{
							{Name: "HARMOSTES_WORKFLOW", Value: wf.Name},
							{Name: "HARMOSTES_NAMESPACE", Value: wf.Namespace},
							{Name: "HARMOSTES_WORKDIR", Value: "/workspace"},
							{Name: "HARMOSTES_SOURCE", Value: wf.Spec.Source.Revision},
							{Name: "DAPR_HTTP_ENDPOINT", Value: "http://127.0.0.1:3500"},
						},
					}},
				},
			},
		},
	}
	if r.WorkerImagePullSecs != "" {
		job.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: r.WorkerImagePullSecs}}
	}
	// Set the Workflow as owner so the Job is GC'd with it.
	if err := ctrl.SetControllerReference(wf, job, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, job)
}

// SetupWithManager registers the reconciler + its watches.
func (r *WorkflowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Workflow{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}

// setCondition upserts a condition into the slice (by Type).
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

func pointerInt32(v int32) *int32 { return &v }
