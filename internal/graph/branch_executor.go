package graph

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"text/template"

	"go.opentelemetry.io/otel/attribute"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/observability"
)

// BranchExecutor runs a "branch" node — a deterministic template condition
// evaluator. It renders the condition template against the node inputs and
// outputs { changed: bool } based on whether the result is truthy.
//
// The template receives a data structure with .Nodes, where each node maps
// to its outputs:
//
//	{{ index (index .Nodes "prepare") "changed" }}
//
// The rendered string is trimmed and compared case-insensitively to "true".
// Any render error is treated as "false" (condition not met).
type BranchExecutor struct{}

// NewBranchExecutor creates a branch node executor.
func NewBranchExecutor() *BranchExecutor {
	return &BranchExecutor{}
}

func (e *BranchExecutor) Type() string        { return "branch" }
func (e *BranchExecutor) Deterministic() bool { return true }

func (e *BranchExecutor) Execute(ctx context.Context, node v1alpha1.NodeSpec, env NodeEnv) (NodeResult, error) {
	ctx, span := observability.Tracer().Start(ctx, "graph.node.branch")
	defer span.End()
	span.SetAttributes(
		attribute.String("harmostes.node.id", node.ID),
		attribute.String("harmostes.node.type", "branch"),
	)

	cfg, err := parseConfig[BranchNodeConfig](node.Config)
	if err != nil {
		return NodeResult{Status: StatusFailed, Feedback: err.Error()}, err
	}

	changed := evaluateCondition(cfg.Condition, env.Inputs)

	span.SetAttributes(
		attribute.Bool("harmostes.branch.changed", changed),
		attribute.String("harmostes.branch.condition", cfg.Condition),
	)

	return NodeResult{
		Status: StatusGreen, // branch always "succeeds" — it routes
		Outputs: NodeOutputs{
			"changed": changed,
		},
		Feedback: fmt.Sprintf("branch: changed=%v", changed),
	}, nil
}

// evaluateCondition renders the template against the inputs and returns true if
// the result is "true" (case-insensitive). Errors default to false.
func evaluateCondition(condition string, inputs map[string]NodeOutputs) bool {
	if condition == "" {
		return false
	}

	tmpl, err := template.New("branch").Parse(condition)
	if err != nil {
		return false
	}

	data := struct {
		Nodes map[string]NodeOutputs
	}{
		Nodes: inputs,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return false
	}

	result := strings.TrimSpace(buf.String())
	return strings.EqualFold(result, "true")
}
