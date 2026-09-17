package k8s

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tibrezus/harmostes/internal/sessionstore"
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

// DefaultSkillsRepo — the served-skills source of truth (the agents repo).
// Must stay equal to the chart's skills.repo default (values.yaml): the pool
// Deployment's sync-skills init container and the per-Attempt Jobs clone the
// SAME repo, or pool-served and attempt-served skills diverge (#441).
// HARMOSTES_SKILLS_REPO overrides both when the fleet forks the agents repo.
const DefaultSkillsRepo = "https://github.com/tibrezus/agents.git"

// SkillsRepo resolves the clone source for the served skills: the env set by
// the chart (values.skills.repo passed through), else the shared default.
func SkillsRepo() string {
	if r := os.Getenv("HARMOSTES_SKILLS_REPO"); r != "" {
		return r
	}
	return DefaultSkillsRepo
}

// SkillsRev resolves the PINNED skills revision (#408 item 9): the env set
// by the chart (values.skills.rev passed through), else "" = track the
// repo's default branch. Empty is the escape hatch — the fleet pins by
// default and the skills-bump workflow owns keeping the pin fresh, so an
// operator who wants to live on main says so explicitly.
func SkillsRev() string {
	return os.Getenv("HARMOSTES_SKILLS_REV")
}

// skillsSyncCommand mirrors the chart's sync-skills init container byte
// for byte: clone the agents repo fresh, copy skills/, write the sha256
// manifest the pool's startup check consumes. Every attempt therefore
// serves the pinned skills rev AS OF THE ATTEMPT (#408 item 9) — the
// owner directive "every update should be available in the runtime" at
// the granularity the bump bot actually moves (the values pin).
//
// #407: the repo URL is deliberately NOT interpolated here. It travels as
// HARMOSTES_SKILLS_REPO env data (set by the chart / SkillsRepo()), and the
// script references it quoted as "$HARMOSTES_SKILLS_REPO": POSIX shells
// never re-parse expansion results as operators, so a crafted skills.repo
// value cannot execute shell in the pod. The chart template carries the
// identical literal — keep them in sync.
//
// #408 item 9: HARMOSTES_SKILLS_REV pins the served revision. Non-empty →
// shallow-fetch that exact commit and check it out (clone --branch cannot
// take an arbitrary SHA); empty → the clone's default branch stands. The
// rev travels as env data too — same injection argument as the repo URL.
func skillsSyncCommand() []string {
	return []string{"sh", "-c", `git clone --depth 1 "$HARMOSTES_SKILLS_REPO" /tmp/agents && { [ -z "$HARMOSTES_SKILLS_REV" ] || { git -C /tmp/agents fetch --depth 1 origin "$HARMOSTES_SKILLS_REV" && git -C /tmp/agents checkout --detach FETCH_HEAD; }; } && mkdir -p /skills && cp -r /tmp/agents/skills/. /skills/ && { echo "[sync-skills] served skills revision: $(git -C /tmp/agents rev-parse HEAD)"; (find /skills -name 'SKILL.md' | sort | xargs -r sha256sum > /skills/.manifest) || echo "[sync-skills] manifest write failed (non-fatal)"; true; }`}
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

	// RunBound is the Job's ActiveDeadlineSeconds source (#348/#333):
	// the workflow's reviewReady.runBound. Zero means OneShotRunBound —
	// the fleet default the wall keeps when no workflow raises it.
	RunBound time.Duration

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

	// Cache mounts the workflow's declared shared caches (#336): a PVC at
	// /cache (SubPath per workflow — isolation on one RWX claim) and the
	// flag-gated tool env. Nil or PVC-less = no cache, byte-identical Job.
	Cache *v1alpha1.CacheSpec
	// Sessions mounts the persistent pi-session lineage claim (ADR-0010
	// follow-up): an RWX PVC at /sessions, SubPath per workflow. Nil or
	// PVC-less = ephemeral /tmp sessions, byte-identical Job.
	Sessions *v1alpha1.SessionsSpec
	// Attachments render the workflow's scoped shared storage components
	// (C2): one mount per attachment, SubPath derived from the scope.
	Attachments []v1alpha1.SharedAttachment
	// TriggerRepo/AttemptName feed the repository/attempt attachment
	// scopes. Empty repo makes a repository-scoped attachment inert (the
	// run is not PR-shaped).
	TriggerRepo string
	AttemptName string
}

