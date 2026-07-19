package graph

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/observability"
	"github.com/tibrezus/harmostes/internal/worker"
)

// PluginExecutor runs a "plugin" node — a deterministic shell-script block.
// It wraps the existing worker.RunPlugin + worker.PluginResolver infrastructure.
type PluginExecutor struct {
	resolver worker.PluginResolver
}

// NewPluginExecutor creates a plugin node executor.
func NewPluginExecutor(resolver worker.PluginResolver) *PluginExecutor {
	return &PluginExecutor{resolver: resolver}
}

func (e *PluginExecutor) Type() string        { return "plugin" }
func (e *PluginExecutor) Deterministic() bool { return true }

func (e *PluginExecutor) Execute(ctx context.Context, node v1alpha1.NodeSpec, env NodeEnv) (NodeResult, error) {
	ctx, span := observability.Tracer().Start(ctx, "graph.node.plugin")
	defer span.End()
	span.SetAttributes(
		attribute.String("harmostes.node.id", node.ID),
		attribute.String("harmostes.node.type", "plugin"),
	)

	cfg, err := parseConfig[PluginNodeConfig](node.Config)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return NodeResult{Status: StatusFailed, Feedback: err.Error()}, err
	}

	command, args, err := e.resolver.Resolve(ctx, cfg.ToPluginRef(), "plugin")
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return NodeResult{Status: StatusFailed, Feedback: "resolve plugin: " + err.Error()}, err
	}

	specJSON := string(node.Config)
	pluginEnv := envToPluginEnv(env, "plugin", specJSON)
	res, out, runErr := worker.RunPlugin(ctx, command, args, pluginEnv, nil)

	span.SetAttributes(
		attribute.String("harmostes.plugin.name", cfg.Name),
		attribute.String("harmostes.plugin.artifact", res.Artifact),
	)
	if res.Changed != nil {
		span.SetAttributes(attribute.Bool("harmostes.plugin.changed", *res.Changed))
	}

	if runErr != nil {
		span.SetStatus(codes.Error, "plugin exited non-zero")
		return NodeResult{
			Status:   StatusFailed,
			Feedback: out,
		}, nil // non-zero exit is a node failure, not a system error
	}

	outputs := NodeOutputs{
		"artifact": res.Artifact,
		"status":   res.Status,
	}
	if res.Changed != nil {
		outputs["changed"] = *res.Changed
	}

	return NodeResult{
		Status:   StatusGreen,
		Outputs:  outputs,
		Feedback: out,
	}, nil
}

// envToPluginEnv converts the graph NodeEnv to the existing worker.PluginEnv.
func envToPluginEnv(env NodeEnv, phase, specJSON string) worker.PluginEnv {
	return worker.PluginEnv{
		Workflow:     env.Workflow,
		Namespace:    env.Namespace,
		Phase:        phase,
		Spec:         specJSON,
		Source:       env.Source,
		Workdir:      env.Workdir,
		State:        env.State,
		SourceURL:    env.SourceURL,
		SourceBranch: env.SourceBranch,
	}
}
