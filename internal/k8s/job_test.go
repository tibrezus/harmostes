package k8s

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sigsyaml "sigs.k8s.io/yaml"

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

// #441: every Attempt Job clones the agents repo fresh (sync-skills init
// container) — served skills move at ATTEMPT granularity, not pool-pod
// restart. Without it the workflow's --skill /skills/... path resolves to
// nothing inside the attempt and agents run on the task prompt alone (the
// skill's methodology detail never reached a single production review run).
func envValue(env []corev1.EnvVar, name string) string {
	for _, e := range env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// #407: skills.repo (values → HARMOSTES_SKILLS_REPO env) is operator-
// controlled data that used to be fmt.Sprintf'd UNQUOTED into the sync-skills
// `sh -c` string — a crafted value executed arbitrary shell in every
// per-Attempt Job pod (which carries the fleet's forge credentials). The
// fix is env indirection: the command is a constant referencing
// "$HARMOSTES_SKILLS_REPO"; the value travels as env data, which the shell
// never re-parses as operators. This test pins that property against
// every injection shape that mattered: command separators, command
// substitution, backticks, redirection, and newline smuggling.
func TestSkillsSyncCommandInjectionSafe(t *testing.T) {
	hostile := []string{
		"https://example.com/x; rm -rf /",
		"https://example.com/$(touch /pwned)",
		"https://example.com/x`touch /pwned2`",
		"https://example.com/x > /etc/passwd",
		"https://example.com/x\nrm -rf /",
	}
	for _, repo := range hostile {
		t.Setenv("HARMOSTES_SKILLS_REPO", repo)
		job := BuildJob(AttemptJobParams{
			Attempt:      jobTestAttempt(),
			WorkflowName: "pr-review-harmostes",
			Namespace:    "harmostes",
			Image:        "ghcr.io/tibrezus/harmostes-worker:1.2.3",
		})
		ic := job.Spec.Template.Spec.InitContainers[0]
		cmd := strings.Join(ic.Command, " ")
		for _, payload := range []string{"rm -rf", "touch /pwned", "`touch", "> /etc/passwd"} {
			if strings.Contains(cmd, payload) {
				t.Fatalf("hostile repo %q: payload %q reached the command string: %q", repo, payload, cmd)
			}
		}
		if v := envValue(ic.Env, "HARMOSTES_SKILLS_REPO"); v != repo {
			t.Fatalf("hostile repo must travel verbatim as env data, got env=%q", v)
		}
		// The literal contract: one quoted env reference, no %s hole left.
		if !strings.Contains(cmd, `--depth 1 "$HARMOSTES_SKILLS_REPO" /tmp/agents`) {
			t.Fatalf("command must reference the env var quoted, got %q", cmd)
		}
	}
}

// #408 item 9: a PINNED revision rides the same env-indirection contract —
// hostile payload in HARMOSTES_SKILLS_REV must reach the shell only as env
// data (inert), with the command referencing "$HARMOSTES_SKILLS_REV"
// quoted inside the fetch/checkout segment.
func TestSkillsRevPinnedCheckoutInjectionSafe(t *testing.T) {
	hostile := []string{
		"main; rm -rf /",
		"$(touch /pwned-rev)",
		"9b63a39c36cb`touch /pwned3`",
		"--upload-pack=evil",
		"deadbeef\nrm -rf /",
	}
	for _, rev := range hostile {
		t.Setenv("HARMOSTES_SKILLS_REV", rev)
		job := BuildJob(AttemptJobParams{
			Attempt:      jobTestAttempt(),
			WorkflowName: "pr-review-harmostes",
			Namespace:    "harmostes",
			Image:        "ghcr.io/tibrezus/harmostes-worker:1.2.3",
		})
		ic := job.Spec.Template.Spec.InitContainers[0]
		cmd := strings.Join(ic.Command, " ")
		for _, payload := range []string{"rm -rf", "touch /pwned", "`touch", "--upload-pack=evil"} {
			if strings.Contains(cmd, payload) {
				t.Fatalf("hostile rev %q: payload %q reached the command string: %q", rev, payload, cmd)
			}
		}
		if v := envValue(ic.Env, "HARMOSTES_SKILLS_REV"); v != rev {
			t.Fatalf("hostile rev must travel verbatim as env data, got env=%q", v)
		}
		// Positive shape: the shell must reference the rev env var QUOTED in
		// the fetch/checkout segment — an env-only value the command never
		// reads would silently unpin the fleet.
		for _, need := range []string{
			`[ -z "$HARMOSTES_SKILLS_REV" ]`,
			`fetch --depth 1 origin "$HARMOSTES_SKILLS_REV"`,
			`checkout --detach FETCH_HEAD`,
		} {
			if !strings.Contains(cmd, need) {
				t.Fatalf("command must carry %q, got %q", need, cmd)
			}
		}
	}
}

// #408 item 9: the chart template and the Go side MUST stay byte-identical
// (#407 discipline, now mechanically pinned). The worker-pool template's
// sync-skills command is a single-line YAML flow scalar — parse it and
// compare against skillsSyncCommand's script.
func TestSyncSkillsCommandMatchesChartTemplate(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "chart", "templates", "worker-pool.yaml"))
	if err != nil {
		t.Fatalf("read chart template: %v", err)
	}
	var cmdLine string
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, `command: ["sh", "-c", "git clone --depth 1`) {
			cmdLine = trimmed
			break
		}
	}
	if cmdLine == "" {
		t.Fatal("worker-pool.yaml carries no sync-skills command line — template moved? update this test's anchor")
	}
	// Strip the mapping key — the flow sequence parses standalone.
	flow := strings.TrimPrefix(cmdLine, "command: ")
	var parsed []string
	if err := sigsyaml.Unmarshal([]byte(flow), &parsed); err != nil {
		t.Fatalf("parse chart command scalar: %v", err)
	}
	if len(parsed) != 3 || parsed[0] != "sh" || parsed[1] != "-c" {
		t.Fatalf("chart command shape = %#v, want [sh -c <script>]", parsed)
	}
	want := skillsSyncCommand()[2]
	if parsed[2] != want {
		t.Errorf("chart and Go sync-skills literals DIVERGED:\nchart: %s\ngo:    %s", parsed[2], want)
	}
}

