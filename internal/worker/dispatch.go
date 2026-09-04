package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/attempt"
	"github.com/tibrezus/harmostes/internal/dapr"
	"github.com/tibrezus/harmostes/internal/k8s"
	"github.com/tibrezus/harmostes/internal/review"
	"github.com/tibrezus/harmostes/internal/timeline"
)

// Dispatcher is the consumer's RunFunc implementation (ADR-0007 phase 3):
// it validates the trigger through the Review-Ready Gate, applies the
// workflow's concurrency capacity, and dispatches the run as an Attempt +
// Job — in milliseconds. It never runs graphs: the Job pod's
// `harmostes-worker run` does, with HARMOSTES_DISPATCHED_ATTEMPT marking the
// gate as already satisfied.
type Dispatcher struct {
	// createMu serializes the Attempt+Job create section: two racing wakes
	// for the same PR must not double-dispatch. Correctness comes from the
	// live-Job check inside the lock (deterministic Attempt identity alone
	// would not dedupe — GenerateName makes Job names unique per run).
	createMu sync.Mutex

	cl        client.Client
	scheme    *runtime.Scheme
	namespace string
	logf      func(string, ...any)

	// cfg is the fleet-level Job shape, carried as ONE value — the
	// per-field copies this struct replaced are exactly where #314 hid.
	cfg DispatchConfig
}

// DispatchConfig is the fleet-level half of the Worker Job shape: every
// fact about how per-Attempt Jobs are built that comes from the chart
// environment. It is the deep owner of the env→config→JobParams seam — a
// config fact is parsed once here, validated once here, logged once here,
// and derives the Job shape once here — so it cannot be dropped at a
// hand-copied parameter hop. That drop is not hypothetical: #311 (pool
// mounts never reached the one-shot Job), #312-r1, and #314
// (DispatcherFromEnv accepted the extra-mount parameter and silently never
// copied it into this struct) were all the same failure mode at this seam.
type DispatchConfig struct {
	FleetMaxConcurrent   int
	JobImage             string
	ServiceAccount       string
	JobTTLSeconds        *int32
	DaprdImage           string
	PluginConfigMaps     []string
	ExtraConfigMapMounts []k8s.ConfigMapMount
}

// DispatchConfigFromEnv resolves the fleet-level dispatch configuration
// from the chart environment: HARMOSTES_WORKER_IMAGE (required — the Job
// runs the same image as the pool), HARMOSTES_SERVICE_ACCOUNT,
// HARMOSTES_MAX_CONCURRENT (default 3, ADR-0007), HARMOSTES_JOB_TTL_SECONDS
// (default 3600), HARMOSTES_PLUGIN_CONFIGMAPS, HARMOSTES_EXTRA_CONFIGMAP_MOUNTS,
// and the optional HARMOSTES_DAPRD_IMAGE pin.
//
// Malformed values are ERRORS, not warnings-with-fallback: a chart typo that
// silently drops a mount or silently keeps a default is the #311 failure
// mode — fail-fast at boot turns it into an immediate, visible crash-loop.
// The resolved config is logged before the cluster dial so the log is
// hermetic and always emitted, even where no kubeconfig exists (CI).
func DispatchConfigFromEnv(logf func(string, ...any)) (DispatchConfig, error) {
	cfg := DispatchConfig{
		JobImage:           os.Getenv("HARMOSTES_WORKER_IMAGE"),
		ServiceAccount:     os.Getenv("HARMOSTES_SERVICE_ACCOUNT"),
		DaprdImage:         os.Getenv("HARMOSTES_DAPRD_IMAGE"),
		FleetMaxConcurrent: 3,
	}
	var err error
	if cfg.PluginConfigMaps, err = pluginConfigMapsFromEnv(); err != nil {
		return cfg, err
	}
	if cfg.ExtraConfigMapMounts, err = extraConfigMapMountsFromEnv(); err != nil {
		return cfg, err
	}
	if v := os.Getenv("HARMOSTES_MAX_CONCURRENT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("HARMOSTES_MAX_CONCURRENT=%q: must be a positive integer", v)
		}
		cfg.FleetMaxConcurrent = n
	}
	ttl := int32(3600)
	if v := os.Getenv("HARMOSTES_JOB_TTL_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return cfg, fmt.Errorf("HARMOSTES_JOB_TTL_SECONDS=%q: must be a non-negative integer", v)
		}
		ttl = int32(n)
	}
	cfg.JobTTLSeconds = &ttl
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	// Startup config visibility: mount wiring problems (pool-vs-Job drift,
	// #311) previously surfaced only as silent 12ms prepare failures.
	logf("dispatch config: image=%s serviceAccount=%s maxConcurrent=%d jobTTLSeconds=%d pluginConfigMaps=%v extraConfigMapMounts=%s",
		cfg.JobImage, cfg.ServiceAccount, cfg.FleetMaxConcurrent, *cfg.JobTTLSeconds,
		cfg.PluginConfigMaps, formatConfigMapMounts(cfg.ExtraConfigMapMounts))
	return cfg, nil
}

