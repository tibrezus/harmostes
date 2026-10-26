package worker

import (
	"context"
	"strings"
	"testing"

	v1meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tibrezus/harmostes/api/v1alpha1"
)

// The #786 materialization contract: composition is deterministic, delta is
// replace-by-name, and every pin that cannot be verified exactly refuses at
// DISPATCH — before a pod is burned, with the cause named (#504).
func TestComposeExtensions(t *testing.T) {
	uni := []v1alpha1.ExtensionPin{
		{Name: "litellm-provider", Source: "@tibrezus/litellm-provider@1.0.0", Integrity: "sha512-AAAA"},
		{Name: "sol-pi", Source: "@tibrezus/sol-pi@2.0.0", Integrity: "sha512-BBBB"},
	}
	delta := []v1alpha1.ExtensionPin{
		{Name: "sol-pi", Source: "@tibrezus/sol-pi@2.1.0", Integrity: "sha512-CCCC"}, // shadows
		{Name: "tpl-ext", Source: "tpl-ext@0.1.0", Integrity: "sha512-DDDD"},         // template-only
	}
	got := ComposeExtensions(uni, delta)
	want := []v1alpha1.ExtensionPin{
		{Name: "litellm-provider", Source: "@tibrezus/litellm-provider@1.0.0", Integrity: "sha512-AAAA"},
		{Name: "sol-pi", Source: "@tibrezus/sol-pi@2.1.0", Integrity: "sha512-CCCC"},
		{Name: "tpl-ext", Source: "tpl-ext@0.1.0", Integrity: "sha512-DDDD"},
	}
	if len(got) != len(want) {
		t.Fatalf("composed %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v (shadow must replace IN the universal slot; template-only entries follow)", i, got[i], want[i])
		}
	}

	// Both empty = nil — the inert contract: no manifest, no materialization
	// init, byte-identical Job.
	if got := ComposeExtensions(nil, nil); got != nil {
		t.Errorf("empty compose = %+v, want nil", got)
	}

	// Determinism: same inputs, same order — two dispatches must render
	// byte-identical manifests (the Job env is reproducible).
	rawA, err := v1alpha1.RenderExtensionManifest(ComposeExtensions(uni, delta))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	rawB, err := v1alpha1.RenderExtensionManifest(ComposeExtensions(uni, delta))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	a, b := rawA, rawB
	if a != b {
		t.Errorf("composition is not deterministic:\n%s\n%s", a, b)
	}
}

func TestValidateExtensionPins(t *testing.T) {
	ok := []v1alpha1.ExtensionPin{
		{Name: "left-pad", Source: "left-pad@1.3.0", Integrity: "sha512-yldyNfNwyFVNmWscHXCYLYj201+/dwR10T9XXpMtaFWPucRkaIXgPLk8DAsPBNzCxsGwDcSJUYgKsP0/1mdgw=="},
	}
	if err := v1alpha1.ValidateExtensionPins(ok); err != nil {
		t.Fatalf("valid pin refused: %v", err)
	}
	if err := v1alpha1.ValidateExtensionPins(ok[:0]); err != nil {
		t.Fatalf("empty set must validate (inert): %v", err)
	}

	cases := []struct {
		name string
		pin  v1alpha1.ExtensionPin
		want string
	}{
		{"range caret", v1alpha1.ExtensionPin{Name: "x", Source: "x@^1.2.3", Integrity: "sha512-AAAA"}, "not an exact semver"},
		{"range tilde", v1alpha1.ExtensionPin{Name: "x", Source: "x@~1.2.3", Integrity: "sha512-AAAA"}, "not an exact semver"},
		{"tag latest", v1alpha1.ExtensionPin{Name: "x", Source: "x@latest", Integrity: "sha512-AAAA"}, "not an exact semver"},
		{"bare name", v1alpha1.ExtensionPin{Name: "x", Source: "x", Integrity: "sha512-AAAA"}, "not an exact npm spec"},
		{"no spec", v1alpha1.ExtensionPin{Name: "x", Integrity: "sha512-AAAA"}, "source is required"},
		{"no name", v1alpha1.ExtensionPin{Source: "x@1.0.0", Integrity: "sha512-AAAA"}, "name is required"},
		{"integrity not sri", v1alpha1.ExtensionPin{Name: "x", Source: "x@1.0.0", Integrity: "deadbeef"}, "not an SRI"},
		{"integrity wrong algo", v1alpha1.ExtensionPin{Name: "x", Source: "x@1.0.0", Integrity: "sha256-AAAA"}, "not an SRI"},
		{"integrity empty", v1alpha1.ExtensionPin{Name: "x", Source: "x@1.0.0"}, "not an SRI"},
	}
	for _, tc := range cases {
		err := v1alpha1.ValidateExtensionPins([]v1alpha1.ExtensionPin{tc.pin})
		if err == nil {
			t.Errorf("%s: malformed pin accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not name %q", tc.name, err, tc.want)
		}
	}
}

