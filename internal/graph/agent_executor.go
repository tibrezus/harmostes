package graph

import (
	"context"
	"encoding/json"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/agent"
	"github.com/tibrezus/harmostes/internal/dapr"
	"github.com/tibrezus/harmostes/internal/observability"
	"github.com/tibrezus/harmostes/internal/worker"
)

// AgentExecutor runs an "agent" node — a non-deterministic pi.dev LLM session
// with optional gate validation. It wraps the existing agent.Task loop via
// the AgentRunner interface.
//
// If the node config includes a gate, the executor resolves the gate plugin
// via the PluginResolver and runs the full task→gate→feedback loop (up to
// maxFixes). If no gate is configured, it runs a single prompt and always
// returns green.
type AgentExecutor struct {
	runner      AgentRunner
	tasks       TaskResolver
	resolver    worker.PluginResolver // for inline gate resolution
	dapr        dapr.Client           // optional: persists usage to state store
	stateStore  string                // Dapr state store component name
	sessionWr   agent.SessionWriter   // optional: persists session transcript
	toolPub     agent.ToolPublisher   // optional: publishes per-tool pub/sub events
	sessionMeta agent.SessionMeta     // identity metadata for the session record
}

// NewAgentExecutor creates an agent node executor. The resolver is used to
// resolve inline gate plugins; it may be nil if gates are always separate
// nodes. dapr + stateStore enable usage persistence (nil-safe).
func NewAgentExecutor(runner AgentRunner, tasks TaskResolver, resolver worker.PluginResolver, daprClient dapr.Client, stateStore string) *AgentExecutor {
	return &AgentExecutor{runner: runner, tasks: tasks, resolver: resolver, dapr: daprClient, stateStore: stateStore}
}

func (e *AgentExecutor) Type() string           { return "agent" }
func (e *AgentExecutor) Deterministic() bool    { return false }
func (e *AgentExecutor) ExecutionClass() string { return ExecutionClassWorkload }