func TestBuildJobServesFreshSkills(t *testing.T) {
	job := BuildJob(AttemptJobParams{
		Attempt:        jobTestAttempt(),
		WorkflowName:   "pr-review-harmostes",
		Namespace:      "harmostes",
		Image:          "ghcr.io/tibrezus/harmostes-worker:1.2.3",
		ServiceAccount: "harmostes-controller",
	})
	pod := job.Spec.Template.Spec

	inits := pod.InitContainers
	if len(inits) != 1 || inits[0].Name != "sync-skills" {
		t.Fatalf("attempt pods must run exactly one sync-skills init container, got %+v", inits)
	}
	ic := inits[0]
	if ic.Image != "ghcr.io/tibrezus/harmostes-worker:1.2.3" {
		t.Fatalf("sync-skills must use the run container's image (same fj/gh tooling), got %q", ic.Image)
	}
	cmd := strings.Join(ic.Command, " ")
	// #407: the repo URL is passed as ENV DATA, never interpolated into
	// the shell string — the command references it as "$HARMOSTES_SKILLS_REPO".
	// Env expansion results are not re-parsed as shell operators (POSIX),
	// so any metacharacters in the value are inert.
	if !strings.Contains(cmd, `git clone --depth 1 "$HARMOSTES_SKILLS_REPO" /tmp/agents`) {
		t.Fatalf("sync-skills must clone via the env-indirected repo URL, got %q", cmd)
	}
	if envVal := envValue(ic.Env, "HARMOSTES_SKILLS_REV"); envVal != "" {
		t.Fatalf("unset HARMOSTES_SKILLS_REV must resolve to empty (track default branch), got %q", envVal)
	}
	if !strings.Contains(cmd, `[ -z "$HARMOSTES_SKILLS_REV" ]`) || !strings.Contains(cmd, `fetch --depth 1 origin "$HARMOSTES_SKILLS_REV"`) {
		t.Fatalf("sync-skills must carry the pinned-rev fetch/checkout segment referencing the env var quoted, got %q", cmd)
	}
	if strings.Contains(cmd, DefaultSkillsRepo) {
		t.Fatalf("#407: the repo URL must never be spliced into the command string, got %q", cmd)
	}
	if strings.Contains(cmd, DefaultSkillsRepo) {
		t.Fatalf("#407: the repo URL must never be spliced into the command string, got %q", cmd)
	}
	if !strings.Contains(cmd, "cp -r /tmp/agents/skills/. /skills/") || !strings.Contains(cmd, ".manifest") {
		t.Fatalf("sync-skills must copy skills/ and write the sha256 manifest, got %q", cmd)
	}
	mountsSkills := false
	for _, m := range ic.VolumeMounts {
		if m.Name == "skills" && m.MountPath == "/skills" {
			mountsSkills = true
		}
	}
	if !mountsSkills {
		t.Fatalf("sync-skills must mount /skills: %+v", ic.VolumeMounts)
	}

	// The skills volume exists and the run container sees it at /skills.
	var skillsVol *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == "skills" {
			skillsVol = &pod.Volumes[i]
		}
	}
	if skillsVol == nil || skillsVol.EmptyDir == nil {
		t.Fatalf("skills volume must be a per-Job emptyDir, volumes: %+v", pod.Volumes)
	}
	run := pod.Containers[0]
	seen := false
	for _, m := range run.VolumeMounts {
		if m.Name == "skills" && m.MountPath == "/skills" {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("run container must mount /skills (the --skill path resolves inside the attempt): %+v", run.VolumeMounts)
	}
}

