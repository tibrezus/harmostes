package graph

import (
	"context"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

func TestBranchExecutorTrue(t *testing.T) {
	exec := NewBranchExecutor()

	node := v1alpha1.NodeSpec{
		ID:   "check",
		Type: "branch",
		Config: mustJSON(t, BranchNodeConfig{
			Condition: "{{ index (index .Nodes \"prepare\") \"changed\" }}",
		}),
	}

	env := NodeEnv{
		Inputs: map[string]NodeOutputs{
			"prepare": {"changed": "true"},
		},
	}

	result, err := exec.Execute(context.Background(), node, env)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}
	if changed, ok := result.Outputs["changed"].(bool); !ok || !changed {
		t.Errorf("changed = %v, want true", result.Outputs["changed"])
	}
}

func TestBranchExecutorFalse(t *testing.T) {
	exec := NewBranchExecutor()

	node := v1alpha1.NodeSpec{
		ID:   "check",
		Type: "branch",
		Config: mustJSON(t, BranchNodeConfig{
			Condition: "{{ index (index .Nodes \"prepare\") \"changed\" }}",
		}),
	}

	env := NodeEnv{
		Inputs: map[string]NodeOutputs{
			"prepare": {"changed": "false"},
		},
	}

	result, err := exec.Execute(context.Background(), node, env)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if changed, ok := result.Outputs["changed"].(bool); !ok || changed {
		t.Errorf("changed = %v, want false", result.Outputs["changed"])
	}
}

func TestBranchExecutorMissingNode(t *testing.T) {
	exec := NewBranchExecutor()

	node := v1alpha1.NodeSpec{
		ID:   "check",
		Type: "branch",
		Config: mustJSON(t, BranchNodeConfig{
			Condition: "{{ index (index .Nodes \"nonexistent\") \"changed\" }}",
		}),
	}

	result, err := exec.Execute(context.Background(), node, NodeEnv{Inputs: map[string]NodeOutputs{}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Missing node → template error → false
	if changed, ok := result.Outputs["changed"].(bool); !ok || changed {
		t.Errorf("changed = %v, want false on missing node", result.Outputs["changed"])
	}
}

func TestBranchExecutorEmptyCondition(t *testing.T) {
	exec := NewBranchExecutor()

	node := v1alpha1.NodeSpec{
		ID:     "check",
		Type:   "branch",
		Config: mustJSON(t, BranchNodeConfig{Condition: ""}),
	}

	result, err := exec.Execute(context.Background(), node, NodeEnv{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if changed, ok := result.Outputs["changed"].(bool); !ok || changed {
		t.Errorf("changed = %v, want false on empty condition", result.Outputs["changed"])
	}
}

func TestBranchEvaluatorDirect(t *testing.T) {
	tests := []struct {
		condition string
		inputs    map[string]NodeOutputs
		want      bool
	}{
		{
			condition: "{{ index (index .Nodes \"a\") \"v\" }}",
			inputs:    map[string]NodeOutputs{"a": {"v": "true"}},
			want:      true,
		},
		{
			condition: "{{ index (index .Nodes \"a\") \"v\" }}",
			inputs:    map[string]NodeOutputs{"a": {"v": "false"}},
			want:      false,
		},
		{
			condition: "{{ index (index .Nodes \"a\") \"v\" }}",
			inputs:    map[string]NodeOutputs{"a": {"v": "TRUE"}},
			want:      true, // case-insensitive
		},
		{
			condition: "invalid template {{{",
			inputs:    map[string]NodeOutputs{},
			want:      false, // parse error → false
		},
		{
			condition: "",
			inputs:    nil,
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.condition, func(t *testing.T) {
			got := evaluateCondition(tt.condition, tt.inputs)
			if got != tt.want {
				t.Errorf("evaluateCondition(%q) = %v, want %v", tt.condition, got, tt.want)
			}
		})
	}
}
