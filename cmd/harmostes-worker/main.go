// Command harmostes-worker runs ONE Workflow's pipeline (prepare → agent → deploy)
// as a Kubernetes Job. The monitor controller spawns it; it fetches its Workflow
// CR by name, builds its collaborators from in-cluster clients + the Dapr
// sidecar, runs worker.Run, and exits by outcome.
//
// Env:
//
//	HARMOSTES_WORKFLOW    the Workflow CR name (required)
//	HARMOSTES_NAMESPACE   its namespace (required)
//	HARMOSTES_WORKDIR     source working dir (default /workspace)
//	HARMOSTES_SOURCE      resolved source ref/path (recorded in status)
//	DAPR_HTTP_ENDPOINT    Dapr sidecar URL (default http://localhost:3500)
//	plugins mounted under /plugins (ConfigMap form); built-ins in the image.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/agent"
	"github.com/tibrezus/harmostes/internal/attempt"
	"github.com/tibrezus/harmostes/internal/dapr"
	"github.com/tibrezus/harmostes/internal/graph"
	"github.com/tibrezus/harmostes/internal/k8s"
	"github.com/tibrezus/harmostes/internal/observability"
	"github.com/tibrezus/harmostes/internal/timeline"
	"github.com/tibrezus/harmostes/internal/worker"
	"github.com/tibrezus/harmostes/version"
)

var (
	logger      *slog.Logger
	obsShutdown observability.ShutdownFunc
)