// #441: a forked fleet overrides values.skills.repo; the chart passes it to
// the dispatcher as HARMOSTES_SKILLS_REPO and BuildJob must clone THAT repo,
// not the default.
func TestBuildJobSkillsRepoOverride(t *testing.T) {
	t.Setenv("HARMOSTES_SKILLS_REPO", "https://git.rezus.cloud/tibrezus/agents.git")
	job := BuildJob(AttemptJobParams{
		Attempt: jobTestAttempt(), WorkflowName: "pr-review-harmostes", Namespace: "harmostes",
		Image: "img", ServiceAccount: "sa",
	})
	// #407: the override travels as env data (see skillsSyncCommand) — the
	// clone reads it at runtime via "$HARMOSTES_SKILLS_REPO".
	ic := job.Spec.Template.Spec.InitContainers[0]
	if v := envValue(ic.Env, "HARMOSTES_SKILLS_REPO"); v != "https://git.rezus.cloud/tibrezus/agents.git" {
		t.Fatalf("skills repo override must reach sync-skills as env data, got %q", v)
	}
	if strings.Contains(strings.Join(ic.Command, " "), "git.rezus.cloud") {
		t.Fatalf("#407: override must not be spliced into the command string: %q", ic.Command)
	}
	if strings.Contains(strings.Join(ic.Command, " "), "github.com/tibrezus/agents") {
		t.Fatalf("default repo leaked into the command string: %q", ic.Command)
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

// The Job wall clock follows the workflow's runBound (#348/#333): a
// configured bound reaches ActiveDeadlineSeconds; zero falls back to the
// fleet default — the deadline the gate's DispatchTimeout margin is
// validated against.
func TestBuildJobRunBoundDeadline(t *testing.T) {
	at := &v1alpha1.Attempt{ObjectMeta: metav1.ObjectMeta{Name: "attempt-w", Namespace: "default"}}
	base := AttemptJobParams{Attempt: at, WorkflowName: "wf", Namespace: "default", Image: "img"}

	if got := base.runBoundSeconds(); got != v1alpha1.OneShotRunBound {
		t.Fatalf("zero runBound must fall back to OneShotRunBound, got %s", got)
	}
	p := base
	p.RunBound = 45 * time.Minute
	if got := p.runBoundSeconds(); got != 45*time.Minute {
		t.Fatalf("configured runBound must reach the deadline, got %s", got)
	}
	job := BuildJob(p)
	if got := *job.Spec.ActiveDeadlineSeconds; got != 2700 {
		t.Fatalf("ActiveDeadlineSeconds = %d, want 2700 (45m)", got)
	}
	job = BuildJob(base)
	if got := *job.Spec.ActiveDeadlineSeconds; got != 1800 {
		t.Fatalf("default ActiveDeadlineSeconds = %d, want 1800 (30m)", got)
	}
}

// #336: the shared cache mounts per the workflow's declared spec — one RWX
// PVC, SubPath-isolated per workflow, flag-gated tool env. Nil/PVC-less cache
// renders a byte-identical Job (no volume, no env).
func TestBuildJobCacheMounts(t *testing.T) {
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
	base := func(cache *v1alpha1.CacheSpec) *batchv1.Job {
		return BuildJob(AttemptJobParams{Attempt: jobTestAttempt(), WorkflowName: "pr-review-harmostes", Namespace: "harmostes", Cache: cache})
	}

	// Declared cache: PVC volume + SubPath isolation + Go env.
	job := base(&v1alpha1.CacheSpec{PVC: "harmostes-worker-cache", Go: true})
	if m := hasVol(job, "cache"); m == nil {
		t.Fatal("cache volume missing")
	} else {
		if m.SubPath != "pr-review-harmostes" {
			t.Fatalf("cache SubPath must isolate per workflow, got %q", m.SubPath)
		}
		if m.MountPath != "/cache" {
			t.Fatalf("cache MountPath = %q, want /cache", m.MountPath)
		}
	}
	env := envMap(job)
	if env["GOCACHE"] != "/cache/go/build" || env["GOMODCACHE"] != "/cache/go/mod" {
		t.Fatalf("Go cache env missing: %v", env)
	}

	// npm + git flags gate their own env; go env absent when flag unset.
	job = base(&v1alpha1.CacheSpec{PVC: "c", NPM: true, Git: true})
	env = envMap(job)
	if env["npm_config_cache"] != "/cache/npm" {
		t.Fatalf("npm cache env missing: %v", env)
	}
	if env["XDG_CACHE_HOME"] != "/cache/xdg" {
		t.Fatalf("git XDG env missing: %v", env)
	}
	if _, ok := env["GOCACHE"]; ok {
		t.Fatalf("Go env must be flag-gated: %v", env)
	}

	// No cache, or PVC-less cache: byte-identical Job (no volume, no env).
	for name, cache := range map[string]*v1alpha1.CacheSpec{"nil": nil, "pvc-less": {Go: true}} {
		job = base(cache)
		if hasVol(job, "cache") != nil {
			t.Fatalf("%s: cache volume must not render", name)
		}
		if _, ok := envMap(job)["GOCACHE"]; ok {
			t.Fatalf("%s: cache env must not render", name)
		}
	}
}

// #336: the agent-visible wall equals the Job wall — one source, so the run
// can pace itself against exactly what will kill it.
func TestBuildJobWallSecondsMatchesDeadline(t *testing.T) {
	job := BuildJob(AttemptJobParams{Attempt: jobTestAttempt(), WorkflowName: "w", Namespace: "ns", RunBound: 45 * time.Minute})
	var wall string
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "HARMOSTES_WALL_SECONDS" {
			wall = e.Value
		}
	}
	if wall == "" {
		t.Fatal("HARMOSTES_WALL_SECONDS missing")
	}
	if ads := job.Spec.ActiveDeadlineSeconds; ads == nil || wall != strconv.FormatInt(*ads, 10) {
		t.Fatalf("wall env %q must equal ActiveDeadlineSeconds %v", wall, *ads)
	}
	// Default bound (0) → OneShotRunBound, still consistent.
	job = BuildJob(AttemptJobParams{Attempt: jobTestAttempt(), WorkflowName: "w", Namespace: "ns"})
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "HARMOSTES_WALL_SECONDS" && e.Value != strconv.FormatInt(int64(v1alpha1.OneShotRunBound/time.Second), 10) {
			t.Fatalf("default wall = %s, want %d", e.Value, int64(v1alpha1.OneShotRunBound/time.Second))
		}
	}
}