func (e *AgentExecutor) Execute(ctx context.Context, node v1alpha1.NodeSpec, env NodeEnv) (NodeResult, error) {
	ctx, span := observability.Tracer().Start(ctx, "graph.node.agent")
	defer span.End()
	span.SetAttributes(
		attribute.String("harmostes.node.id", node.ID),
		attribute.String("harmostes.node.type", "agent"),
	)

	cfg, err := parseConfig[AgentNodeConfig](node.Config)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return NodeResult{Status: StatusFailed, Feedback: err.Error()}, err
	}

	span.SetAttributes(
		attribute.String("harmostes.agent.model", cfg.Model),
		attribute.String("harmostes.agent.skill", cfg.Skill),
		attribute.Int("harmostes.agent.max_fixes", cfg.MaxFixes),
		attribute.Int("harmostes.message_chars", len(cfg.Task)),
	)

	// Resolve the task text: if a TaskResolver is configured and the task looks
	// like a reference (not inline text), resolve it. Otherwise use inline.
	task := cfg.Task
	if e.tasks != nil && looksLikeRef(cfg.Task) {
		resolved, err := e.tasks.Get(ctx, cfg.Task)
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			return NodeResult{Status: StatusFailed, Feedback: "resolve task: " + err.Error()}, err
		}
		task = resolved
	}

	// Append optional scope (injected by CompileWorkflow for gate-specific
	// workflows like wiki-lint, where the agent must be confined to one
	// project under raw/arch/).
	if cfg.Scope != "" {
		task = task + "\n\n" + cfg.Scope
	}

	// Append the attempt handoff brief (ADR-0008): what interrupted
	// predecessor runs of this attempt accomplished. The brief carries its
	// own framing (CONTINUE vs SUMMARY) — the executor is only the transport.
	if env.Handoff != "" {
		task = task + "\n\n" + env.Handoff
	}

	// Build the gate (optional).
	var gate agent.Gate
	if cfg.Gate != nil {
		if e.resolver == nil {
			err := fmt.Errorf("agent node %q has inline gate but no plugin resolver wired", node.ID)
			span.SetStatus(codes.Error, err.Error())
			return NodeResult{Status: StatusFailed, Feedback: err.Error()}, err
		}
		g, err := cfg.Gate.AsAgentGate(ctx, e.resolver, env)
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			return NodeResult{Status: StatusFailed, Feedback: "resolve gate: " + err.Error()}, err
		}
		gate = g
	}

	// Run the agent loop.
	maxFixes := cfg.MaxFixes
	if maxFixes < 1 {
		maxFixes = 1
	}
	// Build session-capture options from executor config.
	var agentOpts []agent.TaskOption
	meta := e.sessionMeta
	if meta.Workflow == "" {
		meta.Workflow = env.Workflow
	}
	meta.RunID = env.RunID
	agentOpts = append(agentOpts, agent.WithSessionMeta(meta))
	if e.sessionWr != nil {
		agentOpts = append(agentOpts, agent.WithSessionWriter(e.sessionWr))
	}
	if e.toolPub != nil {
		agentOpts = append(agentOpts, agent.WithToolPublisher(e.toolPub))
	}

	result, err := e.runner.Run(ctx, task, gate, maxFixes, nil, agentOpts...)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return NodeResult{Status: StatusFailed, Feedback: err.Error()}, err
	}

	status := StatusFailed
	if result.Green {
		status = StatusGreen
	}

	span.SetAttributes(
		attribute.String("harmostes.agent.status", string(status)),
		attribute.Int("harmostes.agent.attempts", result.Attempts),
		attribute.Int("harmostes.tokens.input", result.Usage.Input),
		attribute.Int("harmostes.tokens.output", result.Usage.Output),
		attribute.Float64("harmostes.tokens.cost", result.Usage.Cost),
	)

	// Persist the full session transcript to Dapr state for the UI viewer.
	if e.dapr != nil && e.stateStore != "" && len(result.Session.Turns) > 0 {
		runID := env.RunID
		if runID == "" {
			runID = env.Workflow
		}
		key := fmt.Sprintf("%s:%s:session", env.Workflow, runID)
		sessionJSON, _ := json.Marshal(result.Session)
		if err := e.dapr.SaveState(ctx, e.stateStore, key, string(sessionJSON)); err != nil {
			// best-effort — don't fail the node over session persistence
		}
	}

	// Persist token usage summary to Dapr state so the UI/API can query
	// per-run token costs (mirrors the declarative pipeline's usage:last key).
	if e.dapr != nil && e.stateStore != "" && result.Usage.Total() > 0 {
		usageJSON, _ := json.Marshal(map[string]any{
			"workflow":  env.Workflow,
			"input":     result.Usage.Input,
			"output":    result.Usage.Output,
			"cacheRead": result.Usage.CacheRead,
			"cost":      result.Usage.Cost,
			"green":     result.Green,
			"attempts":  result.Attempts,
		})
		if err := e.dapr.SaveState(ctx, e.stateStore, env.Workflow+":usage:last", string(usageJSON)); err != nil {
			// best-effort — don't fail the node over usage persistence
		}
	}

	return NodeResult{
		Status: status,
		Outputs: NodeOutputs{
			"green":    result.Green,
			"attempts": result.Attempts,
			"usage":    result.Usage,
			// Agent metadata for the live wall (carried on node.completed
			// lifecycle events; the UI caches per workflow). Model is the
			// configured model id; turns is the conversation length of the
			// persisted session.
			"model": cfg.Model,
			"turns": len(result.Session.Turns),
		},
		Feedback: fmt.Sprintf("agent %s after %d attempt(s), %s", status, result.Attempts, result.Usage.String()),
	}, nil
}

// looksLikeRef returns true if the task string looks like a reference path
// (e.g. "tasks/wiki-update" or "configmap:my-task") rather than inline text.
func looksLikeRef(s string) bool {
	if len(s) == 0 {
		return false
	}
	// References are short, no spaces, contain a slash or colon.
	for _, c := range s {
		if c == ' ' || c == '\n' || c == '\t' {
			return false
		}
	}
	for _, c := range s {
		if c == '/' || c == ':' {
			return true
		}
	}
	return false
}
