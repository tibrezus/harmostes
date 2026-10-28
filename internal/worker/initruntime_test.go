package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tibrezus/harmostes/api/v1alpha1"
)

// #786 run-form contract: a DECLARED runtime set must never silently shrink
// — every declared extension resolves to a materialized directory and a
// declared harness to a materialized pi binary, or the run refuses with the
// artifact named (#504).
func TestConsumeMaterializedRuntime(t *testing.T) {
	extManifest, _ := v1alpha1.RenderExtensionManifest([]v1alpha1.ExtensionPin{
		{Name: "left-pad", Source: "left-pad@1.3.0", Integrity: "sha512-AAAA"},
		{Name: "tpl-ext", Source: "tpl-ext@0.1.0", Integrity: "sha512-BBBB"},
	})
	harnessManifest, _ := v1alpha1.RenderHarnessManifest(v1alpha1.HarnessRef{
		Runtime: "pi@1.3.0", Source: "@earendil-works/pi-coding-agent@1.3.0", Integrity: "sha512-CCCC",
	})

	root := t.TempDir()
	extRoot := filepath.Join(root, "extensions")
	harnessRoot := filepath.Join(root, "harness")
	if err := os.MkdirAll(harnessRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"left-pad", "tpl-ext"} {
		if err := os.MkdirAll(filepath.Join(extRoot, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	piPath := filepath.Join(root, "pi", "pi.js")
	if err := os.MkdirAll(filepath.Dir(piPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(piPath, []byte("#!/usr/bin/env node"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(harnessRoot, ".pi-path"), []byte(piPath+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	env := map[string]string{
		EnvExtensionManifest: extManifest,
		EnvHarnessManifest:   harnessManifest,
	}
	getenv := func(k string) string { return env[k] }
	stat := func(p string) (os.FileInfo, error) { return os.Stat(p) }

	mat, err := consumeMaterializedRuntime(getenv, extRoot, harnessRoot, PluginsRoot, stat)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(mat.ExtensionPaths) != 2 || filepath.Base(mat.ExtensionPaths[1]) != "tpl-ext" {
		t.Errorf("extension paths = %v, want both in manifest order", mat.ExtensionPaths)
	}
	if mat.PiPath != piPath {
		t.Errorf("pi path = %q, want %q", mat.PiPath, piPath)
	}

	// A torn pod: the second extension's directory is missing — the run
	// refuses with the ARTIFACT NAMED, never a silently-smaller set.
	if err := os.RemoveAll(filepath.Join(extRoot, "tpl-ext")); err != nil {
		t.Fatal(err)
	}
	_, err = consumeMaterializedRuntime(getenv, extRoot, harnessRoot, PluginsRoot, stat)
	if err == nil || !strings.Contains(err.Error(), "tpl-ext") || !strings.Contains(err.Error(), "not materialized") {
		t.Fatalf("shrunken set not refused with the artifact named: %v", err)
	}

	// A declared harness without .pi-path refuses the same way (a FRESH
	// empty harness root — the happy path's .pi-path must not leak in).
	emptyHarness := filepath.Join(root, "empty-harness")
	if err := os.MkdirAll(emptyHarness, 0o755); err != nil {
		t.Fatal(err)
	}
	env2 := map[string]string{EnvHarnessManifest: harnessManifest}
	_, err = consumeMaterializedRuntime(func(k string) string { return env2[k] }, extRoot, emptyHarness, PluginsRoot, stat)
	if err == nil || !strings.Contains(err.Error(), "is missing") {
		t.Fatalf("harness without .pi-path not refused: %v", err)
	}

	// Nothing declared = zero effect (the inert default).
	mat, err = consumeMaterializedRuntime(func(string) string { return "" }, extRoot, harnessRoot, PluginsRoot, stat)
	if err != nil || mat.ExtensionPaths != nil || mat.PiPath != "" {
		t.Fatalf("inert consume = %+v, %v; want empty, nil", mat, err)
	}
}

// TestConsumeMaterializedRuntime_PluginRefs pins the run form's explicit-ref
// verification (three-tier 1c): a declared ref with a projected dir passes;
// a declared ref WITHOUT its dir is the torn-pod class — refuse with the
// name (#504); no refs env = inert.
func TestConsumeMaterializedRuntime_PluginRefs(t *testing.T) {
	root := t.TempDir()
	pluginsRoot := filepath.Join(root, "plugins")
	if err := os.MkdirAll(filepath.Join(pluginsRoot, "tpl-own-plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		if k == EnvPluginRefs {
			return "tpl-own-plugins"
		}
		return ""
	}
	stat := func(p string) (os.FileInfo, error) { return os.Stat(p) }

	if _, err := consumeMaterializedRuntime(getenv, filepath.Join(root, "ext"), filepath.Join(root, "harness"), pluginsRoot, stat); err != nil {
		t.Fatalf("materialized ref refused: %v", err)
	}

	// The torn pod: the dir is gone — the run refuses with the ref named.
	if err := os.RemoveAll(filepath.Join(pluginsRoot, "tpl-own-plugins")); err != nil {
		t.Fatal(err)
	}
	_, err := consumeMaterializedRuntime(getenv, filepath.Join(root, "ext"), filepath.Join(root, "harness"), pluginsRoot, stat)
	if err == nil || !strings.Contains(err.Error(), "tpl-own-plugins") || !strings.Contains(err.Error(), "not materialized") {
		t.Fatalf("shrunken refs not refused with the name: %v", err)
	}

	// No refs env: inert.
	mat, err := consumeMaterializedRuntime(func(string) string { return "" }, filepath.Join(root, "ext"), filepath.Join(root, "harness"), pluginsRoot, stat)
	if err != nil || mat.ExtensionPaths != nil {
		t.Fatalf("inert consume = %+v, %v", mat, err)
	}
}
