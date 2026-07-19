package graph

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/agent"
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
	runner   AgentRunner
	tasks    TaskResolver
	resolver worker.PluginResolver // for inline gate resolution
}

// NewAgentExecutor creates an agent node executor. The resolver is used to
// resolve inline gate plugins; it may be nil if gates are always separate
// nodes.
func NewAgentExecutor(runner AgentRunner, tasks TaskResolver, resolver worker.PluginResolver) *AgentExecutor {
	return &AgentExecutor{runner: runner, tasks: tasks, resolver: resolver}
}

func (e *AgentExecutor) Type() string        { return "agent" }
func (e *AgentExecutor) Deterministic() bool { return false }

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
	result, err := e.runner.Run(ctx, task, gate, maxFixes, nil)
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
	)

	return NodeResult{
		Status: status,
		Outputs: NodeOutputs{
			"green":    result.Green,
			"attempts": result.Attempts,
		},
		Feedback: fmt.Sprintf("agent %s after %d attempt(s)", status, result.Attempts),
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
