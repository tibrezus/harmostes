// Package materialize is the runtime-materialization engine (ADR-0011
// amendment, three-tier 1b / #786): it turns DECLARED pins — exact npm
// specs + SRI integrity hashes — into on-disk content inside the attempt
// pod, before the run form starts. Three surfaces:
//
//   - Extension: fetch the exact npm tarball, verify sha512 against THE PIN
//     (never the registry's own word — the pin is the contract), extract
//     zip-slip-safe into /runtime/extensions/<name>.
//   - Harness: the same fetch+verify for the pinned runtime, plus bin
//     resolution — the absolute pi path lands in /runtime/harness/.pi-path
//     for the run form's RPCOptions.PiPath.
//   - Plugins: project every ConfigMap labeled harmostes.dev/plugin=<value>
//     in the namespace into /plugins/<cm>/<key> — the aggregated plugin
//     mount, template-count-agnostic.
//
// Fail-loud is the WHOLE contract (#504): any unverifiable pin, any
// mismatched hash, any traversal attempt returns a NAMED error — the
// init-runtime subcommand prints it and exits non-zero, the Job dies, and
// the refusal is visible in the pod events. A silently-empty extension set
// is the one outcome this package exists to make impossible.
package materialize

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RegistryBase is the npm registry root; overridable per call (tests inject
// an httptest server; an air-gapped future injects a mirror).
type RegistryBase string

const DefaultRegistry RegistryBase = "https://registry.npmjs.org"

// httpClient bounds every fetch — the init runs inside the Job's wall clock
// and a hung registry must fail the pod, not the run bound.
var httpClient = &http.Client{Timeout: 60 * time.Second}

// packument is the sliver of the npm registry document this package reads.
type packument struct {
	Versions map[string]struct {
		Dist struct {
			Tarball   string `json:"tarball"`
			Integrity string `json:"integrity"`
		} `json:"dist"`
	} `json:"versions"`
}

// ParseNPMSpec splits an exact spec "path@X.Y.Z" at the LAST @ — scoped
// names (@scope/pkg@1.2.3) keep their @. The caller validated the shape
// (v1alpha1.ValidateNPMSpec); this is the mechanical split.
func ParseNPMSpec(spec string) (name, version string, err error) {
	i := strings.LastIndex(spec, "@")
	if i <= 0 {
		return "", "", fmt.Errorf("spec %q is not \"path@version\"", spec)
	}
	return spec[:i], spec[i+1:], nil
}

// FetchExtension fetches, verifies, and installs one pinned extension into
// destDir/<name>. Integrity is checked in BOTH places it can be checked:
// the packument's dist entry must MATCH the pin (a registry-side reshuffle
// never silently upgrades a pinned artifact) and the downloaded bytes must
// hash to the pin (the transport's word is nothing).
func FetchExtension(ctx context.Context, reg RegistryBase, hc *http.Client, spec, integrity, destDir string) error {
	name, version, err := ParseNPMSpec(spec)
	if err != nil {
		return fmt.Errorf("extension %s: %w", nameFor(spec), err)
	}
	return fetchAndExtract(ctx, reg, hc, name, version, integrity, filepath.Join(destDir, name))
}

// FetchHarness fetches, verifies, and installs the pinned runtime into
// destDir, resolving the package's bin entry to an absolute path written to
// destDir/.pi-path (the run form's RPCOptions.PiPath contract).
func FetchHarness(ctx context.Context, reg RegistryBase, hc *http.Client, spec, integrity, destDir string) error {
	name, version, err := ParseNPMSpec(spec)
	if err != nil {
		return fmt.Errorf("harness %s: %w", nameFor(spec), err)
	}
	pkgDir := filepath.Join(destDir, name)
	if err := fetchAndExtract(ctx, reg, hc, name, version, integrity, pkgDir); err != nil {
		return err
	}
	bin, err := resolveBin(pkgDir)
	if err != nil {
		return fmt.Errorf("harness %s: %w", spec, err)
	}
	abs, err := filepath.Abs(bin)
	if err != nil {
		return fmt.Errorf("harness %s: resolve bin path: %w", spec, err)
	}
	if err := os.WriteFile(filepath.Join(destDir, ".pi-path"), []byte(abs), 0o644); err != nil {
		return fmt.Errorf("harness %s: write .pi-path: %w", spec, err)
	}
	return nil
}

// resolveBin reads package.json's bin field (string or map form) and returns
// the package-relative executable path. Exactly one bin expected — a runtime
// package with zero or ambiguous bins is a pinning error (#504), not a
// best-effort guess.
func resolveBin(pkgDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(pkgDir, "package.json"))
	if err != nil {
		return "", fmt.Errorf("read package.json: %w", err)
	}
	var pkg struct {
		Bin json.RawMessage `json:"bin"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return "", fmt.Errorf("parse package.json: %w", err)
	}
	if len(pkg.Bin) == 0 {
		return "", fmt.Errorf("package declares no bin entry — nothing to execute")
	}
	var binPath string
	if s := string(pkg.Bin); strings.HasPrefix(s, "\"") {
		// String form: {"bin": "cli.js"} — unmarshal the raw JSON string.
		var single string
		if err := json.Unmarshal(pkg.Bin, &single); err != nil {
			return "", fmt.Errorf("bin field is not a valid string: %w", err)
		}
		binPath = single
	} else {
		var m map[string]string
		if err := json.Unmarshal(pkg.Bin, &m); err != nil {
			return "", fmt.Errorf("bin field is neither string nor map")
		}
		if len(m) != 1 {
			return "", fmt.Errorf("bin map has %d entries — ambiguous runtime, refusing (want exactly one)", len(m))
		}
		for _, p := range m {
			binPath = p
		}
	}
	return filepath.Join(pkgDir, binPath), nil
}

// fetchAndExtract is the shared engine: packument → pin-vs-registry
// integrity agreement → tarball download → byte-hash verification →
// zip-slip-safe extraction with the package/ root stripped.
func fetchAndExtract(ctx context.Context, reg RegistryBase, hc *http.Client, name, version, integrity, destDir string) error {
	// 1. Packument: the registry's word on what this version IS.
	url := fmt.Sprintf("%s/%s", strings.TrimSuffix(string(reg), "/"), escapePkgName(name))
	pk, err := getJSON(ctx, hc, url)
	if err != nil {
		return fmt.Errorf("packument %s: %w", name, err)
	}
	v, ok := pk.Versions[version]
	if !ok {
		return fmt.Errorf("packument %s: version %s not found — the pin does not resolve (registry has %d versions)", name, version, len(pk.Versions))
	}
	// 2. The registry's integrity for this version must MATCH the pin. A
	// disagreement is fatal: either the pin is stale or the registry moved —
	// both mean "do not proceed" (#504; the #502 stall class never ships).
	if v.Dist.Integrity != integrity {
		return fmt.Errorf("integrity mismatch for %s@%s: pin says %s, registry says %s — refusing (verify the pin or re-derive it)", name, version, shortSRI(integrity), shortSRI(v.Dist.Integrity))
	}
	// 3. Download + hash + extract.
	tarball, err := getBytes(ctx, hc, v.Dist.Tarball)
	if err != nil {
		return fmt.Errorf("tarball %s@%s: %w", name, version, err)
	}
	if err := verifySRI(tarball, integrity); err != nil {
		return fmt.Errorf("tarball %s@%s: %w", name, version, err)
	}
	if err := extractPkgTarball(tarball, destDir); err != nil {
		return fmt.Errorf("extract %s@%s: %w", name, version, err)
	}
	return nil
}

// escapePkgName URL-encodes scoped names (@scope/pkg → @scope%2Fpkg); plain
// names pass through.
func escapePkgName(name string) string {
	if strings.HasPrefix(name, "@") && strings.Contains(name, "/") {
		return strings.Replace(name, "/", "%2F", 1)
	}
	return name
}

// extractPkgTarball unpacks an npm tarball (gzipped tar with a package/
// root) into destDir, stripping the root. The zip-slip guard is absolute:
// every entry must resolve INSIDE destDir — a tarball that reaches out of
// its directory is a hostile artifact and refuses by name (#504; the
// delivery contract's trust boundary ends at the integrity hash, which a
// path traversal would render meaningless anyway).
func extractPkgTarball(tarball []byte, destDir string) error {
	gz, err := gzip.NewReader(strings.NewReader(string(tarball)))
	if err != nil {
		return fmt.Errorf("gunzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		// Strip the npm root (package/ or ./) and clean.
		rel := hdr.Name
		rel = strings.TrimPrefix(rel, "package/")
		rel = strings.TrimPrefix(rel, "./")
		if rel == "" {
			continue
		}
		clean := path.Clean(rel)
		if strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
			return fmt.Errorf("tar entry %q escapes the extraction directory — hostile artifact, refusing", hdr.Name)
		}
		dest := filepath.Join(destDir, filepath.FromSlash(clean))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(hdr.Mode) & 0o777
			if mode == 0 {
				mode = 0o644
			}
			if err := os.WriteFile(dest, nil, mode); err != nil {
				return err
			}
			f, err := os.OpenFile(dest, os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("write %s: %w", rel, err)
			}
			f.Close()
		case tar.TypeSymlink:
			// npm packages legitimately ship relative symlinks (bin
			// shims); absolute or escaping targets refuse.
			target := filepath.FromSlash(hdr.Linkname)
			if filepath.IsAbs(target) {
				return fmt.Errorf("tar entry %q: absolute symlink %q refused", hdr.Name, hdr.Linkname)
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(dest), target))
			if !strings.HasPrefix(resolved, filepath.Clean(destDir)+string(os.PathSeparator)) && resolved != filepath.Clean(destDir) {
				return fmt.Errorf("tar entry %q: symlink escapes the extraction directory, refusing", hdr.Name)
			}
			if err := os.Symlink(target, dest); err != nil {
				return err
			}
		default:
			// Hard links, devices, fifos: not part of any npm contract —
			// skip rather than fail (an extension tarball carrying one is
			// exotic, not hostile).
		}
	}
	return nil
}

// verifySRI hashes the artifact and compares the SRI form exactly.
func verifySRI(data []byte, sri string) error {
	sum := sha512.Sum512(data)
	got := "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
	if got != sri {
		return fmt.Errorf("integrity mismatch: pin says %s, bytes hash to %s — the artifact is not what was pinned, refusing", shortSRI(sri), shortSRI(got))
	}
	return nil
}

// ProjectPlugins writes every ConfigMap labeled harmostes.dev/plugin=<value>
// in the namespace into destDir/<cm>/<key> (mode 0755 — plugin scripts exec;
// data keys are readable regardless). Zero matches is LEGAL and writes an
// empty manifest — a namespace may simply not carry template plugins yet.
// Returns the projected ConfigMap count (the init logs it; a deployment that
// expected content and got zero reads the number off the pod log).
func ProjectPlugins(ctx context.Context, cl client.Client, namespace, labelValue, destDir string) (int, error) {
	var list corev1.ConfigMapList
	if err := cl.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabels{
		"harmostes.dev/plugin": labelValue,
	}); err != nil {
		return 0, fmt.Errorf("list plugin ConfigMaps (label harmostes.dev/plugin=%s in %s): %w", labelValue, namespace, err)
	}
	count := 0
	for i := range list.Items {
		cm := &list.Items[i]
		cmDir := filepath.Join(destDir, cm.Name)
		if err := os.MkdirAll(cmDir, 0o755); err != nil {
			return count, fmt.Errorf("project %s: %w", cm.Name, err)
		}
		for key, val := range cm.Data {
			// Keys are /-free by ConfigMap convention; a hostile key cannot
			// traverse (the apiserver rejects keys with path separators
			// inside hidden dirs only, but clean + guard regardless).
			clean := path.Clean(key)
			if strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
				return count, fmt.Errorf("project %s: key %q escapes the plugins dir, refusing", cm.Name, key)
			}
			if err := os.WriteFile(filepath.Join(cmDir, filepath.FromSlash(clean)), []byte(val), 0o755); err != nil {
				return count, fmt.Errorf("project %s/%s: %w", cm.Name, key, err)
			}
		}
		for key, val := range cm.BinaryData {
			clean := path.Clean(key)
			if strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
				return count, fmt.Errorf("project %s: binary key %q escapes the plugins dir, refusing", cm.Name, key)
			}
			if err := os.WriteFile(filepath.Join(cmDir, filepath.FromSlash(clean)), val, 0o755); err != nil {
				return count, fmt.Errorf("project %s/%s (binary): %w", cm.Name, key, err)
			}
		}
		count++
	}
	return count, nil
}

// getJSON fetches and parses a packument.
func getJSON(ctx context.Context, hc *http.Client, url string) (*packument, error) {
	body, err := getBytes(ctx, hc, url)
	if err != nil {
		return nil, err
	}
	var pk packument
	if err := json.Unmarshal(body, &pk); err != nil {
		return nil, fmt.Errorf("parse packument from %s: %w", url, err)
	}
	return &pk, nil
}

// getBytes fetches a URL with the bounded client, refusing non-2xx with the
// status named (a 404 registry response must not parse as an empty packument).
func getBytes(ctx context.Context, hc *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func nameFor(spec string) string {
	if i := strings.LastIndex(spec, "@"); i > 0 {
		return spec[:i]
	}
	return spec
}

func shortSRI(sri string) string {
	if len(sri) > 24 {
		return sri[:24] + "…"
	}
	return sri
}

// HTTPClient exposes the bounded fetch client (the init-runtime seam passes
// it explicitly so tests can inject timeouts).
func HTTPClient() *http.Client { return httpClient }
