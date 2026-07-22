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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/observability"
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

// collectConfigMaps scans the Workflow spec for all ConfigMap references (prepare
// plugin, gate plugin, deploy plugin, task template) and returns unique names.
// These are mounted at /plugins/<name>/ so the worker's BuiltinResolver can
// execute ConfigMap-delivered scripts.
func collectConfigMaps(wf *v1alpha1.Workflow) []string {
	seen := map[string]bool{}
	var names []string
	add := func(cm string) {
		if cm != "" && !seen[cm] {
			seen[cm] = true
			names = append(names, cm)
		}
	}
	add(wf.Spec.Prepare.Plugin.ConfigMap)
	add(wf.Spec.Agent.Gate.Plugin.ConfigMap)
	add(wf.Spec.Deploy.Plugin.ConfigMap)
	add(wf.Spec.Agent.TaskTemplate.ConfigMap)
	return names
}

// configMapVolumes builds corev1.Volume entries for each referenced ConfigMap.
func configMapVolumes(wf *v1alpha1.Workflow) []corev1.Volume {
	var vols []corev1.Volume
	for _, cm := range collectConfigMaps(wf) {
		vols = append(vols, corev1.Volume{
			Name: cm,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: cm},
					DefaultMode:          pointerInt32(0755), // executable scripts
				},
			},
		})
	}
	return vols
}

// configMapVolumeMounts builds VolumeMount entries mounting each ConfigMap at
// /plugins/<name>/.
func configMapVolumeMounts(wf *v1alpha1.Workflow) []corev1.VolumeMount {
	var mounts []corev1.VolumeMount
	for _, cm := range collectConfigMaps(wf) {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      cm,
			MountPath: "/plugins/" + cm,
		})
	}
	return mounts
}

// labelsFor ties a worker Job to its Workflow. It propagates the
// harmostes.dev/owner label (if present) so the UI's owner-filtered Job
// queries can list a user's run history. Workflows without an owner label
// (GitOps-created system workflows) produce Jobs without one.
func labelsFor(wf *v1alpha1.Workflow) map[string]string {
	labels := map[string]string{
		"app.kubernetes.io/managed-by": "harmostes",
		v1alpha1.WorkflowLabel:         wf.Name,
	}
	if owner := wf.Labels[v1alpha1.OwnerLabel]; owner != "" {
		labels[v1alpha1.OwnerLabel] = owner
	}
	return labels
}

