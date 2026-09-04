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

	// ExtraConfigMapMounts mounts additional ConfigMaps at explicit paths on
	// the one-shot Job, mirroring named mounts the worker-pool Deployment
	// carries. This is how pool-only plugins stay runnable in one-shot
	// workers: a fork-maintenance plugin needs its plugin ConfigMap AND the
	// engine scripts it execs (/workspace/scripts et al.) — mounts the pool
	// has but the Job never did (#311: prepare died in 12ms on a missing
	// script, empty message, forever).
	ExtraConfigMapMounts []ConfigMapMount

	// DaprdImage overrides the sidecar image when non-empty (fleet
	// observability pinning, same knob as the worker-pool deployment).
	DaprdImage string

	// ExtraEnv carries KEY=VALUE entries appended to the runner env — the
	// trigger envelope (HARMOSTES_TRIGGER_*), as buildChildEnv passes to
	// consumer children.
	ExtraEnv []string
}

// ConfigMapMount is one additional ConfigMap volume: name (the ConfigMap and
// volume name) and the absolute MountPath. Mode is the volume defaultMode
// (0o755 when nil — mounts exist so the one-shot worker can exec their
// contents; match the pool deployment's per-mount mode when it differs).
type ConfigMapMount struct {
	Name      string
	MountPath string
	Mode      *int32
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
	// Wall-clock bound (ADR-0008): a finite runBound on the attempt is
	// honored verbatim. Empty (the platform default) sets the 2h wedged-run
	// reaper — runs complete in minutes; a run this old is hung, not
	// working, and would hold its claim slot forever. Malformed values
	// degrade to the reaper, never to unlimited.
	bound := v1alpha1.DefaultRunBound
	if d, err := time.ParseDuration(p.Attempt.Spec.RunBound); err == nil {
		if d == 0 {
			bound = 0 // explicit "0" = truly unlimited (documented escape hatch)
		} else if d > 0 {
			bound = d
		}
	}
	var deadline *int64
	if bound > 0 {
		deadline = ptr.To(int64(bound / time.Second))
	}
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

	// Toolchain caches (ADR-0008/#336): when the attempt declares a cache,
	// mount the shared PVC and point the toolchains at it — a warm GOCACHE/
	// GOMODCACHE turns a cold `go test -race` from minutes into seconds,
	// which is the difference between a review fitting its budget and dying
	// at it. Namespaced subpaths keep workflows from trampling each other.
	if c := p.Attempt.Spec.Cache; c != nil && c.PVC != "" {
		volumes = append(volumes, corev1.Volume{
			Name:         "toolchain-cache",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c.PVC}},
		})
		// SubPath per namespace/workflow: the module cache's extraction path
		// does not tolerate cross-pod writers on one shared volume.
		mounts = append(mounts, corev1.VolumeMount{
			Name: "toolchain-cache", MountPath: "/toolchain-cache",
			SubPath: p.Namespace + "/" + p.WorkflowName,
		})
		if c.Go {
			env = append(env,
				corev1.EnvVar{Name: "GOCACHE", Value: "/toolchain-cache/go-build"},
				corev1.EnvVar{Name: "GOMODCACHE", Value: "/toolchain-cache/go-mod"},
			)
		}
		if c.NPM {
			env = append(env, corev1.EnvVar{Name: "npm_config_cache", Value: "/toolchain-cache/npm"})
		}
	}
	for _, m := range p.ExtraConfigMapMounts {
		mode := int32(0o755)
		if m.Mode != nil {
			mode = *m.Mode
		}
		vol := "extra-cm-" + m.Name
		volumes = append(volumes, corev1.Volume{
			Name: vol,
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: m.Name},
				DefaultMode:          ptr.To(mode),
			}},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: vol, MountPath: m.MountPath, ReadOnly: true})
	}
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
			// Wall-clock bound (ADR-0008): OPT-IN. A finite runBound on the
			// attempt sets activeDeadlineSeconds; empty (the platform default)
			// sets none — a run completes or fails on its own, the kernel does
			// not kill it. The review-ready breaker bounds zombie claims, and
			// attempt-scoped resumption covers real crashes (OOM, node loss).
			ActiveDeadlineSeconds:   deadline,
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
