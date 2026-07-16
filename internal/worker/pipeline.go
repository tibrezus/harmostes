package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/dapr"
	"github.com/tibrezus/harmostes/internal/observability"
)

// Outcome of a pipeline run.
type Outcome int

const (
	OutcomeGreen   Outcome = iota // agent passed the gate; deploy ran
	OutcomeSkipped                // prepare reported no change (deterministic skip)
	OutcomeFailed                 // a phase failed (prepare, agent gate, or deploy)
)

func (o Outcome) String() string {
	return [...]string{"green", "skipped", "failed"}[o]
}

// Result is the pipeline outcome.
type Result struct {
	Outcome Outcome
	Message string
}

// StatusPatcher reconciles the Workflow status. The mutate closure edits the
// status in place; the implementation performs the actual status-subresource
// patch (the worker never writes spec).
type StatusPatcher interface {
	PatchStatus(ctx context.Context, name string, mutate func(*v1alpha1.WorkflowStatus)) error
}

// Deps are the pipeline's collaborators (all injectable → testable).
type Deps struct {
	Plugins        PluginResolver
	Tasks          TaskResolver
	Dapr           dapr.Client
	Status         StatusPatcher
	Agent          AgentRunner
	Log            func(format string, args ...any)
	DaprStateStore string // default "statestore"
	DaprPubSub     string // default "pubsub"
}

// Options for one run.
type Options struct {
	Workflow *v1alpha1.Workflow
	Workdir  string   // pre-populated source working directory
	Source   string   // resolved source (artifact path / ref / revision)
	ExtraEnv []string // extra env for plugins (tokens, …)
}

