package k8s

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// #707 gate half 2: uid separation renders ONLY when the uid is set — the
// historical (uid 0) Job shape stays byte-identical. Mutation probe: removing
// the `if p.RunUID > 0` gate renders a securityContext into the uid-0 Job and
// this test fails.
func TestBuildJobRunUIDInertWhenZero(t *testing.T) {
	job := BuildJob(AttemptJobParams{
		Attempt:      jobTestAttempt(),
		WorkflowName: "pr-review-harmostes",
		Namespace:    "harmostes",
		RunUID:       0,
	})
	pod := job.Spec.Template.Spec
	if pod.SecurityContext != nil {
		t.Fatal("uid-0 Job must carry no pod securityContext (byte-identical render)")
	}
	run := pod.Containers[0]
	if run.SecurityContext != nil {
		t.Fatal("uid-0 run container must carry no securityContext")
	}
	for _, c := range pod.InitContainers {
		if c.Name == "session-ownership" {
			t.Fatal("uid-0 Job must not render the ownership init")
		}
	}
	for _, e := range run.Env {
		if e.Name == "HOME" || e.Name == "XDG_CONFIG_HOME" {
			t.Fatalf("uid-0 Job must not override %s", e.Name)
		}
	}
	for _, v := range pod.Volumes {
		if v.Name == "home" {
			t.Fatal("uid-0 Job must not render the home emptyDir")
		}
	}
}

// The uid flip's shape: pod runAsNonRoot + fsGroup, run container uid,
// root-run ownership init BEFORE skills, HOME seeded, sessions migrated.
func TestBuildJobRunUIDArmsSeparation(t *testing.T) {
	job := BuildJob(AttemptJobParams{
		Attempt:      jobTestAttempt(),
		WorkflowName: "pr-review-harmostes",
		Namespace:    "harmostes",
		Sessions:     &v1alpha1.SessionsSpec{PVC: "harmostes-worker-sessions"},
		RunUID:       10001,
		FSGroup:      10001,
	})
	pod := job.Spec.Template.Spec

	// runAsNonRoot is NOT pod-level: it applies to every container and would
	// refuse the root-run ownership init (live-proven defect).
	if pod.SecurityContext != nil && pod.SecurityContext.RunAsNonRoot != nil {
		t.Fatal("pod-level runAsNonRoot must stay unset — it would refuse the root ownership init")
	}
	if pod.SecurityContext == nil {
		t.Fatal("uid flip must render the pod securityContext (fsGroup)")
	}
	if pod.SecurityContext.FSGroup == nil || *pod.SecurityContext.FSGroup != 10001 {
		t.Fatal("fsGroup must carry the claim group")
	}

	run := pod.Containers[0]
	if run.SecurityContext == nil || run.SecurityContext.RunAsUser == nil || *run.SecurityContext.RunAsUser != 10001 {
		t.Fatal("run container must run as the target uid")
	}
	if run.SecurityContext.RunAsNonRoot == nil || !*run.SecurityContext.RunAsNonRoot {
		t.Fatal("run container must declare runAsNonRoot (the uid is non-root)")
	}
	if run.SecurityContext.AllowPrivilegeEscalation == nil || *run.SecurityContext.AllowPrivilegeEscalation {
		t.Fatal("privilege escalation must be disallowed")
	}
	env := map[string]string{}
	for _, e := range run.Env {
		env[e.Name] = e.Value
	}
	if env["HOME"] != "/home/10001" {
		t.Fatalf("HOME must be seeded off the claim, got %q", env["HOME"])
	}

	// Init ordering: ownership BEFORE skills (the tree and identity must be
	// migrated before the later containers use them).
	initNames := []string{}
	for _, c := range pod.InitContainers {
		initNames = append(initNames, c.Name)
	}
	ownershipIdx := -1
	skillsIdx := -1
	for i, n := range initNames {
		if n == "session-ownership" {
			ownershipIdx = i
		}
		if n == "sync-skills" {
			skillsIdx = i
		}
	}
	if ownershipIdx < 0 {
		t.Fatal("ownership init missing")
	}
	if skillsIdx >= 0 && ownershipIdx > skillsIdx {
		t.Fatalf("ownership init must precede skills (got %v)", initNames)
	}

	// The init runs as root (the tree's owner) — it cannot chown as the target uid.
	var init *corev1.Container
	for i := range pod.InitContainers {
		if pod.InitContainers[i].Name == "session-ownership" {
			init = &pod.InitContainers[i]
		}
	}
	if init.SecurityContext == nil || init.SecurityContext.RunAsUser == nil || *init.SecurityContext.RunAsUser != 0 {
		t.Fatal("ownership init must run as root to migrate a root-owned tree")
	}
	cmd := strings.Join(init.Command, " ")
	for _, want := range []string{"chown -R", "/sessions", "git config --file", "HARMOSTES_RUN_UID"} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("ownership command missing %q", want)
		}
	}
	// SCOPE: the init must mount the WORKFLOW LEAF (SubPath), not the claim root.
	// A root mount makes `chown -R /sessions` migrate every workflow's tree on
	// the shared claim — the cross-workflow write this boundary exists to stop.
	// Live-proven: a SubPath mount to a missing leaf is created empty by kubelet,
	// so a first run's leaf is safe.
	for _, m := range init.VolumeMounts {
		if m.Name == "sessions" && m.SubPath != "pr-review-harmostes" {
			t.Fatalf("ownership init must mount the workflow leaf, got SubPath %q", m.SubPath)
		}
	}
	sessionsMount := false
	for _, m := range init.VolumeMounts {
		if m.Name == "sessions" {
			sessionsMount = true
		}
	}
	if !sessionsMount {
		t.Fatal("ownership init must mount the sessions claim")
	}
	// The home must NOT live on the sessions claim (janitor newest-file clock).
	if strings.Contains(cmd, "/sessions/.homes") {
		t.Fatal("home must not be seeded inside the sessions claim")
	}
}