// Validate rejects configurations that would dispatch Jobs that cannot run:
// no image, or mount specs whose ConfigMap/path the Job cannot mount.
func (c DispatchConfig) Validate() error {
	if c.JobImage == "" {
		return fmt.Errorf("dispatch config: worker image is required (HARMOSTES_WORKER_IMAGE) — an Attempt Job without an image can never run")
	}
	for _, cm := range c.PluginConfigMaps {
		if cm == "" {
			return fmt.Errorf("dispatch config: empty plugin ConfigMap name")
		}
	}
	for _, m := range c.ExtraConfigMapMounts {
		if m.Name == "" || !strings.HasPrefix(m.MountPath, "/") {
			return fmt.Errorf("dispatch config: invalid ConfigMap mount %q (need name and absolute mount path)", m.Name+"="+m.MountPath)
		}
	}
	return nil
}

// JobParams derives the STATIC Job shape from the fleet config. One method
// so a config fact cannot be dropped at a struct-copy hop (#311/#314):
// callers supply only the per-run fields (attempt, workflow, namespace,
// extraEnv).
func (c DispatchConfig) JobParams(at *v1alpha1.Attempt, workflow, namespace string, extraEnv []string) k8s.AttemptJobParams {
	return k8s.AttemptJobParams{
		Attempt:                 at,
		WorkflowName:            workflow,
		Namespace:               namespace,
		Image:                   c.JobImage,
		ServiceAccount:          c.ServiceAccount,
		TTLSecondsAfterFinished: c.JobTTLSeconds,
		DaprdImage:              c.DaprdImage,
		PluginConfigMaps:        c.PluginConfigMaps,
		ExtraConfigMapMounts:    c.ExtraConfigMapMounts,
		ExtraEnv:                extraEnv,
	}
}

// formatConfigMapMounts renders mounts name=path[=mode] for the startup
// config log — the full specs, so operator-visible drift (a missing or
// wrong-mode mount) is readable at boot.
func formatConfigMapMounts(ms []k8s.ConfigMapMount) string {
	if len(ms) == 0 {
		return "none"
	}
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = m.Name + "=" + m.MountPath
		if m.Mode != nil {
			parts[i] += fmt.Sprintf("=%#o", *m.Mode)
		}
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// NewDispatcher builds the dispatcher, connecting the in-cluster client.
func NewDispatcher(ctx context.Context, cfg DispatchConfig, logf func(string, ...any)) (*Dispatcher, error) {
	// The non-env construction path gets the same fail-fast checks.
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("k8s config: %w", err)
	}
	// k8s.Scheme() registers harmostes + corev1 + batchv1. The dispatcher
	// lists batchv1.Jobs (live-Job dedupe, capacity) — a v1alpha1-only
	// scheme fails every List with "no kind is registered" (#277).
	scheme := k8s.Scheme()
	cl, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("k8s client: %w", err)
	}
	ns := os.Getenv("HARMOSTES_NAMESPACE")
	if ns == "" {
		ns = "harmostes"
	}
	return &Dispatcher{
		cl:        cl,
		scheme:    scheme,
		namespace: ns,
		logf:      logf,
		cfg:       cfg,
	}, nil
}