// Run executes the full pipeline for one Workflow work item:
//
//	prepare(plugin) →[changed]→ agent(harmostes primitive + gate, feedback loop)
//	                  → deploy(plugin), emitting Dapr events + reconciling status.
//
// prepare reporting changed=false short-circuits to a deterministic skip.
//
// The whole run is one trace: a root `harmostes.worker.run` span with
// prepare/agent/deploy children. The Dapr client injects the active span's W3C
// traceparent onto every sidecar call, so daprd's state/pubsub spans nest under
// the phase that triggered them (the trace-join). The default sampler samples
// root spans at 1.0 (decision #2) — agent runs are rare + expensive, always
// traced. A disabled tracer (no Init) makes all of this a no-op.
func Run(ctx context.Context, deps Deps, opts Options) (res Result, err error) {
	wf := opts.Workflow
	name := wf.Name
	logf := deps.log()
	tracer := observability.Tracer()

	// Root span: one trace per worker run. outcome is set on exit (named returns)
	// so the trace records how the run ended.
	runCtx, root := tracer.Start(ctx, "harmostes.worker.run",
		trace.WithAttributes(
			attribute.String("harmostes.workflow", name),
			attribute.String("harmostes.namespace", wf.Namespace),
			attribute.String("harmostes.source", opts.Source),
			attribute.String("harmostes.workdir", opts.Workdir),
		))
	runCtx = observability.WithWorkflow(runCtx, name) // metric attribute for agent/plugin layers
	defer func() {
		root.SetAttributes(attribute.String("harmostes.outcome", res.Outcome.String()))
		if err != nil {
			root.RecordError(err)
			root.SetStatus(codes.Error, err.Error())
		}
		root.End()
	}()

	envFor := func(phase, specJSON string) PluginEnv {
		wr := wf.Spec.WorkspaceRepo
		wsDir := opts.Workdir
		shadow := ""
		if wr != nil {
			shadow = wr.Shadow
		}
		return PluginEnv{
			Workflow: name, Namespace: wf.Namespace, Phase: phase,
			Spec: specJSON, Source: opts.Source, Workdir: opts.Workdir, State: name,
			SourceURL: wf.Spec.Source.Repo, SourceBranch: wf.Spec.Source.Branch,
			SourceLanguage: wf.Spec.Source.Language,
			WorkspaceDir:   wsDir, Shadow: shadow,
		}
	}

	// ── 1. prepare (deterministic) ────────────────────────────────────────────
	pctx, prepSpan := tracer.Start(runCtx, "prepare")
	prepCmd, prepArgs, perr := deps.Plugins.Resolve(pctx, wf.Spec.Prepare.Plugin, "prepare")
	if perr != nil {
		return failPhase(pctx, prepSpan, deps, name, "resolve prepare plugin: "+perr.Error(), perr)
	}
	prepSpec, _ := json.Marshal(wf.Spec.Prepare)
	prepRes, prepOut, prepErr := runPluginTraced(pctx, "prepare", wf.Spec.Prepare.Plugin.Name, prepCmd, prepArgs, envFor("prepare", string(prepSpec)), opts.ExtraEnv)
	if prepErr != nil {
		return failPhase(pctx, prepSpan, deps, name, "prepare plugin failed: "+tailN(prepOut, 400), prepErr)
	}
	prepSpan.SetAttributes(attribute.String("harmostes.plugin.artifact", prepRes.Artifact))
	publish(pctx, deps, wf, "onPrepare", prepRes)
	if prepRes.Changed != nil && !*prepRes.Changed {
		logf("prepare: no change — deterministic skip")
		skipSpan(pctx, "prepare.no_change", attribute.String("harmostes.skip", "no_change"))
		patchStatus(pctx, deps, name, func(s *v1alpha1.WorkflowStatus) {
			s.GateStatus = "green"
			s.Message = "no change (deterministic skip)"
			s.LastRunAt = nowMeta()
		})
		prepSpan.End()
		return Result{Outcome: OutcomeSkipped, Message: "no change"}, nil
	}

	// Hash-based deterministic skip: the prepare plugin includes a rig_hash in
	// its event payload. If it matches the hash stored in the Workflow status
	// from the last successful run, the source structure is unchanged — skip the
	// agent entirely (no LLM tokens consumed). This is independent of which git
	// branch was pushed to (shadow branches can lag behind 'main'), making it
	// more reliable than a file-level diff in the plugin.
	if rigHash, ok := prepRes.Event["rig_hash"].(string); ok && rigHash != "" {
		if rigHash == wf.Status.LastRigHash {
			logf("prepare: RIG hash unchanged (%s) — deterministic skip", rigHash[:12])
			skipSpan(pctx, "prepare.rig_hash_unchanged",
				attribute.String("harmostes.skip", "rig_hash_unchanged"),
				attribute.String("harmostes.rig_hash", rigHash))
			patchStatus(pctx, deps, name, func(s *v1alpha1.WorkflowStatus) {
				s.GateStatus = "green"
				s.Message = "no change (rig hash unchanged)"
				s.LastRunAt = nowMeta()
			})
			prepSpan.End()
			return Result{Outcome: OutcomeSkipped, Message: "rig hash unchanged"}, nil
		}
		logf("prepare: RIG hash differs from last processed — agent will run")
	}

	logf("prepare: artifact=%s changed=true", prepRes.Artifact)
	prepSpan.End()

	// ── 2. agent (framework-native: the harmostes primitive + gate) ───────────
	actx, agentSpan := tracer.Start(runCtx, "agent")
	gateCmd, gateArgs, gerr := deps.Plugins.Resolve(actx, wf.Spec.Agent.Gate.Plugin, "gate")
	if gerr != nil {
		return failPhase(actx, agentSpan, deps, name, "resolve gate plugin: "+gerr.Error(), gerr)
	}
	gateSpec, _ := json.Marshal(wf.Spec.Agent.Gate)
	gate := GatePlugin{Name: wf.Spec.Agent.Gate.Plugin.Name, Command: gateCmd, Args: gateArgs, Env: envFor("gate", string(gateSpec)), ExtraEnv: opts.ExtraEnv}

	task, terr := deps.Tasks.Get(actx, wf.Spec.Agent.TaskTemplate)
	if terr != nil {
		return failPhase(actx, agentSpan, deps, name, "resolve task template: "+terr.Error(), terr)
	}
	// Scope the agent to THIS workflow's project only. A harmostes namespace runs
	// many Workflows (one per project); without this, a generic task like "sync the
	// projects under raw/arch/" would have the agent touch every project.
	task = task + "\n\nSCOPE: this Workflow owns exactly ONE project: " + name + ". Work ONLY on " +
		"raw/arch/" + name + "/, its model.c4, and wiki/entities/" + name + ".md " +
		"(plus index.md/log.md). Do NOT read or modify any other project under " +
		"raw/arch/ — those are owned by other Workflows."
	maxFixes := wf.Spec.Agent.MaxFixes
	if maxFixes == 0 {
		maxFixes = 3
	}

	logf("agent: task=%q gate=%s maxFixes=%d", wf.Spec.Agent.TaskTemplate.Name, wf.Spec.Agent.Gate.Plugin.Name, maxFixes)
	agentRes, aerr := deps.Agent.Run(actx, task, gate, maxFixes, logBridge(logf))
	if aerr != nil {
		return failPhase(actx, agentSpan, deps, name, "agent run: "+aerr.Error(), aerr)
	}
	agentSpan.SetAttributes(attribute.Int("harmostes.gate.attempts", agentRes.Attempts))
	if !agentRes.Green {
		msg := fmt.Sprintf("gate failed after %d evaluation(s)", agentRes.Attempts)
		publish(actx, deps, wf, "onFailed", PluginResult{Status: "failed"})
		patchStatus(actx, deps, name, func(s *v1alpha1.WorkflowStatus) {
			s.GateStatus = "failed"
			s.Message = msg
			s.LastRunAt = nowMeta()
		})
		agentSpan.SetStatus(codes.Error, msg)
		agentSpan.End()
		return Result{Outcome: OutcomeFailed, Message: msg}, nil
	}
	logf("agent: gate GREEN after %d pass(es)", agentRes.Attempts)
	agentSpan.End()

	// ── 3. deploy (deterministic) ─────────────────────────────────────────────
	dctx, deploySpan := tracer.Start(runCtx, "deploy")
	depCmd, depArgs, derr := deps.Plugins.Resolve(dctx, wf.Spec.Deploy.Plugin, "deploy")
	if derr != nil {
		return failPhase(dctx, deploySpan, deps, name, "resolve deploy plugin: "+derr.Error(), derr)
	}
	depSpec, _ := json.Marshal(wf.Spec.Deploy)
	depRes, depOut, depErr := runPluginTraced(dctx, "deploy", wf.Spec.Deploy.Plugin.Name, depCmd, depArgs, envFor("deploy", string(depSpec)), opts.ExtraEnv)
	if depErr != nil {
		return failPhase(dctx, deploySpan, deps, name, "deploy plugin failed: "+tailN(depOut, 400), depErr)
	}
	deploySpan.SetAttributes(attribute.String("harmostes.plugin.artifact", depRes.Artifact))
	publish(dctx, deps, wf, "onResolved", depRes)

	patchStatus(dctx, deps, name, func(s *v1alpha1.WorkflowStatus) {
		s.GateStatus = "green"
		s.Message = "deployed"
		s.LastAgentCommit = commitFrom(depRes)
		// Persist the RIG hash so the next run can skip if the source is unchanged.
		if rigHash, ok := prepRes.Event["rig_hash"].(string); ok {
			s.LastRigHash = rigHash
		}
		if opts.Source != "" {
			s.LastProcessedRevision = opts.Source
		}
		s.LastRunAt = nowMeta()
	})
	logf("deploy: %s — pipeline complete", depRes.Artifact)
	deploySpan.End()
	return Result{Outcome: OutcomeGreen, Message: "deployed"}, nil
}