// +kubebuilder:rbac:groups=harmostes.dev,resources=workflows,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=harmostes.dev,resources=workflows/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=harmostes.dev,resources=workflows/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete

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

	// A worker Job already running for this Workflow? Wait for it.
	if active, err := r.hasActiveJob(ctx, &wf); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return ctrl.Result{}, err
	} else if active {
		span.SetAttributes(attribute.Bool("harmostes.active_job", true))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
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
	// Trace handoff: stamp the reconcile span's W3C context onto the worker Job
	// so its root harmostes.worker.run span is a child of this reconcile span —
	// one trace from "controller noticed a change" through "worker ran".
	tp := observability.TraceparentFromContext(ctx)
	if err := r.createWorkerJob(ctx, &wf, tp); err != nil {
		logger.Error(err, "create worker job")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}
	recordWorkflowRunScheduled(ctx, wf.Name)

	// If this run was triggered by a webhook, clear the trigger annotation now
	// that a worker has been scheduled. Without this, the status patch from
	// observeGeneration (below) triggers another reconcile, which sees the
	// annotation again and schedules another worker — an infinite rapid-fire
	// loop (~1 worker per reconcile cycle, every few seconds).
	if triggerRev := wf.Annotations["harmostes.dev/trigger-revision"]; triggerRev != "" {
		base := wf.DeepCopy()
		delete(wf.Annotations, "harmostes.dev/trigger-revision")
		if err := r.Patch(ctx, &wf, client.MergeFrom(base)); err != nil {
			logger.Error(err, "clear webhook trigger annotation")
		}
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
func (r *WorkflowReconciler) hasActiveJob(ctx context.Context, wf *v1alpha1.Workflow) (bool, error) {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(wf.Namespace), client.MatchingLabels(labelsFor(wf))); err != nil {
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
// changed since last observed, the poll interval elapsed since the last run,
// or a webhook trigger annotation is present.
func (r *WorkflowReconciler) isDue(wf *v1alpha1.Workflow) (bool, time.Duration) {
	// Webhook trigger: check for trigger-revision annotation
	if triggerRev := wf.Annotations["harmostes.dev/trigger-revision"]; triggerRev != "" {
		// Trigger if revision changed from last processed
		if triggerRev != wf.Status.LastProcessedRevision {
			return true, 0 // Trigger immediately
		}
		// Re-queue after short interval (webhook may arrive before annotation is processed)
		return false, 10 * time.Second
	}

	// Spec changed
	if wf.Status.ObservedGeneration != wf.Generation {
		return true, r.PollInterval
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
	// Default git token: shared cluster secret.
	gitToken := corev1.EnvVar{
		Name:      "HARMOSTES_GIT_TOKEN",
		ValueFrom: secretRef("harmostes-github-token", "token"),
	}
	// Phase C: if the workflow specifies a per-user tokenRef, use that instead.
	// This is how multi-tenant workflows get their own git credentials.
	if wf.Spec.WorkspaceRepo != nil && wf.Spec.WorkspaceRepo.TokenRef != nil {
		gitToken = corev1.EnvVar{
			Name: "HARMOSTES_GIT_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: wf.Spec.WorkspaceRepo.TokenRef.Name},
					Key:                  wf.Spec.WorkspaceRepo.TokenRef.Key,
				},
			},
		}
	}

	return []corev1.EnvVar{
		{Name: "HARMOSTES_WORKFLOW", Value: wf.Name},
		{Name: "HARMOSTES_NAMESPACE", Value: wf.Namespace},
		{Name: "HARMOSTES_WORKDIR", Value: "/workspace"},
		{Name: "HARMOSTES_SOURCE", Value: wf.Spec.Source.Revision},
		{Name: "DAPR_HTTP_ENDPOINT", Value: "http://127.0.0.1:3500"},
		{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: r.OTLPEndpoint},
		{Name: "OTEL_EXPORTER_OTLP_INSECURE", Value: strconv.FormatBool(r.OTLPInsecure)},
		gitToken,
		{Name: "HARMOSTES_RZC_USERNAME", ValueFrom: secretRef("harmostes-rzc-token", "username")},
		{Name: "HARMOSTES_RZC_PASSWORD", ValueFrom: secretRef("harmostes-rzc-token", "password")},
		{Name: "LITELLM_URL", ValueFrom: secretRef("harmostes-litellm-token", "url")},
		{Name: "LITELLM_API_KEY", ValueFrom: secretRef("harmostes-litellm-token", "key")},
	}
}

// workerEnvWithTraceparent returns the worker env with the W3C traceparent of the
// controller's reconcile span appended (Phase 4 trace handoff), so the worker's
// root harmostes.worker.run span is a child of the controller's. An empty
// traceparent (telemetry disabled / no recording span) omits it — the worker's
// root span is then its own trace root (the local-dev path).
func (r WorkflowReconciler) workerEnvWithTraceparent(wf *v1alpha1.Workflow, traceparent string) []corev1.EnvVar {
	env := r.workerEnv(wf)
	if traceparent != "" {
		env = append(env, corev1.EnvVar{Name: observability.TraceparentCarrierKey, Value: traceparent})
	}
	// Provenance (G8): stamp who/what triggered this run. The worker passes
	// these to the graph executor, which includes them in lifecycle events.
	triggeredBy := wf.Labels[v1alpha1.OwnerLabel]
	if triggeredBy == "" {
		triggeredBy = "system"
	}
	triggerSource := "controller" // default: periodic poll
	if wf.Annotations["harmostes.dev/trigger-revision"] != "" {
		triggerSource = "webhook"
	} else if wf.Spec.Source.Schedule != "" {
		triggerSource = "schedule"
	}
	env = append(env,
		corev1.EnvVar{Name: "HARMOSTES_TRIGGERED_BY", Value: triggeredBy},
		corev1.EnvVar{Name: "HARMOSTES_TRIGGER_SOURCE", Value: triggerSource},
	)
	return env
}

// createWorkerJob builds + creates the worker Job for one pipeline run. The
// traceparent (the W3C context of the reconcile span) is stamped on the worker
// container's env so the worker's root run-span is a child of the reconcile span.
func (r *WorkflowReconciler) createWorkerJob(ctx context.Context, wf *v1alpha1.Workflow, traceparent string) error {
	ctx, span := observability.Tracer().Start(ctx, "controller.create_worker_job",
		trace.WithAttributes(attribute.String("harmostes.workflow", wf.Name)))
	defer span.End()
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
			Labels:       labelsFor(wf),
			Annotations:  daprAnnotations,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            pointerInt32(0),
			TTLSecondsAfterFinished: pointerInt32(3600),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labelsFor(wf),
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
						Env:             r.workerEnvWithTraceparent(wf, traceparent),
						VolumeMounts:    append([]corev1.VolumeMount{{Name: "skills", MountPath: "/skills"}}, configMapVolumeMounts(wf)...),
					}},
					Volumes: append([]corev1.Volume{{
						Name:         "skills",
						VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
					}}, configMapVolumes(wf)...),
				},
			},
		},
	}
	if r.WorkerImagePullSecs != "" {
		job.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: r.WorkerImagePullSecs}}
	}
	// Set the Workflow as owner so the Job is GC'd with it.
	if err := ctrl.SetControllerReference(wf, job, r.Scheme); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	if err := r.Create(ctx, job); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}

// SetupWithManager registers the reconciler + its watches.
func (r *WorkflowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.registerActiveJobsGauge()
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
