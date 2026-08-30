package k8s

import (
	"context"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// attemptGVK is the Attempt's fully-qualified kind (owner references).
var attemptGVK = schema.GroupVersionKind{
	Group: v1alpha1.GroupName, Version: v1alpha1.Version, Kind: "Attempt",
}

// AttemptJobParams parameterize BuildJob — the per-Attempt Job pod shape
// (ADR-0007): one `harmostes-worker run` process per Attempt, isolated by
// construction (own pod, own /workspace, own Dapr sidecar).
type AttemptJobParams struct {
	// Attempt is the claim this Job executes. The Job carries a controller
	// ownerReference to it, so Job cleanup follows Attempt GC.
	Attempt *v1alpha1.Attempt

	// WorkflowName/Namespace are copied into the runner's env.
	WorkflowName string
	Namespace    string

	// Image is the harmostes-worker image; ServiceAccount the pod runs as.
	Image          string
	ServiceAccount string

	// TTLSecondsAfterFinished self-cleans finished Jobs (nil = cluster
	// default / none — set from chart values by the dispatcher).
	TTLSecondsAfterFinished *int32

	// PluginConfigMaps mount at /plugins/<name>, readOnly — the same
	// convention as the worker-pool deployment.
	PluginConfigMaps []string

	// DaprdImage overrides the sidecar image when non-empty (fleet
	// observability pinning, same knob as the worker-pool deployment).
	DaprdImage string

	// ExtraEnv carries KEY=VALUE entries appended to the runner env — the
	// trigger envelope (HARMOSTES_TRIGGER_*), as buildChildEnv passes to
	// consumer children.
	ExtraEnv []string
}

// BuildJob renders the per-Attempt Job. Shape contract (pinned by
// k8s_test.go): `harmostes-worker run` command; /workspace emptyDir; plugin
// ConfigMaps under /plugins; dapr sidecar annotations WITHOUT app-port (the
// runner never serves the subscription endpoint — that is the consumer's
// role); restartPolicy Never + backoffLimit 0 (retries are the dispatcher's
// re-arm, never kubelet's); ttlSecondsAfterFinished so finished Jobs and
// their pods self-clean.
func BuildJob(p AttemptJobParams) *batchv1.Job {
	attemptName := p.Attempt.Name
	labels := map[string]string{
		"app.kubernetes.io/name":      "harmostes",
		"app.kubernetes.io/component": "attempt-runner",
		"harmostes.dev/workflow":      p.WorkflowName,
		"harmostes.dev/attempt":       attemptName,
	}
	annotations := map[string]string{
		"dapr.io/enabled": "true",
		"dapr.io/app-id":  attemptName, // unique per Attempt: distinct state/pubsub identity
		"dapr.io/config":  "harmostes-config",
	}
	if p.DaprdImage != "" {
		annotations["dapr.io/sidecar-image"] = p.DaprdImage
		annotations["dapr.io/env"] = "OTEL_EXPORTER_OTLP_INSECURE=true"
	}

	env := []corev1.EnvVar{
		{Name: "HARMOSTES_WORKFLOW", Value: p.WorkflowName},
		{Name: "HARMOSTES_NAMESPACE", Value: p.Namespace},
		{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
	}
	for _, kv := range p.ExtraEnv {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env = append(env, corev1.EnvVar{Name: k, Value: v})
		}
	}

	volumes := []corev1.Volume{{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	mounts := []corev1.VolumeMount{{Name: "workspace", MountPath: "/workspace"}}
	for _, cm := range p.PluginConfigMaps {
		vol := "plugin-cm-" + cm
		volumes = append(volumes, corev1.Volume{
			Name: vol,
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: cm},
				// Plugins are executed (workspace.sh et al.); ConfigMap
				// volumes default to 0644 and exec dies with permission
				// denied. The pool's own Deployment mounts 0755 too (#283).
				DefaultMode: ptr.To(int32(0o755)),
			}},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: vol, MountPath: "/plugins/" + cm, ReadOnly: true})
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			// GenerateName: a Job name is unique PER RUN (an Attempt
			// continues across triggers, ADR-0005) — the Attempt itself is
			// the deterministic claim, carried in labels + ownerRef.
			GenerateName:    attemptName + "-",
			Namespace:       p.Namespace,
			Labels:          labels,
			Annotations:     annotations,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(p.Attempt, attemptGVK)},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: new(int32), // 0: retries are the dispatcher's re-arm
			// The wall-clock bound follows the run out of the pool pod
			// (was the consumer's run context; OneShotRunBound unchanged).
			ActiveDeadlineSeconds:   ptr.To(int64(v1alpha1.OneShotRunBound / time.Second)),
			TTLSecondsAfterFinished: p.TTLSecondsAfterFinished,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations},
				Spec: corev1.PodSpec{
					ServiceAccountName: p.ServiceAccount,
					RestartPolicy:      corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:         "run",
						Image:        p.Image,
						Command:      []string{"/usr/local/bin/harmostes-worker", "run"},
						Env:          env,
						VolumeMounts: mounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}
}

// ListActiveJobs returns the workflow's unfinished attempt Jobs — the
// dispatcher's capacity signal (ADR-0007): one live Job per accepted claim.
func ListActiveJobs(ctx context.Context, cl client.Client, namespace, workflow string) ([]batchv1.Job, error) {
	var list batchv1.JobList
	if err := cl.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabels{
		"app.kubernetes.io/name": "harmostes", "harmostes.dev/workflow": workflow,
	}); err != nil {
		return nil, err
	}
	live := make([]batchv1.Job, 0, len(list.Items))
	for _, j := range list.Items {
		// Live = not finished: a just-created Job reports Active==0 until
		// the job controller syncs, so counting only active would let
		// racing wakes double-dispatch inside that window.
		if j.DeletionTimestamp == nil && j.Status.CompletionTime == nil && !jobFailed(&j) {
			live = append(live, j)
		}
	}
	return live, nil
}

// jobFailed reports whether the Job reached its failed condition
// (backoffLimit 0 → one pod failure finishes it).
func jobFailed(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
