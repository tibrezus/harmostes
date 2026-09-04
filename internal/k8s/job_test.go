package k8s

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"

	"k8s.io/utils/ptr"
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
// #283: plugin ConfigMap volumes must mount executable (0755) — the pool
// Deployment does; Jobs created by the dispatcher otherwise fail every
// plugin node with permission denied in milliseconds.
func TestBuildJobPluginVolumeExecutable(t *testing.T) {
	job := BuildJob(AttemptJobParams{
		Attempt:          jobTestAttempt(),
		WorkflowName:     "pr-review-harmostes",
		Namespace:        "harmostes",
		PluginConfigMaps: []string{"harmostes-pr-review"},
	})
	var vol *corev1.Volume
	for i := range job.Spec.Template.Spec.Volumes {
		if job.Spec.Template.Spec.Volumes[i].Name == "plugin-cm-harmostes-pr-review" {
			vol = &job.Spec.Template.Spec.Volumes[i]
		}
	}
	if vol == nil {
		t.Fatal("plugin volume missing")
	}
	if mode := vol.ConfigMap.DefaultMode; mode == nil || *mode != 0o755 {
		t.Fatalf("plugin ConfigMap defaultMode must be 0755, got %v", mode)
	}
}

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

	if job.GenerateName != attempt.Name+"-" || job.Namespace != attempt.Namespace {
		t.Fatalf("job must generate from the attempt's name, got %s/%s", job.Namespace, job.GenerateName)
	}
	if job.Labels["harmostes.dev/attempt"] != attempt.Name {
		t.Fatalf("job must label the attempt claim: %+v", job.Labels)
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

// Pool-only named mounts must reach the per-Attempt Job (#311): a
// fork-maintenance plugin resolves to /plugins/<cm>/<name>.sh AND execs
// engine scripts under /workspace — neither existed on attempt Jobs, so
// prepare died in 12ms on every run of a UI-created fork-maintenance
// instance.
func TestBuildJobExtraConfigMapMounts(t *testing.T) {
	job := BuildJob(AttemptJobParams{
		Attempt:          jobTestAttempt(),
		WorkflowName:     "fork-maintenance-forgejo",
		Namespace:        "harmostes",
		PluginConfigMaps: []string{"harmostes-pr-review"},
		ExtraConfigMapMounts: []ConfigMapMount{
			{Name: "fork-scripts", MountPath: "/workspace/scripts"},
			{Name: "fork-defs", MountPath: "/workspace/forks", Mode: ptr.To(int32(0o644))},
		},
	})
	c := job.Spec.Template.Spec.Containers[0]

	var paths []string
	for _, m := range c.VolumeMounts {
		paths = append(paths, m.MountPath)
	}
	for _, want := range []string{"/plugins/harmostes-pr-review", "/workspace/scripts", "/workspace/forks"} {
		found := false
		for _, got := range paths {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("mount %s missing, have %v", want, paths)
		}
	}
	// Extra mounts carry the pool's per-mount modes: scripts 0755 (exec
	// targets — the #283 class), data 0644, exactly as the pool mounts them.
	modes := map[string]int32{}
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.ConfigMap != nil && v.ConfigMap.DefaultMode != nil {
			modes[v.Name] = *v.ConfigMap.DefaultMode
		}
	}
	if modes["extra-cm-fork-scripts"] != 0o755 {
		t.Errorf("fork-scripts mode = %o, want 755 — non-executable scripts are the #283 regression", modes["extra-cm-fork-scripts"])
	}
	if modes["extra-cm-fork-defs"] != 0o644 {
		t.Errorf("fork-defs mode = %o, want 644 (must match the pool's mount)", modes["extra-cm-fork-defs"])
	}
}

// ADR-0008 decision 2: runs don't die by default. Empty runBound sets the
// 2h wedged-run reaper (a real review finishes in minutes; a 2h-old run is
// hung). A finite bound is honored verbatim; "0" is truly unlimited.
func TestBuildJobRunBound(t *testing.T) {
	// Default: the 2h wedged-run reaper.
	job := BuildJob(AttemptJobParams{Attempt: jobTestAttempt(), WorkflowName: "w", Namespace: "harmostes", Image: "img"})
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != int64((v1alpha1.DefaultRunBound).Seconds()) {
		t.Errorf("empty runBound → 2h reaper, got %v", job.Spec.ActiveDeadlineSeconds)
	}

	// Finite bound: honored.
	at := jobTestAttempt()
	at.Spec.RunBound = "90m"
	job = BuildJob(AttemptJobParams{Attempt: at, WorkflowName: "w", Namespace: "harmostes", Image: "img"})
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 5400 {
		t.Errorf("runBound 90m → activeDeadlineSeconds 5400, got %v", job.Spec.ActiveDeadlineSeconds)
	}

	// Explicit "0": truly unlimited.
	at.Spec.RunBound = "0"
	job = BuildJob(AttemptJobParams{Attempt: at, WorkflowName: "w", Namespace: "harmostes", Image: "img"})
	if job.Spec.ActiveDeadlineSeconds != nil {
		t.Errorf("runBound \"0\" must set no deadline, got %ds", *job.Spec.ActiveDeadlineSeconds)
	}

	// Malformed bound: degrade to the reaper (never unlimited by surprise).
	at.Spec.RunBound = "not-a-duration"
	job = BuildJob(AttemptJobParams{Attempt: at, WorkflowName: "w", Namespace: "harmostes", Image: "img"})
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != int64((v1alpha1.DefaultRunBound).Seconds()) {
		t.Errorf("malformed runBound must degrade to the 2h reaper, got %v", job.Spec.ActiveDeadlineSeconds)
	}
}

// #336: spec.cache mounts the shared toolchain-cache PVC and points the
// toolchains at namespaced subpaths — warm GOCACHE/GOMODCACHE is the
// difference between a review fitting its budget and dying at it.
func TestBuildJobCacheMounts(t *testing.T) {
	at := jobTestAttempt()
	at.Spec.Cache = &v1alpha1.CacheSpec{PVC: "harmostes-toolchain-cache", Go: true, NPM: true}
	job := BuildJob(AttemptJobParams{Attempt: at, WorkflowName: "w", Namespace: "harmostes", Image: "img"})

	volNames := map[string]bool{}
	for _, v := range job.Spec.Template.Spec.Volumes {
		volNames[v.Name] = true
	}
	if !volNames["toolchain-cache"] {
		t.Fatal("cache PVC volume missing")
	}
	env := map[string]string{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["GOCACHE"] != "/toolchain-cache/go-build" || env["GOMODCACHE"] != "/toolchain-cache/go-mod" {
		t.Errorf("Go cache env missing: %v", env)
	}
	if env["npm_config_cache"] != "/toolchain-cache/npm" {
		t.Errorf("npm cache env missing: %v", env)
	}

	// No cache declared: none of it appears.
	job = BuildJob(AttemptJobParams{Attempt: jobTestAttempt(), WorkflowName: "w", Namespace: "harmostes", Image: "img"})
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "GOCACHE" {
			t.Error("GOCACHE must not be set without spec.cache")
		}
	}
}