func main() {
	_ = flag.CommandLine.Parse(os.Args[1:])

	// Consumer mode: if HARMOSTES_CONSUMER_MODE is set, start the pub/sub
	// consumer instead of the one-shot Job mode. The consumer subscribes to
	// the harmostes-triggers topic via daprd and processes trigger events by
	// execing itself in one-shot mode (process isolation per run).
	if os.Getenv("HARMOSTES_CONSUMER_MODE") != "" {
		// Initialize a minimal logger so fatal() (which uses the global logger)
		// doesn't panic with a nil pointer if RunConsumer returns an error.
		logger = slog.Default().With("component", "harmostes-worker")
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		if err := worker.RunConsumer(ctx); err != nil {
			fatal("consumer: %v", err)
		}
		return
	}

	workflow := envReq("HARMOSTES_WORKFLOW")
	namespace := envReq("HARMOSTES_NAMESPACE")
	workdir := envOr("HARMOSTES_WORKDIR", "/workspace")
	source := os.Getenv("HARMOSTES_SOURCE")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Observability first: structured JSON logger (trace-aware) + OTLP providers.
	// An unset OTEL_EXPORTER_OTLP_ENDPOINT disables telemetry (no-op providers) —
	// local dev + tests never need a collector.
	logger = observability.NewLogger("harmostes-worker", os.Stdout)
	if sh, err := observability.Init(ctx, observability.Config{
		Component:    "harmostes-worker",
		Version:      version.Version,
		OTLPEndpoint: os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		Insecure:     os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") == "true",
		PodName:      os.Getenv("POD_NAME"),
		PodNamespace: namespace,
	}); err != nil {
		logger.Error("observability init failed — telemetry disabled", "error", err)
	} else {
		obsShutdown = sh
	}

	cfg, err := ctrl.GetConfig()
	if err != nil {
		fatal("k8s config: %v", err)
	}
	cl, err := client.New(cfg, client.Options{Scheme: k8s.Scheme()})
	if err != nil {
		fatal("k8s client: %v", err)
	}

	var wf v1alpha1.Workflow
	if err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: workflow}, &wf); err != nil {
		fatal("get workflow %s/%s: %v", namespace, workflow, err)
	}
	// A Workflow referencing a WorkflowTemplate (spec.templateRef) inherits the
	// template's prepare/agent/deploy defaults; instance-set fields and
	// spec.config win. Applied here so every trigger path executes the merged
	// spec (the stored CR stays thin — the template remains the single source
	// of pipeline shape).
	if ref := wf.Spec.TemplateRef; ref != "" {
		var tmpl v1alpha1.WorkflowTemplate
		if err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref}, &tmpl); err != nil {
			fatal("get workflow template %s/%s: %v", namespace, ref, err)
		}
		v1alpha1.ApplyTemplateDefaults(&wf, &tmpl)
	}
	logf("workflow %s/%s phase=run source=%q workdir=%s", namespace, workflow, source, workdir)

	// ── Review-Ready Gate (ADR-0006) — the PRODUCTION seam. ──────────────
	// Event-armed deterministic trigger for adversarial PR review, evaluated
	// BEFORE any provisioning or graph execution (the graph path is the live
	// one since #177; pipeline.Run is legacy). The gate consumes the wake
	// (env-carried PR pointer + revision) and either hands the run a Trigger
	// Envelope or ends this cycle: waiting (CI pending/red — stays armed,
	// poll re-evaluates) or stood down (label gone / closed / horizon).
	// The envelope flows to the workspace plugin via the process env
	// (HARMOSTES_TRIGGER_*), which the consumer set from the TriggerEvent
	// payload — nothing else to wire.
	if wf.Spec.ReviewReady != nil {
		gateTL := timeline.NewGateWriter(dapr.Tracing(dapr.New(os.Getenv("DAPR_HTTP_ENDPOINT"))),
			envOr("HARMOSTES_STATE_STORE", "statestore"), wf.Name, os.Getenv("HARMOSTES_ATTEMPT"), subjectFromEnv())
		env := worker.RunReviewGateWithTimeline(ctx, k8s.StatusPatcher{Client: cl, Namespace: namespace}, logf, &wf, gateTL)
		if env == nil {
			logf("review-ready: not proceeding this cycle — exiting (waiting/standdown recorded in status)")
			flushTelemetry()
			recordAttemptOutcome(ctx, cl, "skipped", nil, "review-ready: waiting or stood down")
			os.Exit(0)
		}
		// Export the FULL Trigger Envelope into the process env: the
		// consumer pre-sets PR/ACTION/REVISION (buildChildEnv), but
		// REPO/SHA/BASE/LABEL/CONTEXTS are the gate's output and must reach
		// the workspace plugin — extraEnv below snapshots os.Environ(), so
		// os.Setenv here flows to every plugin node.
		envelopeJSON, _ := json.Marshal(env)
		os.Setenv("HARMOSTES_TRIGGER_REPO", env.Repo)
		os.Setenv("HARMOSTES_TRIGGER_PR", fmt.Sprintf("%d", env.PR))
		os.Setenv("HARMOSTES_TRIGGER_SHA", env.HeadSHA)
		os.Setenv("HARMOSTES_TRIGGER_BASE", env.Base)
		os.Setenv("HARMOSTES_TRIGGER_LABEL", env.Label)
		os.Setenv("HARMOSTES_TRIGGER_CONTEXTS", string(envelopeJSON))
		logf("review-ready: proceed pr=%d head=%s base=%s — provisioning workspace", env.PR, env.HeadSHA, env.Base)
	}

	// If the Workflow declares a workspace repo (the wiki / the fork), fetch it
	// into the workdir + operate there. prepare populates it, the agent edits it,
	// deploy pushes it.
	if wr := wf.Spec.WorkspaceRepo; wr != nil && wr.URL != "" {
		wdir, err := fetchWorkspaceRepo(ctx, wr, workdir)
		if err != nil {
			fatal("fetch workspace repo: %v", err)
		}
		workdir = wdir
		logf("workspace repo fetched → %s", workdir)
	}

	// Wait for the Dapr sidecar (best-effort): events + state are fabric, not a
	// hard dependency, but racing ahead means the first publish misses a not-yet-
	// ready daprd. Mirrors the proven llm-wiki / fork-maintenance pattern.
	waitForDapr(os.Getenv("DAPR_HTTP_ENDPOINT"))

	// Single logging source of truth: logf (package-level) already redacts
	// credentials. Aliasing it here prevents the two-logger drift that let the
	// pipeline-internal "FAILED" line leak a token while the worker's own line
	// was redacted (#115).
	logfFn := logf

	deps := worker.Deps{
		Plugins: worker.BuiltinResolver{
			Builtins:      builtinPlugins(),
			ConfigMapRoot: "/plugins",
		},
		Tasks:          k8s.ConfigMapTasks{Client: cl, Namespace: namespace},
		Dapr:           dapr.Tracing(dapr.New(os.Getenv("DAPR_HTTP_ENDPOINT"))),
		Status:         k8s.StatusPatcher{Client: cl, Namespace: namespace},
		DaprStateStore: envOr("HARMOSTES_STATE_STORE", "statestore"),
		DaprPubSub:     envOr("HARMOSTES_PUBSUB", "pubsub"),
		Log:            logfFn,
	}

	// Session capture (Phase 1): wire Dapr state writer + pub/sub publisher so
	// the agent transcript (prompts, tools, responses, gates) is persisted for
	// the UI session viewer.
	runID := runName()
	sessionMeta := agent.SessionMeta{
		Workflow: workflow,
		RunID:    runID,
		Model:    wf.Spec.Agent.Model,
		Skill:    wf.Spec.Agent.Skill,
	}
	// Timeline evidence (ADR-0005 evidence layer): one writer per run; the
	// Attempt CR stays the canonical index. Nil (no attempt / no Dapr) = skip.
	var runTL *timeline.DaprWriter
	if attemptName := os.Getenv("HARMOSTES_ATTEMPT"); attemptName != "" && deps.Dapr != nil {
		runTL = timeline.NewWriter(deps.Dapr, deps.DaprStateStore, attemptName, workflow, runID, subjectFromEnv())
		_ = runTL.SaveSubject(ctx)
		_ = runTL.Emit(ctx, timeline.KindRunStarted, "", map[string]any{"source": source})
	}

	seenTurns := 0
	sessionWriter := func(sctx context.Context, session agent.SessionRecord) error {
		if deps.Dapr == nil {
			return nil
		}
		if runTL != nil {
			for i := seenTurns; i < len(session.Turns); i++ {
				t := session.Turns[i]
				runTL.Emit(sctx, timeline.KindAgentTurn, "agent", map[string]any{
					"turn": i, "label": t.Label, "green": t.Gate != nil && t.Gate.Green,
					"tokensIn": t.Usage.Input, "tokensOut": t.Usage.Output,
				})
			}
			seenTurns = len(session.Turns)
		}
		key := fmt.Sprintf("%s:%s:session", workflow, runID)
		b, err := json.Marshal(session)
		if err != nil {
			return err
		}
		return deps.Dapr.SaveState(sctx, deps.DaprStateStore, key, string(b))
	}
	toolPublisher := func(pctx context.Context, wfName, rid string, tool agent.ToolCall) {
		if runTL != nil {
			runTL.Emit(pctx, timeline.KindAgentTool, "agent", map[string]any{
				"tool": tool.Name, "success": tool.Success,
			})
		}
		if deps.Dapr == nil {
			return
		}
		ev := map[string]any{
			"event":    "tool.call",
			"workflow": wfName,
			"runId":    rid,
			"tool":     tool.Name,
			"success":  tool.Success,
			"args":     tool.Args,
			"result":   tool.Result,
		}
		b, _ := json.Marshal(ev)
		_ = deps.Dapr.Publish(pctx, deps.DaprPubSub, "harmostes-events", string(b))
	}

	// Inject session callbacks into the agent runner.
	// SessionRoot keeps pi's native session file per run (#243): the exact
	// conversation, forkable later (pi --fork). Default on; "off" disables.
	piSessions := envDefault("HARMOSTES_PI_SESSIONS", "/tmp/harmostes-pi-sessions")
	if piSessions != "off" {
		_ = os.MkdirAll(piSessions, 0o755)
	} else {
		piSessions = ""
	}
	deps.Agent = worker.RPCAgentRunner{
		Opts: agent.RPCOptions{
			Args:        worker.PiArgs(wf.Spec.Agent),
			Workdir:     workdir,
			Env:         os.Environ(),
			SessionRoot: piSessions,
			Log: func(ev agent.Event) {
				logfFn("agent: %s %s", ev.Type, ev.ToolName)
			},
		},
		SessionWriter: sessionWriter,
		ToolPublisher: toolPublisher,
		SessionMeta:   sessionMeta,
		// Upload the forkable session alongside the transcript record —
		// best-effort, the run already succeeded.
		SessionFiles: func(fctx context.Context, files []string) {
			if err := worker.SavePiSession(fctx, deps.Dapr, deps.DaprStateStore, workflow, runID, files); err != nil {
				logfFn("pi session upload failed: %v", err)
			}
		},
	}

	// ── Single execution path: graph executor ────────────────────────────
	// Every workflow — declarative (prepare→agent→deploy) or graph-native
	// (spec.graph) — runs through the graph executor. Declarative workflows
	// are compiled to an equivalent graph via graph.CompileWorkflow. This
	// activates the full ADR guarantees (capability enforcement, Node Result
	// Envelopes, claim trust, canonical history) for every workflow.
	var execGraph v1alpha1.GraphSpec
	if wf.Spec.Graph != nil {
		execGraph = *wf.Spec.Graph
		logf("workflow %s/%s — graph-native mode (%d nodes, %d edges)", namespace, wf.Name, len(execGraph.Nodes), len(execGraph.Edges))
	} else {
		execGraph = graph.CompileWorkflow(&wf)
		logf("workflow %s/%s — compiled to graph (%d nodes, %d edges)", namespace, wf.Name, len(execGraph.Nodes), len(execGraph.Edges))
	}

	// Pass HARMOSTES_LAST_RIG_HASH so the rig-emit plugin can do a cross-run
	// deterministic skip (structure unchanged → changed=false → graph skips
	// agent/deploy). Also propagate the full process env so plugins inherit
	// credentials and Dapr endpoints.
	extraEnv := os.Environ()
	if wf.Status.LastRigHash != "" {
		extraEnv = append(extraEnv, "HARMOSTES_LAST_RIG_HASH="+wf.Status.LastRigHash)
	}

	shadow := ""
	if wr := wf.Spec.WorkspaceRepo; wr != nil {
		shadow = wr.Shadow
	}

	graphCtx := observability.ContextWithTraceparent(ctx, os.Getenv(observability.TraceparentCarrierKey))
	graphCtx, graphCancel := context.WithTimeout(graphCtx, runTimeout(&wf))
	defer graphCancel()

	graphDeps := graph.Dependencies{
		PluginResolver: deps.Plugins,
		AgentRunner:    deps.Agent,
		TaskResolver:   taskResolverAdapter{inner: deps.Tasks},
		DaprClient:     deps.Dapr,
		StateStore:     deps.DaprStateStore,
		KubeClient:     graph.NewKubeClient(cl),
		SessionWriter:  sessionWriter,
		ToolPublisher:  toolPublisher,
		SessionMeta:    sessionMeta,
	}
	graphDeps.Timeline = runTL
	result, gErr := graph.ExecuteGraph(graphCtx, execGraph, wf.Name, graphDeps,
		graph.WithStateStore(deps.DaprStateStore),
		graph.WithPubSub(deps.DaprPubSub),
		graph.WithLogger(logfFn),
		graph.WithProvenance(
			os.Getenv("HARMOSTES_TRIGGERED_BY"),
			os.Getenv("HARMOSTES_TRIGGER_SOURCE"),
		),
		graph.WithBindings(wf.Spec.Bindings),
		graph.WithRunID(runID),
		graph.WithTimeline(runTL),
		graph.WithWorkflowContext(graph.WorkflowContext{
			Name:           wf.Name,
			Namespace:      namespace,
			Workdir:        workdir,
			Source:         source,
			SourceURL:      wf.Spec.Source.Repo,
			SourceBranch:   wf.Spec.Source.Branch,
			SourceLanguage: wf.Spec.Source.Language,
			WorkspaceDir:   workdir,
			Shadow:         shadow,
			State:          wf.Name,
			ExtraEnv:       extraEnv,
		}),
	)

	if runTL != nil {
		_ = runTL.Emit(ctx, timeline.KindRunCompleted, "", map[string]any{
			"status": result.Status, "message": result.Message, "source": source,
		})
	}

	// Patch Workflow status from the graph result (mirrors the declarative
	// pipeline's status patching at each phase boundary).
	patchWorkflowStatus(ctx, cl, namespace, wf.Name, &result, source)

	flushTelemetry()
	if gErr != nil {
		recordAttemptOutcome(ctx, cl, "failed", envelopesFor(result.NodeEnvelopes), fmt.Sprintf("graph pipeline error: %v", gErr))
		fatal("graph pipeline error: %v", gErr)
	}
	if result.Status == graph.StatusGreen {
		recordAttemptOutcome(ctx, cl, "succeeded", envelopesFor(result.NodeEnvelopes), result.Message)
		logf("graph complete: %s (%d node envelopes recorded)", result.Message, len(result.NodeEnvelopes))
		finish(0)
	}
	recordAttemptOutcome(ctx, cl, "failed", envelopesFor(result.NodeEnvelopes), result.Message)
	logf("graph complete: %s (%s) — %d node envelopes recorded", result.Status, result.Message, len(result.NodeEnvelopes))
	finish(1)
}