func TestAttachmentSubPath(t *testing.T) {
	// C2: the scope picks the sharing dimension; identity-unavailable
	// scopes are INERT (a repository attachment on a non-PR run shares
	// nothing, by design).
	cases := []struct {
		name     string
		scope    string
		workflow string
		repo     string
		attempt  string
		want     string
		ok       bool
	}{
		{"workflow default", "", "wf-a", "o/r#1", "att-1", "wf-a", true},
		{"explicit workflow", "workflow", "wf-a", "o/r#1", "att-1", "wf-a", true},
		{"repository shares by repo hash", "repository", "wf-a", "git.rezus.cloud/tibrez/rhesadox", "att-1", "git.rezus.cloud-tibrez-rhesadox-" + "d782cf64", true},
		{"repository inert without repo", "repository", "wf-a", "", "att-1", "", false},
		{"attempt is private", "attempt", "wf-a", "o/r#1", "att-1", "attempt-att-1", true},
		{"attempt inert without attempt", "attempt", "wf-a", "o/r#1", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := AttachmentSubPath(tc.scope, tc.workflow, tc.repo, tc.attempt)
			if ok != tc.ok {
				t.Fatalf("ok=%v want %v", ok, tc.ok)
			}
			if tc.ok && !strings.HasPrefix(got, tc.want) {
				t.Fatalf("subpath %q, want prefix %q", got, tc.want)
			}
		})
	}
	// the repository hash must be the collision-proof LineageDir scheme
	_, ok := AttachmentSubPath("repository", "wf", "a_b/c", "x")
	_, ok2 := AttachmentSubPath("repository", "wf", "a/b-c", "x")
	if ok && ok2 {
		sub1, _ := AttachmentSubPath("repository", "wf", "a_b/c", "x")
		sub2, _ := AttachmentSubPath("repository", "wf", "a/b-c", "x")
		if sub1 == sub2 {
			t.Fatalf("sanitizer colliders must not share a subpath: %q", sub1)
		}
	}
}