// DispatcherFromEnv resolves the fleet-level configuration from the chart
// environment (DispatchConfigFromEnv) and builds the dispatcher on top of
// it.
func DispatcherFromEnv(logf func(string, ...any)) (*Dispatcher, error) {
	cfg, err := DispatchConfigFromEnv(logf)
	if err != nil {
		return nil, err
	}
	return NewDispatcher(context.Background(), cfg, logf)
}

// Dispatch processes one trigger event: gate → capacity → Attempt+Job.
// A nil return ACKs the event (the armed state is the durable queue —
// waiting/standdown/capacity all re-evaluate on the next wake or sweep).
func (d *Dispatcher) Dispatch(ctx context.Context, req RunRequest) error {
	wf, err := FetchWorkflow(ctx, d.cl, req.Namespace, req.Workflow)
	if err != nil {
		return fmt.Errorf("fetch workflow: %w", err)
	}

	// ── Review-Ready Gate (ADR-0006): validate before dispatching. ──────
	// Only workflows with reviewReady gate; every other class dispatches
	// straight through (every class is Job-per-run, ADR-0007). The gate
	// drains to capacity: one sweep accepts every free slot.
	gateDeps := GateDeps{
		Status:             k8s.StatusPatcher{Client: d.cl, Namespace: req.Namespace},
		Client:             d.cl,
		Scheme:             d.scheme,
		FleetMaxConcurrent: d.cfg.FleetMaxConcurrent,
		Log:                d.logf,
		TL: timeline.NewGateWriter(dapr.Tracing(dapr.New(os.Getenv("DAPR_HTTP_ENDPOINT"))),
			envOr("HARMOSTES_STATE_STORE", "statestore"), wf.Name, "", triggerSubject(req)),
	}

	var dispatches []GateDispatch
	if wf.Spec.ReviewReady != nil {
		dispatches, err = RunReviewGateSweep(ctx, gateDeps, wf)
		if err != nil {
			return fmt.Errorf("review gate: %w", err)
		}
		if len(dispatches) == 0 {
			return nil // waiting/standdown/at-capacity — recorded in aggregates
		}
	} else {
		// Non-gated class: dispatch straight through under a claimless
		// attempt (deterministic objective identity).
		obj := attempt.DeriveObjective(wf, attempt.TriggerContext{Revision: req.Revision, Source: "webhook"})
		at, _, err := attempt.ResolveOrCreate(ctx, d.cl, obj, attempt.ResolveOptions{
			Namespace:   req.Namespace,
			WorkflowRef: req.Namespace + "/" + req.Workflow,
			Owner:       wf,
			Scheme:      d.scheme,
		})
		if err != nil {
			return fmt.Errorf("resolve attempt: %w", err)
		}
		dispatches = append(dispatches, GateDispatch{Attempt: at.Name})
	}

	// ── Create the Jobs (serialized: dedupe racing wakes). ──────────────
	d.createMu.Lock()
	defer d.createMu.Unlock()

	live, err := k8s.ListActiveJobs(ctx, d.cl, req.Namespace, req.Workflow)
	if err != nil {
		return fmt.Errorf("list active jobs: %w", err)
	}
	for _, g := range dispatches {
		duplicate := false
		for _, j := range live {
			if j.Labels["harmostes.dev/attempt"] == g.Attempt {
				duplicate = true
				break
			}
		}
		if duplicate {
			d.logf("dispatch: attempt %s already has a live job — deduped", g.Attempt)
			continue
		}
		var at v1alpha1.Attempt
		if err := d.cl.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: g.Attempt}, &at); err != nil {
			return fmt.Errorf("get claim attempt %s: %w", g.Attempt, err)
		}
		job := k8s.BuildJob(d.cfg.JobParams(&at, req.Workflow, req.Namespace,
			append(jobCredentialEnv(), dispatchEnv(req, &at, g.Envelope)...)))
		if err := d.cl.Create(ctx, job); err != nil {
			if errors.IsAlreadyExists(err) {
				d.logf("dispatch: job for attempt %s already exists — deduped", at.Name)
				continue
			}
			return fmt.Errorf("create job: %w", err)
		}
		if err := attempt.MarkClaimDispatched(ctx, d.cl, req.Namespace, at.Name); err != nil {
			return fmt.Errorf("mark dispatched %s: %w", at.Name, err)
		}
		d.logf("dispatch: job %s created for attempt %s (workflow %s)", job.Name, at.Name, req.Workflow)
	}
	return nil
}

