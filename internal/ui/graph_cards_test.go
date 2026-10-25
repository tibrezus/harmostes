package ui

import (
	"testing"

	"github.com/tibrezus/harmostes/api/v1alpha1"
)

// wrapTitle: long model ids split at a path separator — the FULL id renders
// (two lines), never an abbreviation (#502: model ids are exact).
func TestWrapTitleModel(t *testing.T) {
	t1, t2 := wrapTitle("litellm/test/fixture-reviewer")
	if t1 != "litellm/test/" || t2 != "fixture-reviewer" {
		t.Errorf("wrapTitle long = %q + %q", t1, t2)
	}
	t1, t2 = wrapTitle("litellm/zal/glm-5.3-flash")
	if t2 != "" || t1 != "litellm/zal/glm-5.3-flash" {
		t.Errorf("wrapTitle short = %q + %q, want single line", t1, t2)
	}
	t1, t2 = wrapTitle("some-model-without-separators-but-very-long-x")
	if t2 != "" || t1 == "some-model-without-separators-but-very-long-x" {
		t.Errorf("wrapTitle separatorless = %q + %q, want truncated single line", t1, t2)
	}
}

// cardDefs: a wrapped model id must reach the panel as the FULL id (#502) —
// the wrap exists only so the canvas can render it on two lines; the panel
// row concatenates the segments. The fixture-reviewer id (29 chars) wraps;
// the panel row shows it whole.
func TestCardDefsWrappedModelIdRendersWhole(t *testing.T) {
	spec := &v1alpha1.WorkflowSpec{Agent: v1alpha1.AgentSpec{Model: "litellm/test/fixture-reviewer"}}
	n := v1alpha1.NodeSpec{ID: "agent", Type: "agent"}
	defs := cardDefs(n, spec)
	if len(defs) == 0 || defs[0].Value != "litellm/test/fixture-reviewer" {
		t.Errorf("panel model row = %+v, want the full wrapped id", defs)
	}
}
