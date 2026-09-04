package worker

import (
	"reflect"
	"strings"
	"testing"

	"github.com/tibrezus/harmostes/api/v1alpha1"
)

// The agent's pi invocation always loads both in-image extensions: the
// litellm provider (model resolution) and rig-query (architecture-graph
// navigation, ADR-0009). rig-query degrades gracefully when prepare emitted
// no graph, so unconditional loading is safe for every workflow.
func TestPiArgsLoadBuiltinExtensions(t *testing.T) {
	args := PiArgs(v1alpha1.AgentSpec{Skill: "pr-review", Model: "litellm/test-model"})
	joined := strings.Join(args, " ")
	for _, ext := range []string{"/extensions/litellm-provider", "/extensions/rig-query"} {
		if !strings.Contains(joined, ext) {
			t.Errorf("PiArgs must load %s, got %v", ext, args)
		}
	}
	// -e flags must pair with their path argument (flag, value alternating).
	for i, a := range args {
		if a == "-e" && i+1 < len(args) && !strings.HasPrefix(args[i+1], "/extensions/") {
			t.Errorf("-e must be followed by an extension path, got %q", args[i+1])
		}
	}
	want := []string{"--skill", "pr-review", "--model", "litellm/test-model"}
	if got := args[len(args)-len(want):]; !reflect.DeepEqual(got, want) {
		t.Errorf("skill/model args lost: tail = %v, want %v", got, want)
	}
}

// --tools is an ALLOWLIST in pi: it replaces the whole tool set, extension
// tools included. A workflow declaring spec.agent.tools would silently lose
// rig while the task contract still mandates it — PiArgs appends it.
func TestPiArgsToolsAllowlistKeepsRig(t *testing.T) {
	args := PiArgs(v1alpha1.AgentSpec{Skill: "s", Model: "m", Tools: []string{"bash", "read"}})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--tools bash,read,rig") {
		t.Errorf("tools allowlist must gain rig, got: %s", joined)
	}
	// Already declared → not duplicated.
	args = PiArgs(v1alpha1.AgentSpec{Skill: "s", Model: "m", Tools: []string{"bash", "rig"}})
	joined = strings.Join(args, " ")
	if !strings.Contains(joined, "--tools bash,rig") || strings.Contains(joined, "rig,rig") {
		t.Errorf("declared rig must not be duplicated, got: %s", joined)
	}
}
