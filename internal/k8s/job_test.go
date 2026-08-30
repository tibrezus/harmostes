package k8s

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

func jobTestAttempt() *v1alpha1.Attempt {
	return &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name: "attempt-pr-review-harmostes-0a1b2c3d4e5f", Namespace: "harmostes", UID: "uid-1234",
		},
	}
}

// #270: the per-Attempt Job shape contract — one `harmostes-worker run`
// process, isolated by construction, owned by the Attempt claim.
func TestBuildJobShape(t *testing.T) {
	attempt := jobTestAttempt()
	ttl := int32(3600)
	job := BuildJob(AttemptJobParams{
		Attempt:                 attempt,
		WorkflowName:            "pr-review-harmostes",
		Namespace:               "harmostes",
		Image:                   "ghcr.io/tibrezus/harmostes-worker:1.2.3",
		ServiceAccount:          "harmostes-controller",
		TTLSecondsAfterFinished: &ttl,
		PluginConfigMaps:        []string{"fork-maintenance-plugins"},
		DaprdImage:              "ghcr.io/daprio/daprd:1.16",
		ExtraEnv:                []string{"HARMOSTES_TRIGGER_PR=github.com/tibrezus/harmostes#264", "malformed-no-equals", ""},
	})

	if job.Name != attempt.Name || job.Namespace != attempt.Namespace {
		t.Fatalf("job must carry the attempt's namespaced name, got %s/%s", job.Namespace, job.Name)
	}

	// Owner: the Attempt claim is the controller owner (GC follows it).
	refs := job.OwnerReferences
	if len(refs) != 1 || refs[0].Kind != "Attempt" || refs[0].UID != types.UID("uid-1234") || refs[0].Controller == nil || !*refs[0].Controller {
		t.Fatalf("job must be controller-owned by the Attempt, got %+v", refs)
	}

	// Dapr sidecar: per-attempt app-id, config, image pin — and NO app-port
	// (the runner never serves the subscription endpoint).
	for _, m := range []map[string]string{job.Annotations, job.Spec.Template.Annotations} {
		if m["dapr.io/enabled"] != "true" || m["dapr.io/app-id"] != attempt.Name || m["dapr.io/config"] != "harmostes-config" {
			t.Fatalf("dapr annotations wrong: %v", m)
		}
		if m["dapr.io/app-port"] != "" {
			t.Fatalf("run pods must not expose app-port (no subscription): %v", m)
		}
		if m["dapr.io/sidecar-image"] != "ghcr.io/daprio/daprd:1.16" {
			t.Fatalf("daprd image pin missing: %v", m)
		}
	}

	c := job.Spec.Template.Spec.Containers[0]
	if got := strings.Join(c.Command, " "); got != "/usr/local/bin/harmostes-worker run" {
		t.Fatalf("command must be the run subcommand, got %q", got)
	}

	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["HARMOSTES_WORKFLOW"] != "pr-review-harmostes" || env["HARMOSTES_NAMESPACE"] != "harmostes" {
		t.Fatalf("workflow env missing: %v", env)
	}
	if env["HARMOSTES_TRIGGER_PR"] != "github.com/tibrezus/harmostes#264" {
		t.Fatalf("trigger envelope env not passed through: %v", env)
	}

	// /workspace is per-Job emptyDir; plugins mount readOnly under /plugins.
	var ws *corev1.Volume
	for i := range job.Spec.Template.Spec.Volumes {
		if job.Spec.Template.Spec.Volumes[i].Name == "workspace" {
			ws = &job.Spec.Template.Spec.Volumes[i]
		}
	}
	if ws == nil || ws.EmptyDir == nil {
		t.Fatalf("/workspace must be a per-Job emptyDir, volumes: %+v", job.Spec.Template.Spec.Volumes)
	}
	foundMount := false
	for _, m := range c.VolumeMounts {
		if m.Name == "workspace" && m.MountPath == "/workspace" {
			foundMount = true
		}
		if m.MountPath == "/plugins/fork-maintenance-plugins" && !m.ReadOnly {
			t.Fatalf("plugin mounts must be readOnly: %+v", m)
		}
	}
	if !foundMount {
		t.Fatalf("/workspace mount missing: %+v", c.VolumeMounts)
	}

	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("restartPolicy must be Never, got %q", job.Spec.Template.Spec.RestartPolicy)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Fatalf("backoffLimit must be 0 (retries are the dispatcher's re-arm), got %+v", job.Spec.BackoffLimit)
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 3600 {
		t.Fatalf("TTL not applied: %+v", job.Spec.TTLSecondsAfterFinished)
	}
	if job.Spec.Template.Spec.ServiceAccountName != "harmostes-controller" {
		t.Fatalf("serviceAccountName not applied: %q", job.Spec.Template.Spec.ServiceAccountName)
	}
}

// #270: no TTL configured → no TTL field rendered (the cluster default or
// GC-by-owner applies; the builder must not invent a policy).
func TestBuildJobTTLNilOmitted(t *testing.T) {
	job := BuildJob(AttemptJobParams{
		Attempt:      jobTestAttempt(),
		WorkflowName: "wf",
		Namespace:    "harmostes",
		Image:        "img",
	})
	if job.Spec.TTLSecondsAfterFinished != nil {
		t.Fatalf("nil TTL must stay nil, got %d", *job.Spec.TTLSecondsAfterFinished)
	}
	if job.Annotations["dapr.io/sidecar-image"] != "" {
		t.Fatalf("unset daprd image must not pin the sidecar, got %q", job.Annotations["dapr.io/sidecar-image"])
	}
}