// patchWorkflowStatus patches the Workflow's observed status from the graph
// execution result. Extracts rig_hash from any plugin node's outputs (the
// prepare plugin produces it) and the deploy artifact/commit. Mirrors the
// declarative pipeline's per-phase status patching in a single post-run call.
func patchWorkflowStatus(ctx context.Context, c client.Client, namespace, name string, result *graph.ExecutionResult, source string) {
	gateStatus := "failed"
	if result.Status == graph.StatusGreen {
		gateStatus = "green"
	}
	msg := result.Message
	if len(msg) > 400 {
		msg = msg[len(msg)-400:]
	}

	var rigHash, agentCommit string
	for _, nr := range result.NodeResults {
		if h, ok := nr.Outputs["rig_hash"].(string); ok && h != "" {
			rigHash = h
		}
		if c, ok := nr.Outputs["commit"].(string); ok && c != "" {
			agentCommit = c
		} else if a, ok := nr.Outputs["artifact"].(string); ok && a != "" && agentCommit == "" {
			agentCommit = a
		}
	}

	patcher := k8s.StatusPatcher{Client: c, Namespace: namespace}
	if err := patcher.PatchStatus(ctx, name, func(s *v1alpha1.WorkflowStatus) {
		s.GateStatus = gateStatus
		s.LastRunAt = metav1.Now()
		s.Message = msg
		if rigHash != "" {
			s.LastRigHash = rigHash
		}
		if agentCommit != "" {
			s.LastAgentCommit = agentCommit
		}
		if source != "" {
			s.LastProcessedRevision = source
		}
	}); err != nil {
		logf("warn: patch workflow status: %v", err)
	}
}