// failPhase records the error on the phase span, records a failed status (whose
// status.patched event lands on the still-open span), ends the span, and returns
// a failed Result. Used at every phase error-exit point.
func failPhase(ctx context.Context, span trace.Span, deps Deps, name, msg string, cause error) (Result, error) {
	span.RecordError(cause)
	r, e := finishFailed(ctx, deps, name, msg)
	span.End()
	return r, e
}

// finishFailed records a failure on the status + returns a failed Result.
func finishFailed(ctx context.Context, deps Deps, name, msg string) (Result, error) {
	deps.log()("pipeline FAILED: %s", msg)
	patchStatus(ctx, deps, name, func(s *v1alpha1.WorkflowStatus) {
		s.GateStatus = "failed"
		s.Message = tailN(msg, 400)
		s.LastRunAt = nowMeta()
	})
	return Result{Outcome: OutcomeFailed, Message: msg}, nil
}

// skipSpan emits a short-lived child span marking a deterministic skip, so an
// "alive & idle" run (no agent work) is observable as a distinct span rather
// than merely the absence of agent/deploy spans.
func skipSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) {
	_, span := observability.Tracer().Start(ctx, name, trace.WithAttributes(attrs...))
	span.End()
}

// publish emits a Dapr event for a phase boundary (best-effort). ctx carries the
// phase span so the sidecar's publish span nests under it (the trace-join).
func publish(ctx context.Context, deps Deps, wf *v1alpha1.Workflow, field string, res PluginResult) {
	if deps.Dapr == nil || wf.Spec.Events == nil {
		return
	}
	var topic string
	switch field {
	case "onPrepare":
		topic = wf.Spec.Events.OnPrepare
	case "onResolved":
		topic = wf.Spec.Events.OnResolved
	case "onFailed":
		topic = wf.Spec.Events.OnFailed
	case "onDeployed":
		topic = wf.Spec.Events.OnDeployed
	}
	if topic == "" {
		return
	}
	payload := map[string]any{"workflow": wf.Name, "status": res.Status, "artifact": res.Artifact, "event": res.Event}
	b, _ := json.Marshal(payload)
	pubsub := deps.DaprPubSub
	if pubsub == "" {
		pubsub = "pubsub"
	}
	if err := deps.Dapr.Publish(ctx, pubsub, topic, string(b)); err != nil {
		deps.log()("warn: publish %s/%s: %v", pubsub, topic, err)
	}
}

