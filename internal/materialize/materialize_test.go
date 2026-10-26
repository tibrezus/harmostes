package materialize

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// sriOf hashes content into the SRI form the pins carry.
func sriOf(b []byte) string {
	sum := sha512.Sum512(b)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

// fakeRegistry serves a packument + one tarball. The tarball carries a
// package/ root with the given files (path → content). integrity is the
// dist.integrity the registry REPORTS (deliberately separate from the
// tarball's real hash so the mismatch paths are exercisable).
func fakeRegistry(t *testing.T, pkg, version string, distIntegrity string, tarball []byte) *httptest.Server {
	t.Helper()
	var tarballURL string // set once the server exists
	mux := http.NewServeMux()
	mux.HandleFunc("/"+escapePkgName(pkg), func(w http.ResponseWriter, r *http.Request) {
		pk := map[string]any{
			"versions": map[string]any{
				version: map[string]any{
					"dist": map[string]string{"tarball": tarballURL, "integrity": distIntegrity},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(pk)
	})
	mux.HandleFunc("/tarball", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	})
	srv := httptest.NewServer(mux)
	tarballURL = srv.URL + "/tarball"
	t.Cleanup(srv.Close)
	return srv
}

// buildTarball produces a gzipped npm-style tarball (package/ root).
func buildTarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "package/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		mode := int64(0o644)
		if strings.HasSuffix(name, ".sh") || !strings.Contains(name, ".") {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: "package/" + name, Mode: mode, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFetchExtensionHappyPath(t *testing.T) {
	tarball := buildTarball(t, map[string]string{
		"package.json": `{"name":"left-pad","version":"1.3.0","main":"index.js"}`,
		"index.js":     `module.exports = pad;`,
	})
	sri := sriOf(tarball)
	srv := fakeRegistry(t, "left-pad", "1.3.0", sri, tarball)

	dest := t.TempDir()
	err := FetchExtension(context.Background(), RegistryBase(srv.URL), srv.Client(), "left-pad@1.3.0", sri, dest)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "left-pad", "index.js"))
	if err != nil {
		t.Fatalf("extracted file missing: %v", err)
	}
	if string(got) != "module.exports = pad;" {
		t.Errorf("extracted content drifted: %q", got)
	}
	// The package/ root must be STRIPPED (files land directly in dest/<name>/).
	if _, err := os.Stat(filepath.Join(dest, "left-pad", "package")); err == nil {
		t.Error("npm root package/ was not stripped")
	}
}

func TestFetchExtensionIntegrityMismatchRefuses(t *testing.T) {
	tarball := buildTarball(t, map[string]string{"index.js": "content"})
	realSRI := sriOf(tarball)
	wrongPin := sriOf([]byte("different bytes entirely"))

	// (a) The REGISTRY's dist disagrees with the pin — refuse before any
	// download (the pin is the contract; a registry reshuffle never wins).
	srv := fakeRegistry(t, "left-pad", "1.3.0", wrongPin, tarball)
	dest := t.TempDir()
	err := FetchExtension(context.Background(), RegistryBase(srv.URL), srv.Client(), "left-pad@1.3.0", realSRI, dest)
	if err == nil || !strings.Contains(err.Error(), "integrity mismatch") {
		t.Fatalf("registry-vs-pin mismatch not refused: %v", err)
	}

	// (b) The registry AGREES but the bytes differ (a compromised mirror) —
	// the byte-hash check catches what the metadata check cannot.
	srv2 := fakeRegistry(t, "left-pad", "1.3.0", realSRI, buildTarball(t, map[string]string{"index.js": "TROJANED"}))
	err = FetchExtension(context.Background(), RegistryBase(srv2.URL), srv2.Client(), "left-pad@1.3.0", realSRI, dest)
	if err == nil || !strings.Contains(err.Error(), "integrity mismatch") {
		t.Fatalf("byte-level mismatch not refused: %v", err)
	}
	// Nothing was installed by either refusal.
	if _, err := os.Stat(filepath.Join(dest, "left-pad")); err == nil {
		t.Error("a refused fetch left content on disk")
	}
}

func TestFetchExtensionUnknownVersionRefuses(t *testing.T) {
	tarball := buildTarball(t, map[string]string{"index.js": "x"})
	srv := fakeRegistry(t, "left-pad", "1.3.0", sriOf(tarball), tarball)
	err := FetchExtension(context.Background(), RegistryBase(srv.URL), srv.Client(), "left-pad@9.9.9", sriOf(tarball), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unresolvable pin not refused: %v", err)
	}
}

func TestFetchExtensionZipSlipRefuses(t *testing.T) {
	// A hostile tarball whose entry climbs out of the extraction dir.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	payload := "owned"
	_ = tw.WriteHeader(&tar.Header{Name: "package/../../escape.sh", Mode: 0o755, Size: int64(len(payload))})
	_, _ = tw.Write([]byte(payload))
	_ = tw.Close()
	_ = gz.Close()
	sri := sriOf(buf.Bytes())
	srv := fakeRegistry(t, "evil", "1.0.0", sri, buf.Bytes())

	outer := t.TempDir()
	dest := filepath.Join(outer, "dest")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	err := FetchExtension(context.Background(), RegistryBase(srv.URL), srv.Client(), "evil@1.0.0", sri, dest)
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("zip-slip not refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outer, "escape.sh")); err == nil {
		t.Fatal("the traversal WROTE outside the destination")
	}
}

func TestFetchHarnessBinResolution(t *testing.T) {
	// Map-form bin (the pi package's shape) + a nested bin dir.
	tarball := buildTarball(t, map[string]string{
		"package.json":  `{"name":"@earendil-works/pi-coding-agent","version":"1.3.0","bin":{"pi":"bin/pi.js"}}`,
		"bin/pi.js":     "#!/usr/bin/env node",
		"dist/index.js": "// runtime",
	})
	sri := sriOf(tarball)
	srv := fakeRegistry(t, "@earendil-works/pi-coding-agent", "1.3.0", sri, tarball)

	dest := t.TempDir()
	err := FetchHarness(context.Background(), RegistryBase(srv.URL), srv.Client(), "@earendil-works/pi-coding-agent@1.3.0", sri, dest)
	if err != nil {
		t.Fatalf("fetch harness: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dest, ".pi-path"))
	if err != nil {
		t.Fatalf(".pi-path missing: %v", err)
	}
	piPath := strings.TrimSpace(string(raw))
	if !strings.HasSuffix(piPath, filepath.Join("@earendil-works/pi-coding-agent", "bin", "pi.js")) {
		t.Errorf(".pi-path = %q, want the resolved bin path", piPath)
	}
	if _, err := os.Stat(piPath); err != nil {
		t.Errorf(".pi-path names a missing file: %v", err)
	}
}

func TestFetchHarnessAmbiguousBinRefuses(t *testing.T) {
	tarball := buildTarball(t, map[string]string{
		"package.json": `{"name":"multi","version":"1.0.0","bin":{"a":"a.js","b":"b.js"}}`,
		"a.js":         "x",
		"b.js":         "y",
	})
	sri := sriOf(tarball)
	srv := fakeRegistry(t, "multi", "1.0.0", sri, tarball)
	err := FetchHarness(context.Background(), RegistryBase(srv.URL), srv.Client(), "multi@1.0.0", sri, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous bin not refused: %v", err)
	}
}

func TestProjectPlugins(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	labeled := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "tpl-plugins", Labels: map[string]string{"harmostes.dev/plugin": "true"}},
		Data:       map[string]string{"greet.sh": "#!/bin/sh\necho hi\n"},
	}
	unlabeled := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "other"},
		Data:       map[string]string{"x.sh": "y"},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(labeled, unlabeled).Build()
	dest := t.TempDir()
	n, err := ProjectPlugins(context.Background(), cl, "ns", "true", dest)
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if n != 1 {
		t.Fatalf("projected %d ConfigMaps, want 1 (only the labeled one)", n)
	}
	got, err := os.ReadFile(filepath.Join(dest, "tpl-plugins", "greet.sh"))
	if err != nil {
		t.Fatalf("projected script missing: %v", err)
	}
	if !strings.HasPrefix(string(got), "#!/bin/sh") {
		t.Errorf("content drifted: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "other")); err == nil {
		t.Error("the UNLABELED ConfigMap was projected — the label is the contract")
	}
	// Zero matches is legal (count 0, no error) — a namespace may simply not
	// carry template plugins yet.
	n, err = ProjectPlugins(context.Background(), cl, "ns", "absent-value", dest)
	if err != nil || n != 0 {
		t.Fatalf("zero-match projection = %d, %v; want 0, nil", n, err)
	}
}

// --- fake-client plumbing -------------------------------------------------