func TestBuildJobAttachments(t *testing.T) {
	// C2 rendering: one volume per attachment, SubPath from the scope,
	// default mountPath /attachments/<name>, shared PVC deduped by claim.
	p := AttemptJobParams{
		WorkflowName: "pr-review-rhesadox",
		AttemptName:  "attempt-abc",
		Attempt:      &v1alpha1.Attempt{ObjectMeta: metav1.ObjectMeta{Name: "attempt-abc", Namespace: "default"}},
		TriggerRepo:  "git.rezus.cloud/tibrez/rhesadox",
		Attachments: []v1alpha1.SharedAttachment{
			{Name: "sol-pi", Scope: v1alpha1.AttachmentScopeRepository, PVC: "harmostes-worker-sessions"},
			{Name: "scratch", Scope: v1alpha1.AttachmentScopeAttempt, PVC: "harmostes-worker-sessions", MountPath: "/scratch"},
			{Name: "nolabel", PVC: ""}, // no backend → skipped
		},
	}
	job := BuildJob(p)
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range job.Spec.Template.Spec.Containers[0].VolumeMounts {
		mounts[m.Name] = m
	}
	if m := mounts["attach-sol-pi"]; m.SubPath != "git.rezus.cloud-tibrez-rhesadox-d782cf64" || m.MountPath != "/attachments/sol-pi" {
		t.Fatalf("sol-pi mount: %+v", m)
	}
	if m := mounts["attach-scratch"]; m.SubPath != "attempt-attempt-abc" || m.MountPath != "/scratch" {
		t.Fatalf("scratch mount: %+v", m)
	}
	claims := map[string]string{}
	for _, v := range job.Spec.Template.Spec.Volumes {
		if strings.HasPrefix(v.Name, "attach-") {
			claims[v.Name] = v.PersistentVolumeClaim.ClaimName
		}
	}
	if claims["attach-sol-pi"] != "harmostes-worker-sessions" || claims["attach-scratch"] != "harmostes-worker-sessions" {
		t.Fatalf("both attachments must ride the sessions claim: %+v", claims)
	}
	for _, m := range job.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == "attach-nolabel" {
			t.Fatalf("backend-less attachment must be skipped: %+v", m)
		}
	}
}
