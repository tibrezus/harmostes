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

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/agent"
	"github.com/tibrezus/harmostes/internal/dapr"
	"github.com/tibrezus/harmostes/internal/graph"
	"github.com/tibrezus/harmostes/internal/k8s"
	"github.com/tibrezus/harmostes/internal/observability"
	"github.com/tibrezus/harmostes/internal/worker"
	"github.com/tibrezus/harmostes/version"
)

var (
	logger      *slog.Logger
	obsShutdown observability.ShutdownFunc
)

func main() {
	workflow := envReq("HARMOSTES_WORKFLOW")
	namespace := envReq("HARMOSTES_NAMESPACE")
	workdir := envOr("HARMOSTES_WORKDIR", "/workspace")
	source := os.Getenv("HARMOSTES_SOURCE")
	_ = flag.CommandLine.Parse(os.Args[1:])

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
	logf("workflow %s/%s phase=run source=%q workdir=%s", namespace, workflow, source, workdir)

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

	logfFn := func(format string, a ...any) { logger.Info(fmt.Sprintf(format, a...)) }

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
		Agent: worker.RPCAgentRunner{Opts: agent.RPCOptions{
			Args:    worker.PiArgs(wf.Spec.Agent),
			Workdir: workdir,
			Env:     os.Environ(),
			Log: func(ev agent.Event) {
				logfFn("agent: %s %s", ev.Type, ev.ToolName)
			},
		}},
	}

	// Graph-native mode: if a Pipeline CR is specified, dispatch to the graph
	// executor instead of the fixed-shape worker.Run pipeline.
	if pipelineName := os.Getenv("HARMOSTES_PIPELINE"); pipelineName != "" {
		graphCtx := observability.ContextWithTraceparent(ctx, os.Getenv(observability.TraceparentCarrierKey))
		graphCtx, graphCancel := context.WithTimeout(graphCtx, 30*time.Minute)
		defer graphCancel()
		var pipe v1alpha1.Pipeline
		if err := cl.Get(graphCtx, client.ObjectKey{Namespace: namespace, Name: pipelineName}, &pipe); err != nil {
			fatal("get pipeline %s/%s: %v", namespace, pipelineName, err)
		}
		logf("pipeline %s/%s — graph execution (%d nodes, %d edges)", namespace, pipelineName, len(pipe.Spec.Graph.Nodes), len(pipe.Spec.Graph.Edges))
		graphDeps := graph.Dependencies{
			PluginResolver: deps.Plugins,
			AgentRunner:    deps.Agent,
			TaskResolver:   taskResolverAdapter{inner: deps.Tasks},
			DaprClient:     deps.Dapr,
			KubeClient:     graph.NewKubeClient(cl),
		}
		result, err := graph.ExecuteGraph(graphCtx, pipe.Spec.Graph, pipelineName, graphDeps,
			graph.WithStateStore(deps.DaprStateStore),
			graph.WithPubSub(deps.DaprPubSub),
			graph.WithLogger(logfFn),
			graph.WithProvenance(
				os.Getenv("HARMOSTES_TRIGGERED_BY"),
				os.Getenv("HARMOSTES_TRIGGER_SOURCE"),
			),
		)
		flushTelemetry()
		if err != nil {
			fatal("graph pipeline error: %v", err)
		}
		if result.Status == graph.StatusGreen {
			logf("graph complete: %s", result.Message)
			finish(0)
		}
		logf("graph complete: %s (%s)", result.Status, result.Message)
		finish(1)
	}

	runCtx := observability.ContextWithTraceparent(ctx, os.Getenv(observability.TraceparentCarrierKey))
	runCtx, runCancel := context.WithTimeout(runCtx, runTimeout(&wf))
	defer runCancel()
	res, err := worker.Run(runCtx, deps, worker.Options{
		Workflow: &wf, Workdir: workdir, Source: source, ExtraEnv: os.Environ(),
	})
	if err != nil {
		fatal("pipeline error: %v", err)
	}
	switch res.Outcome {
	case worker.OutcomeGreen, worker.OutcomeSkipped:
		logf("complete: %s (%s)", res.Outcome, res.Message)
		finish(0)
	default:
		logf("complete: %s (%s)", res.Outcome, res.Message)
		finish(1)
	}
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

func logf(format string, a ...any) { logger.Info(fmt.Sprintf(format, a...)) }

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
	logger.Error(fmt.Sprintf(format, a...))
	finish(2)
}

// shutdownDapr asks the Dapr sidecar to terminate so the pod reaches Completed
// (otherwise daprd keeps the pod alive forever, stranding the Job as "Running").
// Best-effort: a missing or not-yet-ready sidecar simply means no shutdown.
func shutdownDapr() {
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

// redact strips credentials from a URL for logging.
func redact(url string) string {
	if i := strings.Index(url, "://"); i >= 0 {
		scheme := url[:i+3]
		rest := url[i+3:]
		if at := strings.Index(rest, "@"); at >= 0 {
			return scheme + rest[at+1:]
		}
	}
	return url
}

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