// recordAttemptOutcome records this run's terminal outcome into the canonical
// orchestration history (ADR-0005). Best-effort: an empty HARMOSTES_ATTEMPT
// (CRD absent / controller resolution failed) is a no-op; a status-patch error
// is logged but never aborts the run.
func recordAttemptOutcome(ctx context.Context, c client.Client, phase string, envelopes []v1alpha1.NodeResultEnvelope, message string) {
	attemptName := os.Getenv("HARMOSTES_ATTEMPT")
	if attemptName == "" {
		return
	}
	message = redact(message) // defense-in-depth: no token reaches Attempt status
	err := attempt.RecordRunOutcome(ctx, c, os.Getenv("HARMOSTES_NAMESPACE"), attemptName, attempt.RunOutcome{
		RunName:   runName(),
		Phase:     phase,
		Envelopes: envelopes,
		Message:   message,
	})
	if err != nil {
		logf("warn: record attempt outcome %s: %v", attemptName, err)
	}
}

// runName returns the canonical Run identity (ADR-0005): the owning Job name,
// stamped by the controller via the downward API (HARMOSTES_RUN_NAME). This
// matches the name the controller used in RecordRunStarted, so the outcome
// upserts the same RunRecord (no orphaned 'running' entries). Falls back to
// POD_NAME then the workflow name for non-Job execution contexts.
func runName() string {
	if n := os.Getenv("HARMOSTES_RUN_NAME"); n != "" {
		return n
	}
	return envOr("POD_NAME", os.Getenv("HARMOSTES_WORKFLOW"))
}

