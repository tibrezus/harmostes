package k8s

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/tibrezus/harmostes/api/v1alpha1"
)

// ADR-0010 follow-up: the persistent lineage claim mounts per the
// workflow's declared spec — RWX PVC, SubPath per workflow (project
// isolation on one claim), sessions-root + TTL env. Nil or PVC-less
// sessions = ephemeral /tmp, byte-identical Job (no volume, no env).
func TestBuildJobSessionsMounts(t *testing.T) {
	envMap := func(job *batchv1.Job) map[string]string {
		m := map[string]string{}
		for _, e := range job.Spec.Template.Spec.Containers[0].Env {
			m[e.Name] = e.Value
		}
		return m
	}
	hasVol := func(job *batchv1.Job, name string) *corev1.VolumeMount {
		cms := job.Spec.Template.Spec.Containers[0].VolumeMounts
		for i := range cms {
			if cms[i].Name == name {
				return &cms[i]
			}
		}
		return nil
	}
	base := func(s *v1alpha1.SessionsSpec) *batchv1.Job {
		return BuildJob(AttemptJobParams{Attempt: jobTestAttempt(), WorkflowName: "pr-review-harmostes", Namespace: "harmostes", Sessions: s})
	}

	// Declared sessions: PVC volume + SubPath isolation + env pair.
	job := base(&v1alpha1.SessionsSpec{PVC: "harmostes-worker-sessions"})
	if m := hasVol(job, "sessions"); m == nil {
		t.Fatal("sessions volume missing")
	} else {
		if m.SubPath != "pr-review-harmostes" {
			t.Fatalf("sessions SubPath must isolate per workflow (project isolation), got %q", m.SubPath)
		}
		if m.MountPath != "/sessions" {
			t.Fatalf("sessions MountPath = %q, want /sessions", m.MountPath)
		}
	}
	env := envMap(job)
	if env["HARMOSTES_PI_SESSIONS"] != "/sessions" {
		t.Fatalf("sessions root env missing: %v", env)
	}
	if env["HARMOSTES_SESSIONS_TTL"] != "336h" {
		t.Fatalf("TTL default must arm the janitor, got %q", env["HARMOSTES_SESSIONS_TTL"])
	}

	// Explicit TTL flows through.
	job = base(&v1alpha1.SessionsSpec{PVC: "c", TTL: "24h"})
	if envMap(job)["HARMOSTES_SESSIONS_TTL"] != "24h" {
		t.Fatalf("explicit TTL must flow, got %v", envMap(job))
	}

	// No sessions, or PVC-less: byte-identical Job (no volume, no env).
	for name, s := range map[string]*v1alpha1.SessionsSpec{"nil": nil, "pvc-less": {TTL: "24h"}} {
		job = base(s)
		if hasVol(job, "sessions") != nil {
			t.Fatalf("%s: sessions volume must not render", name)
		}
		for _, k := range []string{"HARMOSTES_PI_SESSIONS", "HARMOSTES_SESSIONS_TTL"} {
			if _, ok := envMap(job)[k]; ok {
				t.Fatalf("%s: %s must not render", name, k)
			}
		}
	}
}
