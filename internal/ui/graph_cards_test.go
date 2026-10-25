package ui

import "testing"

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