// envelopesFor converts the graph executor's per-node envelope map into the
// slice the Attempt status stores.
func envelopesFor(m map[string]v1alpha1.NodeResultEnvelope) []v1alpha1.NodeResultEnvelope {
	if len(m) == 0 {
		return nil
	}
	out := make([]v1alpha1.NodeResultEnvelope, 0, len(m))
	for _, env := range m {
		out = append(out, env)
	}
	return out
}

func runTimeout(wf *v1alpha1.Workflow) time.Duration {
	secs := wf.Spec.Agent.Timeout
	if secs <= 0 {
		secs = 1800
	}
	return time.Duration(secs) * time.Second
}

// builtinPlugins maps plugin names to executable paths shipped in the worker
// image (under /usr/local/lib/harmostes/plugins/<name>). Populated as plugins
// are ported (see plugins/README.md).
// taskResolverAdapter adapts worker.TaskResolver (which takes a TaskTemplate) to
// graph.TaskResolver (which takes a plain string ref). The graph model's agent
// node stores the task as a string (e.g. "tasks/wiki-update"); this wraps it in
// a TaskTemplate{Name: ref} for the underlying resolver.
type taskResolverAdapter struct{ inner worker.TaskResolver }

func (a taskResolverAdapter) Get(ctx context.Context, ref string) (string, error) {
	// "configmap/key" refs (emitted by compile.taskRef) resolve directly;
	// anything else falls back to the legacy Name-only form.
	if cm, key, ok := strings.Cut(ref, "/"); ok && cm != "" && key != "" && !strings.Contains(key, " ") {
		return a.inner.Get(ctx, v1alpha1.TaskTemplate{ConfigMap: cm, Key: key})
	}
	return a.inner.Get(ctx, v1alpha1.TaskTemplate{Name: ref})
}

