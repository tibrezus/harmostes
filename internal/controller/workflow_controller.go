// Package controller implements the harmostes monitor controller: a
// controller-runtime Reconciler that watches Workflow CRs and, when a run is due
// (spec changed or schedule elapsed) and no worker is active, spawns a worker
// Job to execute that Workflow's pipeline. It owns the scheduling decision +
// the Workflow's observedGeneration; the worker owns the run outcome.
package controller

import (
	"context"
	"fmt"
	"strconv"
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
	Scheme              *runtime.Scheme
	WorkerImage         string
	WorkerImagePullSecs string // imagePullSecret name (optional)
	PollInterval        time.Duration
	ServiceAccountName  string
	JobNamespace        string // namespace to create worker Jobs in (default: the workflow's)
	DaprEnabled         bool   // inject the Dapr sidecar into worker Jobs (best-effort events/state)
	DaprdImage          string // rezuscloud/dapr fork sidecar image; empty = stock daprd (events/state only, no OTLP push)
	OTLPEndpoint        string // OTLP collector endpoint stamped on worker Jobs (enables the worker's own OTel SDK; empty = disabled)
	OTLPInsecure        bool   // set OTEL_EXPORTER_OTLP_INSECURE on worker sidecars (fork's GetIsSecure() defaults to TLS)
	SkillsRepo          string // git URL cloned by the init container into /skills before the worker starts
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

// buildDaprAnnotations returns the Dapr sidecar annotations for a worker Job.
// Stock daprd (no DaprdImage) injects events/state only; the rezuscloud/dapr
// fork (DaprdImage set) additionally pushes traces+metrics+logs via OTLP, so it
// gets dapr.io/sidecar-image plus OTEL_EXPORTER_OTLP_INSECURE when OTLPInsecure.
func (r *WorkflowReconciler) buildDaprAnnotations(wf *v1alpha1.Workflow) map[string]string {
	if !r.DaprEnabled {
		return map[string]string{}
	}
	a := map[string]string{
		"dapr.io/enabled": "true",
		"dapr.io/app-id":  "harmostes-worker-" + wf.Name,
		"dapr.io/config":  "harmostes-config",
	}
	if r.DaprdImage != "" {
		a["dapr.io/sidecar-image"] = r.DaprdImage
		if r.OTLPInsecure {
			a["dapr.io/env"] = "OTEL_EXPORTER_OTLP_INSECURE=true"
		}
	}
	return a
}

// workerEnv builds the worker container's environment for a Workflow run:
// identity (workflow/namespace/source), the Dapr sidecar endpoint, the model/git
// tokens (from secrets — never in the CR spec), and the OTel exporter config
// (Phase 2: OTEL_EXPORTER_OTLP_ENDPOINT enables the worker's own pipeline spans +
// traceparent join; an empty endpoint disables telemetry).
func (r WorkflowReconciler) workerEnv(wf *v1alpha1.Workflow) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "HARMOSTES_WORKFLOW", Value: wf.Name},
		{Name: "HARMOSTES_NAMESPACE", Value: wf.Namespace},
		{Name: "HARMOSTES_WORKDIR", Value: "/workspace"},
		{Name: "HARMOSTES_SOURCE", Value: wf.Spec.Source.Revision},
		{Name: "DAPR_HTTP_ENDPOINT", Value: "http://127.0.0.1:3500"},
		{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: r.OTLPEndpoint},
		{Name: "OTEL_EXPORTER_OTLP_INSECURE", Value: strconv.FormatBool(r.OTLPInsecure)},
		{Name: "HARMOSTES_GIT_TOKEN", ValueFrom: secretRef("harmostes-github-token", "token")},
		{Name: "HARMOSTES_RZC_USERNAME", ValueFrom: secretRef("harmostes-rzc-token", "username")},
		{Name: "HARMOSTES_RZC_PASSWORD", ValueFrom: secretRef("harmostes-rzc-token", "password")},
		{Name: "LITELLM_URL", ValueFrom: secretRef("harmostes-litellm-token", "url")},
		{Name: "LITELLM_API_KEY", ValueFrom: secretRef("harmostes-litellm-token", "key")},
	}
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
	// When DaprdImage is set, the rezuscloud/dapr fork is injected instead of
	// stock daprd, pushing traces+metrics+logs via OTLP.
	daprAnnotations := r.buildDaprAnnotations(wf)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("harmostes-%s-", wf.Name),
			Namespace:    ns,
			Labels:       labelsFor(wf.Name),
			Annotations:  daprAnnotations,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            pointerInt32(0),
			TTLSecondsAfterFinished: pointerInt32(3600),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labelsFor(wf.Name),
					Annotations: daprAnnotations,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: r.ServiceAccountName,
					RestartPolicy:      corev1.RestartPolicyNever,
					InitContainers: []corev1.Container{{
						Name:            "sync-skills",
						Image:           r.WorkerImage, // reuse worker image (has git)
						ImagePullPolicy: pullPolicy,
						Command: []string{"sh", "-c", fmt.Sprintf(
							"git clone --depth 1 %s /tmp/agents && cp -r /tmp/agents/skills /skills",
							r.SkillsRepo)},
						VolumeMounts: []corev1.VolumeMount{{Name: "skills", MountPath: "/skills"}},
					}},
					Containers: []corev1.Container{{
						Name:            "worker",
						Image:           r.WorkerImage,
						ImagePullPolicy: pullPolicy,
						Env:             r.workerEnv(wf),
						VolumeMounts:    []corev1.VolumeMount{{Name: "skills", MountPath: "/skills"}},
					}},
					Volumes: []corev1.Volume{{
						Name: "skills",
						VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
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

// secretRef builds a SecretKeySelector env source (so worker Jobs inherit the git
// + model tokens without those ever appearing in the CR spec).
func secretRef(name, key string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key,
	}}
}