func TestValidateHarnessPin(t *testing.T) {
	// Absent = fine (inert — the baked harness stands).
	if err := v1alpha1.ValidateHarnessPin(v1alpha1.HarnessRef{}); err != nil {
		t.Fatalf("absent pin refused: %v", err)
	}
	// Complete = fine.
	full := v1alpha1.HarnessRef{Runtime: "pi@1.3.0", Source: "@earendil-works/pi-coding-agent@1.3.0", Integrity: "sha512-yldyNfNwyFVNmWscHXCYLYj201+/dwR10T9XXpMtaFWPucRkaIXgPLk8DAsPBNzCxsGwDcSJUYgKsP0/1mdgw=="}
	if err := v1alpha1.ValidateHarnessPin(full); err != nil {
		t.Fatalf("complete pin refused: %v", err)
	}
	// PARTIAL pins refuse: a runtime declared without an exact source is a
	// pin that cannot be verified — it would silently no-op while looking
	// declared (#504). Same for a missing integrity.
	if err := v1alpha1.ValidateHarnessPin(v1alpha1.HarnessRef{Runtime: "pi@1.3.0"}); err == nil || !strings.Contains(err.Error(), "source is required") {
		t.Errorf("runtime-without-source accepted: %v", err)
	}
	if err := v1alpha1.ValidateHarnessPin(v1alpha1.HarnessRef{Runtime: "pi@1.3.0", Source: "@earendil-works/pi-coding-agent@1.3.0"}); err == nil || !strings.Contains(err.Error(), "not an SRI") {
		t.Errorf("pin-without-integrity accepted: %v", err)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	pins := []v1alpha1.ExtensionPin{
		{Name: "left-pad", Source: "left-pad@1.3.0", Integrity: "sha512-AAAA"},
		{Name: "@scoped/ext", Source: "@scoped/ext@0.2.1", Integrity: "sha512-BBBB"},
	}
	raw, err := v1alpha1.RenderExtensionManifest(pins)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(raw, `"name":"left-pad"`) || !strings.Contains(raw, `@scoped/ext@0.2.1`) {
		t.Errorf("manifest %q does not carry the pins", raw)
	}
	got, err := v1alpha1.ParseExtensionManifest(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 || got[1].Name != "@scoped/ext" {
		t.Errorf("round trip lost entries: %+v", got)
	}
	// Empty = "" (env unset — inert).
	if raw, _ := v1alpha1.RenderExtensionManifest(nil); raw != "" {
		t.Errorf("empty set rendered %q, want \"\"", raw)
	}
	// Malformed JSON refuses (#504: never a silently-empty set).
	if _, err := v1alpha1.ParseExtensionManifest("{not json"); err == nil {
		t.Error("malformed manifest accepted")
	}
	// Incomplete entries refuse.
	if _, err := v1alpha1.ParseExtensionManifest(`[{"name":"x"}]`); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("incomplete manifest accepted: %v", err)
	}
}

func TestHarnessManifestRoundTrip(t *testing.T) {
	raw, err := v1alpha1.RenderHarnessManifest(v1alpha1.HarnessRef{Runtime: "pi@1.3.0", Source: "@earendil-works/pi-coding-agent@1.3.0", Integrity: "sha512-AAAA"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got, err := v1alpha1.ParseHarnessManifest(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Runtime != "pi@1.3.0" || got.Source != "@earendil-works/pi-coding-agent@1.3.0" {
		t.Errorf("round trip drifted: %+v", got)
	}
	if raw, _ := v1alpha1.RenderHarnessManifest(v1alpha1.HarnessRef{}); raw != "" {
		t.Errorf("zero pin rendered %q, want \"\"", raw)
	}
	if _, err := v1alpha1.ParseHarnessManifest(`{"runtime":"pi@1.3.0"}`); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("partial harness pin accepted: %v", err)
	}
}

// validSRI is a SHAPE-CORRECT SRI (86 base64 chars + padding) for fixtures
// that exercise composition, not hashing — the real-hash paths live in the
// materialize tests.
func validSRI(tag byte) string { return "sha512-" + strings.Repeat(string(tag), 86) + "==" }

// #786 dispatch contract: composeRuntime reads the profile (universal set +
// harness pin) through the client, merges the workflow's template delta,
// validates every pin, and refuses malformed ones BEFORE any pod exists.
func TestComposeRuntimeDispatch(t *testing.T) {
	profile := &v1alpha1.AgentProfile{
		ObjectMeta: v1meta.ObjectMeta{Name: "default", Namespace: "default"},
		Spec: v1alpha1.AgentProfileSpec{
			Extensions: []v1alpha1.ExtensionPin{
				{Name: "sol-pi", Source: "@tibrezus/sol-pi@2.0.0", Integrity: validSRI('A')},
			},
		},
	}
	wf := &v1alpha1.Workflow{ObjectMeta: v1meta.ObjectMeta{Name: "wf", Namespace: "default"}}

	// No env, no template delta → inert.
	t.Setenv("HARMOSTES_AGENT_PROFILE", "")
	d := &Dispatcher{cl: nil, logf: func(string, ...any) {}}
	pins, harness, err := d.composeRuntime(context.Background(), "default", wf)
	if err != nil || pins != nil || harness != nil {
		t.Fatalf("inert compose = %+v, %v, %v; want nil,nil,nil", pins, harness, err)
	}

	// Profile universal ⊕ template delta, replace-by-name.
	t.Setenv("HARMOSTES_AGENT_PROFILE", "default")
	scheme := dispatchScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(profile).Build()
	d.cl = cl
	wf.Spec.Agent.Extensions = []v1alpha1.ExtensionPin{
		{Name: "sol-pi", Source: "@tibrezus/sol-pi@2.1.0", Integrity: validSRI('B')}, // shadows
		{Name: "tpl-ext", Source: "tpl-ext@0.1.0", Integrity: validSRI('C')},
	}
	pins, harness, err = d.composeRuntime(context.Background(), "default", wf)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if len(pins) != 2 || pins[0].Integrity != validSRI('B') || pins[1].Name != "tpl-ext" {
		t.Fatalf("composed = %+v; want shadow-in-place + template-only appended", pins)
	}
	if harness != nil {
		t.Fatalf("no harness declared, got %v", harness)
	}

	// A malformed template pin refuses AT DISPATCH, before any pod burns.
	wf.Spec.Agent.Extensions = []v1alpha1.ExtensionPin{
		{Name: "risky", Source: "risky@latest", Integrity: validSRI('D')},
	}
	if _, _, err := d.composeRuntime(context.Background(), "default", wf); err == nil || !strings.Contains(err.Error(), "risky") {
		t.Fatalf("range-spec pin not refused at dispatch: %v", err)
	}

	// The profile's harness pin flows through (valid pin).
	profile.Spec.Harness = v1alpha1.HarnessRef{
		Runtime: "pi@1.3.0", Source: "@earendil-works/pi-coding-agent@1.3.0", Integrity: validSRI('E'),
	}
	if err := cl.Update(context.Background(), profile); err != nil {
		t.Fatal(err)
	}
	wf.Spec.Agent.Extensions = nil
	pins, harness, err = d.composeRuntime(context.Background(), "default", wf)
	if err != nil || harness == nil || harness.Runtime != "pi@1.3.0" || len(pins) != 1 {
		t.Fatalf("harness passthrough = %+v, %v, %v", pins, harness, err)
	}
}