func builtinPlugins() map[string]string {
	return map[string]string{
		"noop":      "/usr/local/lib/harmostes/plugins/noop.sh",
		"rig-emit":  "/usr/local/lib/harmostes/plugins/rig-emit.sh",
		"wiki-lint": "/usr/local/lib/harmostes/plugins/wiki-lint.sh",
		"git-push":  "/usr/local/lib/harmostes/plugins/git-push.sh",
	}
}

func envReq(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Fprintf(os.Stderr, "ERROR: %s is required\n", key)
		os.Exit(2)
	}
	return v
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func logf(format string, a ...any) { logger.Info(redact(fmt.Sprintf(format, a...))) }

// finish is the single exit path for the worker: it flushes telemetry, then
// drains the Dapr sidecar, then exits. Every outcome (green/skipped, failed,
// fatal) routes through it so the ephemeral Job never drops telemetry — the
// Phase 1 guarantee. Telemetry is flushed BEFORE the sidecar, which carries
// some of it.
func finish(code int) {
	flushTelemetry()
	shutdownDapr()
	os.Exit(code)
}

// flushTelemetry pushes in-flight spans/metrics within ShutdownTimeout. A nil
// obsShutdown (disabled/failed Init) is a no-op.
func flushTelemetry() {
	if obsShutdown == nil {
		return
	}
	if err := observability.ShutdownWithTimeout(context.Background(), obsShutdown, observability.ShutdownTimeout); err != nil {
		logger.Error("telemetry flush error", "error", err)
	}
}

func fatal(format string, a ...any) {
	logger.Error(redact(fmt.Sprintf(format, a...)))
	finish(2)
}