// ptr import sanity: fsGroup nil when zero.
func TestPodSecurityContextNilWhenZero(t *testing.T) {
	if sc := podSecurityContext(AttemptJobParams{RunUID: 0}); sc != nil {
		t.Fatal("zero uid must produce nil pod security context")
	}
	if sc := podSecurityContext(AttemptJobParams{RunUID: 10001, FSGroup: 0}); sc == nil || sc.FSGroup != nil {
		t.Fatal("fsGroup must stay unset when zero")
	}
	_ = ptr.To(int64(1))
}

// #707 regression class: the uid is FLEET-WIDE, so a plugin-only workflow with
// NO sessions claim must still render a usable uid Job — a per-pod home, the
// git identity seed, and NO sessions mount (a blind chown would fail). Live
// consequence before this fix: dev's fork-maintenance canary could never
// dispatch under uid 10001.
func TestBuildJobRunUIDClaimlessPluginWorkflow(t *testing.T) {
	job := BuildJob(AttemptJobParams{
		Attempt:      jobTestAttempt(),
		WorkflowName: "fork-maintenance-canary",
		Namespace:    "harmostes-dev",
		RunUID:       10001,
		FSGroup:      10001,
	})
	pod := job.Spec.Template.Spec
	hasHome := false
	for _, v := range pod.Volumes {
		if v.Name == "home" {
			hasHome = true
		}
	}
	if !hasHome {
		t.Fatal("a uid Job must always seed a per-pod home, claim or not")
	}
	run := pod.Containers[0]
	env := map[string]string{}
	for _, e := range run.Env {
		env[e.Name] = e.Value
	}
	if env["HOME"] != "/home/10001" {
		t.Fatalf("HOME must be seeded, got %q", env["HOME"])
	}
	homeMounted := false
	for _, m := range run.VolumeMounts {
		if m.Name == "home" {
			homeMounted = true
		}
	}
	if !homeMounted {
		t.Fatal("HOME points at a path with no /home mount — the run has no seeded home")
	}
	var init *corev1.Container
	for i := range pod.InitContainers {
		if pod.InitContainers[i].Name == "session-ownership" {
			init = &pod.InitContainers[i]
		}
	}
	if init == nil {
		t.Fatal("a uid Job must render the ownership init (it seeds the identity)")
	}
	for _, m := range init.VolumeMounts {
		if m.Name == "sessions" {
			t.Fatalf("claim-less Job must not mount the sessions claim for the chown: %+v", m)
		}
	}
	cmd := strings.Join(init.Command, " ")
	if !strings.Contains(cmd, "[ -d /sessions ]") {
		t.Fatal("the chown must be guarded: a claim-less run has no /sessions")
	}
}
