package graph

import (
	"context"

	"go.opentelemetry.io/otel/attribute"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/agent"
	"github.com/tibrezus/harmostes/internal/observability"
	"github.com/tibrezus/harmostes/internal/worker"
)

// GateExecutor runs a "gate" node — a validation plugin where exit 0 = green
// and the combined stdout+stderr becomes feedback. It wraps the existing
// worker.GatePlugin infrastructure.
type GateExecutor struct {
	resolver worker.PluginResolver
}

// NewGateExecutor creates a gate node executor.
func NewGateExecutor(resolver worker.PluginResolver) *GateExecutor {
	return &GateExecutor{resolver: resolver}
}

func (e *GateExecutor) Type() string        { return "gate" }
func (e *GateExecutor) Deterministic() bool { return true }

func (e *GateExecutor) Execute(ctx context.Context, node v1alpha1.NodeSpec, env NodeEnv) (NodeResult, error) {
	ctx, span := observability.Tracer().Start(ctx, "graph.node.gate")
	defer span.End()
	span.SetAttributes(
		attribute.String("harmostes.node.id", node.ID),
		attribute.String("harmostes.node.type", "gate"),
	)

	cfg, err := parseConfig[GateNodeConfig](node.Config)
	if err != nil {
		return NodeResult{Status: StatusFailed, Feedback: err.Error()}, err
	}

	command, args, err := e.resolver.Resolve(ctx, cfg.Plugin.ToPluginRef(), "gate")
	if err != nil {
		return NodeResult{Status: StatusFailed, Feedback: "resolve gate plugin: " + err.Error()}, err
	}

	// Reuse the existing GatePlugin adapter (exit 0 = green).
	gate := worker.GatePlugin{
		Command: command,
		Args:    args,
		Env:     envToPluginEnv(env, "gate", string(node.Config)),
		Name:    cfg.Plugin.Name,
	}

	green, feedback, gateErr := gate.Run(ctx)
	if gateErr != nil {
		// A failure to START the command (bad shell, missing executable) is a
		// system error, not a gate failure.
		span.SetAttributes(attribute.String("harmostes.gate.error", gateErr.Error()))
		return NodeResult{Status: StatusFailed, Feedback: gateErr.Error()}, gateErr
	}

	status := StatusFailed
	if green {
		status = StatusGreen
	}

	span.SetAttributes(
		attribute.String("harmostes.gate.status", string(status)),
		attribute.String("harmostes.gate.plugin", cfg.Plugin.Name),
	)

	return NodeResult{
		Status:   status,
		Outputs:  NodeOutputs{"green": green},
		Feedback: feedback,
	}, nil
}

// AsAgentGate adapts a gate node config to the agent.Gate interface, so the
// agent executor can use an inline gate config. This bridges the graph gate
// config to the existing agent loop without duplicating GatePlugin logic.
func (g GateNodeConfig) AsAgentGate(ctx context.Context, resolver worker.PluginResolver, env NodeEnv) (agent.Gate, error) {
	command, args, err := resolver.Resolve(ctx, g.Plugin.ToPluginRef(), "gate")
	if err != nil {
		return nil, err
	}
	return worker.GatePlugin{
		Command: command,
		Args:    args,
		Env:     envToPluginEnv(env, "gate", ""),
		Name:    g.Plugin.Name,
	}, nil
}