// shutdownDapr asks the Dapr sidecar to terminate so the pod reaches Completed
// (otherwise daprd keeps the pod alive forever, stranding the Job as "Running").
// Best-effort: a missing or not-yet-ready sidecar simply means no shutdown.
//
// SKIPPED when HARMOSTES_NO_DAPR_SHUTDOWN is set — the consumer-exec'd worker
// shares the pod's daprd sidecar and must NOT shut it down (that would kill
// the consumer process).
func shutdownDapr() {
	if os.Getenv("HARMOSTES_NO_DAPR_SHUTDOWN") != "" {
		logf("dapr shutdown: skipped (consumer-exec'd worker shares sidecar)")
		return
	}
	ep := os.Getenv("DAPR_HTTP_ENDPOINT")
	if ep == "" {
		ep = "http://127.0.0.1:3500"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(ep+"/v1.0/shutdown", "application/json", nil)
	if err != nil {
		logf("dapr shutdown: %v (continuing)", err)
		return
	}
	resp.Body.Close()
	logf("dapr shutdown: sent (status %s)", resp.Status)
}

// fetchWorkspaceRepo clones the workspace repo (shallow) into <base>/<dir> and
// returns that path. The pipeline (prepare/agent/gate/deploy) operates there.
func fetchWorkspaceRepo(ctx context.Context, wr *v1alpha1.WorkspaceRepoSpec, base string) (string, error) {
	dir := wr.Dir
	if dir == "" {
		dir = "repo"
	}
	target := filepath.Join(base, dir)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	_ = os.RemoveAll(target) // idempotent: remove a stale checkout
	cloneURL := tokenizeGitURL(wr.URL, os.Getenv("HARMOSTES_GIT_TOKEN"))
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "100", cloneURL, target)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git clone %s: %w (%s)", redact(wr.URL), err, string(out))
	}
	if wr.Branch != "" {
		co := exec.CommandContext(ctx, "git", "-C", target, "checkout", wr.Branch)
		if out, err := co.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git checkout %s: %w (%s)", wr.Branch, err, string(out))
		}
	}
	return target, nil
}

// tokenizeGitURL embeds a token into an https git URL for auth. No-op for SSH or
// already-authenticated URLs. The token comes from HARMOSTES_GIT_TOKEN (injected
// from a secret by the controller), never from the CR spec.
func tokenizeGitURL(url, token string) string {
	if token == "" || !strings.HasPrefix(url, "https://") {
		return url
	}
	return strings.Replace(url, "https://", "https://x-access-token:"+token+"@", 1)
}

// redact strips embedded HTTP basic-auth credentials from a URL or arbitrary
// string (plugin output, error messages, pipeline results) before it is logged
// or recorded in Attempt status. Applied at the logf / fatal /
// recordAttemptOutcome choke points so no token can reach logs or history.
func redact(s string) string { return worker.Redact(s) }

// waitForDapr polls the sidecar healthz up to ~15s; proceeds regardless (Dapr is
// best-effort — the pipeline runs even without it, just without events/state).
func waitForDapr(endpoint string) {
	if endpoint == "" {
		endpoint = "http://127.0.0.1:3500" // not localhost (Go IPv6 ::1 vs daprd 127.0.0.1)
	}
	for i := 0; i < 30; i++ {
		resp, err := http.Get(endpoint + "/v1.0/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 300 { // 200 (ready) or 204 (some Dapr versions)
				return
			}
		}
		time.Sleep(time.Second)
	}
	logf("warn: Dapr sidecar not ready at %s after 30s — proceeding without events/state", endpoint)
}

// subjectFromEnv builds the timeline Subject from the Trigger Envelope env:
// what triggered this run and the human anchor to orient by.
func subjectFromEnv() timeline.Subject {
	s := timeline.Subject{}
	if pr := os.Getenv("HARMOSTES_TRIGGER_PR"); pr != "" {
		s.Kind = "pr"
		if repo := os.Getenv("HARMOSTES_TRIGGER_REPO"); repo != "" {
			// consumer env carries the bare number; the gate's full envelope
			// exports "REPO" + "PR" separately.
			if strings.Contains(pr, "#") {
				s.Ref = pr // pointer form "host/owner/name#N" (annotation fallback)
			} else {
				s.Ref = repo + "#" + pr
			}
		} else if strings.Contains(pr, "#") {
			s.Ref = pr
		} else {
			s.Ref = "#" + pr
		}
		// SHA: the gate's envelope SHA on proceed; the wake revision otherwise
		// (the head that triggered this cycle).
		s.SHA = os.Getenv("HARMOSTES_TRIGGER_SHA")
		if s.SHA == "" {
			s.SHA = os.Getenv("HARMOSTES_TRIGGER_REVISION")
		}
	}
	if t := os.Getenv("HARMOSTES_TRIGGER_TITLE"); t != "" {
		s.Title = t
	}
	return s
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