// AttachmentSubPath derives the SubPath for a scoped shared attachment:
// the scope picks the sharing dimension — workflow (the workflow's name),
// repository (the sanitized trigger repo, hashed like the session
// lineages), attempt (the attempt's own name). ok=false when the scope's
// identity is unavailable (a repository-scoped attachment on a
// non-PR-shaped run shares nothing — inert, by design).
func AttachmentSubPath(scope, workflow, repo, attempt string) (string, bool) {
	switch scope {
	case v1alpha1.AttachmentScopeRepository:
		if repo == "" {
			return "", false
		}
		sum := sha256.Sum256([]byte(repo))
		return fmt.Sprintf("%s-%s", sessionstore.SanitizeRepo(repo), hex.EncodeToString(sum[:4])), true
	case v1alpha1.AttachmentScopeAttempt:
		if attempt == "" {
			return "", false
		}
		return "attempt-" + attempt, true
	default: // workflow ("" too — the zero value)
		if workflow == "" {
			return "", false
		}
		return workflow, true
	}
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
// runBoundSeconds resolves the Job's wall clock: the workflow-configured
// bound, or the fleet default when unset.
func (p AttemptJobParams) runBoundSeconds() time.Duration {
	if p.RunBound > 0 {
		return p.RunBound
	}
	return v1alpha1.OneShotRunBound
}

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
		// The wall the run paces against (#336): the SAME effective bound
		// ActiveDeadlineSeconds enforces (one source — runBoundSeconds —
		// so they cannot disagree). The task contract reads this so the
		// agent can budget depth: "the review is written by minute 8"
		// needs a visible clock.
		{Name: "HARMOSTES_WALL_SECONDS", Value: strconv.FormatInt(int64(p.runBoundSeconds()/time.Second), 10)},
	}
	for _, kv := range p.ExtraEnv {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env = append(env, corev1.EnvVar{Name: k, Value: v})
		}
	}

	volumes := []corev1.Volume{
		{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		// Served skills (#441): the pool pod gets them from its sync-skills
		// init container at POD start; attempts run for minutes against a
		// repo that moves between restarts, so each Job clones fresh. The
		// workflow's --skill path (/skills/...) resolves inside the attempt.
		{Name: "skills", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	mounts := []corev1.VolumeMount{
		{Name: "workspace", MountPath: "/workspace"},
		{Name: "skills", MountPath: "/skills"},
	}
	// Shared caches (#336): one RWX PVC, SubPath per workflow — concurrent
	// review Jobs of the same repo share a warm GOCACHE (go's cache is
	// concurrency-safe), and workflows never thrash each other's dirs.
	// The tools create their leaf dirs on demand; the claim must be
	// writable by the pod (fsGroup / no root-squash is the claim owner's
	// concern — the chart's worker.cache template renders it right).
	if p.Cache != nil && p.Cache.PVC != "" {
		volumes = append(volumes, corev1.Volume{
			Name:         "cache",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: p.Cache.PVC}},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "cache", SubPath: p.WorkflowName, MountPath: "/cache"})
		if p.Cache.Go {
			env = append(env,
				corev1.EnvVar{Name: "GOCACHE", Value: "/cache/go/build"},
				corev1.EnvVar{Name: "GOMODCACHE", Value: "/cache/go/mod"},
			)
		}
		if p.Cache.NPM {
			env = append(env, corev1.EnvVar{Name: "npm_config_cache", Value: "/cache/npm"})
		}
		if p.Cache.Git {
			// git has no dedicated object cache for plain clones; the XDG
			// dir carries credential/socket/commit-graph state — cheap to
			// mount, harmless when idle.
			env = append(env, corev1.EnvVar{Name: "XDG_CACHE_HOME", Value: "/cache/xdg"})
		}
	}
	// Persistent session lineages (ADR-0010): one RWX PVC, SubPath per
	// workflow — the per-PR lineage dirs (<repo>-<hash>~<pr>, and the
	// SoL-Pi data inside them) survive across per-Attempt Jobs, so a
	// re-armed PR resumes its compacted context instead of starting
	// cold. Project isolation is physical (SubPath), like the cache
	// claim. The TTL env arms the in-attempt janitor; the default keeps
	// every claim mount self-pruning even when the spec omits it.
	if p.Sessions != nil && p.Sessions.PVC != "" {
		volumes = append(volumes, corev1.Volume{
			Name:         "sessions",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: p.Sessions.PVC}},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "sessions", SubPath: p.WorkflowName, MountPath: "/sessions"})
		env = append(env, corev1.EnvVar{Name: "HARMOSTES_PI_SESSIONS", Value: "/sessions"})
		ttl := p.Sessions.TTL
		if ttl == "" {
			ttl = "336h"
		}
		env = append(env, corev1.EnvVar{Name: "HARMOSTES_SESSIONS_TTL", Value: ttl})
	}
	for _, at := range p.Attachments {
		if at.PVC == "" || at.Name == "" {
			continue
		}
		volName := "attach-" + at.Name
		sub, ok := AttachmentSubPath(at.Scope, p.WorkflowName, p.TriggerRepo, p.AttemptName)
		if !ok {
			continue // repository scope without a repo: nothing to share
		}
		mountPath := at.MountPath
		if mountPath == "" {
			mountPath = "/attachments/" + at.Name
		}
		volumes = append(volumes, corev1.Volume{
			Name:         volName,
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: at.PVC}},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: volName, SubPath: sub, MountPath: mountPath})
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
			// The wall-clock bound follows the run out of the pool pod
			// (was the consumer's run context). The workflow's runBound
			// (#348/#333) raises it for reviews whose methodology honestly
			// exceeds the fleet default; 0 falls back to OneShotRunBound —
			// and the gate's DispatchTimeout margin is validated over the
			// SAME effective bound (reviewready.go), so exactly-once holds
			// for every configured wall.
			ActiveDeadlineSeconds:   ptr.To(int64(p.runBoundSeconds() / time.Second)),
			TTLSecondsAfterFinished: p.TTLSecondsAfterFinished,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations},
				Spec: corev1.PodSpec{
					ServiceAccountName: p.ServiceAccount,
					RestartPolicy:      corev1.RestartPolicyNever,
					InitContainers: []corev1.Container{{
						Name:    "sync-skills",
						Image:   p.Image,
						Command: skillsSyncCommand(),
						// #407/#408: clone source and pinned rev travel as env
						// data, never spliced into the shell string (see
						// skillsSyncCommand).
						Env: []corev1.EnvVar{
							{Name: "HARMOSTES_SKILLS_REPO", Value: SkillsRepo()},
							{Name: "HARMOSTES_SKILLS_REV", Value: SkillsRev()},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: "skills", MountPath: "/skills"}},
					}},
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

// DeleteJob deletes a per-attempt review Job by name (#402). Used by the
// gate's cancel-on-supersede pass: a claim released as superseded/closed
// leaves its Job running — nothing else deletes it, so the dead-head review
// would burn the full run bound before the moved-head guard discards the
// verdict. Deleting uses default (foreground-adjacent) propagation: the
// running pod is SIGTERMed, which IS the mechanism — the ctx-cancelled run
// never reaches post-review, so no verdict can land for the dead head.
// DeleteJob removes the attempt's review Job. Default propagation cascades
// to the running pod — and that cascade IS the cancellation mechanism
// (#402): SIGTERM → context cancel → the graph aborts before post-review
// ever runs, so a cancelled claim leaves no verdict behind.
func DeleteJob(ctx context.Context, cl client.Client, namespace, name string) error {
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	return cl.Delete(ctx, j)
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