// triggerSubject builds the timeline subject from the trigger event.
func triggerSubject(req RunRequest) timeline.Subject {
	s := timeline.Subject{}
	if req.Pr != "" {
		s.Kind = "pr"
		s.Ref = req.Pr
		s.SHA = req.Revision
		s.Title = req.PrTitle
	}
	return s
}

// jobEnvAllowlist is the deployment-level environment runs consume:
// host credentials (Forgejo/Git/Codeberg tokens, rezus.cloud basics) and
// the agent's LLM endpoint. Pre-ADR, runs inherited the pool's whole env
// via buildChildEnv; the Job boundary dropped it, so the workspace plugin
// hit the private Forgejo anonymously (404) and the agent node had no LLM
// credentials (#287). Exact names on purpose — pod-scoped noise
// (DAPR_*, KUBERNETES_*, service links, POD_NAME) must not cross the
// boundary, and future credentials are added here explicitly.
var jobEnvAllowlist = []string{
	"HARMOSTES_FORGEJO_TOKEN",
	"HARMOSTES_GIT_TOKEN",
	"HARMOSTES_CODEBERG_TOKEN",
	"HARMOSTES_RZC_USERNAME",
	"HARMOSTES_RZC_PASSWORD",
	"LITELLM_API_KEY",
	"LITELLM_URL",
}

// jobCredentialEnv forwards the allowlisted deployment-level vars from the
// dispatcher's own environment to the attempt Job. Later env entries win,
// so dispatchEnv's per-run vars override any forwarded names.
func jobCredentialEnv() []string {
	wanted := map[string]bool{}
	for _, n := range jobEnvAllowlist {
		wanted[n] = true
	}
	var out []string
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 && wanted[kv[:i]] {
			out = append(out, kv)
		}
	}
	return out
}

// dispatchEnv renders the Job's trigger env: the envelope (gate output) when
// present, else the raw wake fields — plus the dispatched-claim marker that
// makes `harmostes-worker run` skip the gate.
func dispatchEnv(req RunRequest, at *v1alpha1.Attempt, env *review.Envelope) []string {
	vars := []string{
		"HARMOSTES_ATTEMPT=" + at.Name,
		"HARMOSTES_DISPATCHED_ATTEMPT=" + at.Name,
	}
	if req.Source != "" {
		vars = append(vars, "HARMOSTES_SOURCE="+req.Source)
	}
	if req.Traceparent != "" {
		vars = append(vars, "HARMOSTES_TRACEPARENT="+req.Traceparent)
	}
	if env != nil {
		// The envelope path still carries the wake action (#336 r1 4.1): the
		// handoff classification reads TRIGGER_ACTION to tell a human
		// re-label (deliberate ⇒ SUMMARY) from an automatic re-arm
		// (interrupted ⇒ CONTINUE). Without this, pr-review — the only class
		// that reaches the gate — never sees the action.
		if req.Action != "" {
			vars = append(vars, "HARMOSTES_TRIGGER_ACTION="+req.Action)
		}
		return append(vars, EnvelopeEnv(env)...)
	}
	if req.Pr != "" {
		vars = append(vars, "HARMOSTES_TRIGGER_PR="+req.Pr)
	}
	if req.Action != "" {
		vars = append(vars, "HARMOSTES_TRIGGER_ACTION="+req.Action)
	}
	if req.Revision != "" {
		vars = append(vars, "HARMOSTES_TRIGGER_REVISION="+req.Revision)
	}
	if req.PrTitle != "" {
		vars = append(vars, "HARMOSTES_TRIGGER_TITLE="+req.PrTitle)
	}
	return vars
}

