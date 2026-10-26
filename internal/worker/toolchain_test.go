package worker

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestToolchainBaselineInImage enforces the toolchain contract (#786): every
// name the kernel promises template plugins must appear in the worker image
// recipe. The Dockerfile is the image; this file is the contract; the test
// is the drift guard (the #606 skill-drift pattern: a documented guarantee
// that cannot fail guards nothing).
func TestToolchainBaselineInImage(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	// internal/worker/toolchain_test.go → repo root is two up.
	dockerfile := filepath.Join(filepath.Dir(thisFile), "..", "..", "Dockerfile.worker")
	raw, err := os.ReadFile(dockerfile)
	if err != nil {
		t.Fatalf("read %s: %v — the contract test must run from the repo checkout", dockerfile, err)
	}
	// Only the RUNTIME stage counts: the build stage (golang) carries its
	// own toolchain that never ships.
	stage := raw
	if i := strings.Index(string(raw), "FROM node:"); i >= 0 {
		stage = raw[i:]
	} else {
		t.Fatal("worker image runtime stage (FROM node:*) not found — the recipe changed shape; update the contract test")
	}
	for _, tool := range ToolchainBaseline {
		if !strings.Contains(string(stage), tool) {
			t.Errorf("toolchain baseline: %q is promised to template plugins but absent from the worker image's runtime stage — restore it or cut a release whose notes drop it", tool)
		}
	}
}