func patchStatus(ctx context.Context, deps Deps, name string, mutate func(*v1alpha1.WorkflowStatus)) {
	if deps.Status == nil {
		return
	}
	if err := deps.Status.PatchStatus(ctx, name, mutate); err != nil {
		deps.log()("warn: patch status: %v", err)
		return
	}
	// Snapshot what the mutate set, so the trace records the resulting status —
	// without coupling the StatusPatcher to OTel. mutate is a pure field-setter,
	// so running it twice (snapshot + patch) is safe.
	snap := &v1alpha1.WorkflowStatus{}
	mutate(snap)
	trace.SpanFromContext(ctx).AddEvent("status.patched", trace.WithAttributes(
		attribute.String("harmostes.gate_status", snap.GateStatus),
		attribute.String("harmostes.message", tailN(snap.Message, 200)),
	))
}

func (d Deps) log() func(format string, args ...any) {
	if d.Log != nil {
		return d.Log
	}
	return func(string, ...any) {}
}

func commitFrom(res PluginResult) string {
	if res.Event != nil {
		if c, ok := res.Event["commit"].(string); ok {
			return c
		}
	}
	return res.Artifact
}

func nowMeta() metav1.Time {
	return metav1.NewTime(time.Now().UTC())
}

// tailN returns the last n bytes of s (for status messages / feedback).
func tailN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