// EnvelopeEnv exports a Trigger Envelope as the HARMOSTES_TRIGGER_* env form
// the workspace plugin consumes. One bridge for both paths: the dispatcher's
// Job env and the one-shot run's process env.
func EnvelopeEnv(env *review.Envelope) []string {
	envelopeJSON, _ := json.Marshal(env)
	return []string{
		"HARMOSTES_TRIGGER_REPO=" + env.Repo,
		"HARMOSTES_TRIGGER_PR=" + strconv.Itoa(env.PR),
		"HARMOSTES_TRIGGER_SHA=" + env.HeadSHA,
		"HARMOSTES_TRIGGER_BASE=" + env.Base,
		"HARMOSTES_TRIGGER_LABEL=" + env.Label,
		"HARMOSTES_TRIGGER_CONTEXTS=" + string(envelopeJSON),
	}
}

// FetchWorkflow fetches a Workflow CR with its WorkflowTemplate defaults
// applied — the merged spec every trigger path executes. Shared by the
// dispatcher and the one-shot run.
func FetchWorkflow(ctx context.Context, cl client.Client, namespace, name string) (*v1alpha1.Workflow, error) {
	var wf v1alpha1.Workflow
	if err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &wf); err != nil {
		return nil, fmt.Errorf("get workflow %s/%s: %w", namespace, name, err)
	}
	if ref := wf.Spec.TemplateRef; ref != "" {
		var tmpl v1alpha1.WorkflowTemplate
		if err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref}, &tmpl); err != nil {
			return nil, fmt.Errorf("get workflow template %s/%s: %w", namespace, ref, err)
		}
		v1alpha1.ApplyTemplateDefaults(&wf, &tmpl)
	}
	return &wf, nil
}

// pluginConfigMapsFromEnv reads the comma-separated plugin ConfigMap list
// the chart renders (HARMOSTES_PLUGIN_CONFIGMAPS) — the same mounts the pool
// pod carries must reach the per-Attempt Jobs. Empty segments (trailing
// commas) are ignored.
func pluginConfigMapsFromEnv() ([]string, error) {
	raw := os.Getenv("HARMOSTES_PLUGIN_CONFIGMAPS")
	if raw == "" {
		return nil, nil
	}
	var cms []string
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			cms = append(cms, name)
		}
	}
	return cms, nil
}

// extraConfigMapMountsFromEnv parses HARMOSTES_EXTRA_CONFIGMAP_MOUNTS —
// comma-separated ConfigMap=path[=mode] entries the chart renders for the
// named mounts the pool carries but per-Attempt Jobs must ALSO carry for
// pool-only plugins to run (#311). Mode is octal (0755 when omitted).
// Malformed entries are ERRORS: a silently-skipped entry is a silently
// missing mount — the exact 12ms-forever prepare failure #311 produced.
func extraConfigMapMountsFromEnv() ([]k8s.ConfigMapMount, error) {
	raw := os.Getenv("HARMOSTES_EXTRA_CONFIGMAP_MOUNTS")
	if raw == "" {
		return nil, nil
	}
	var out []k8s.ConfigMapMount
	for _, pair := range strings.Split(raw, ",") {
		parts := strings.Split(pair, "=")
		if len(parts) < 2 || parts[0] == "" || !strings.HasPrefix(parts[1], "/") {
			return nil, fmt.Errorf("HARMOSTES_EXTRA_CONFIGMAP_MOUNTS: malformed entry %q (want ConfigMap=/absolute/path[=octal-mode])", pair)
		}
		m := k8s.ConfigMapMount{Name: parts[0], MountPath: parts[1]}
		if len(parts) >= 3 {
			mode, err := strconv.ParseInt(parts[2], 8, 32)
			if err != nil {
				return nil, fmt.Errorf("HARMOSTES_EXTRA_CONFIGMAP_MOUNTS: bad octal mode in %q: %w", pair, err)
			}
			mode32 := int32(mode)
			m.Mode = &mode32
		}
		out = append(out, m)
	}
	return out, nil
}
