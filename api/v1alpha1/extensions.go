package v1alpha1

import (
	"encoding/json"
	"fmt"
	"strings"

	"golang.org/x/mod/semver"
)

// The runtime-materialization wire contract (ADR-0011 amendment, three-tier
// 1b / #786). The dispatcher composes the per-attempt pi extension set
// (profile universal ⊕ template delta) and the profile's harness pin,
// validates the pins HERE-SHAPED rules, and stamps deterministic JSON
// manifests into the Attempt Job env; the materialize init container and the
// run form both parse the same documents, so pool and pod can never disagree
// about what was declared.
//
// Everything here lives in the types package because three layers consume it
// (api types, the k8s Job builder, the worker) and the leaf-ness rules
// forbid two of them from importing each other.

// ValidateExtensionPins enforces the materialization contract on a pin set
// BEFORE a pod is burned (#504, at the earliest surface): every pin must be
// exact and verifiable —
//   - name non-empty (it names the composed set and the on-disk extension dir),
//   - source an exact npm spec "path@X.Y.Z" (a range, tag, or bare path is a
//     #502-class hazard: an unpinned resolution is a fleet-wide stall
//     waiting to happen),
//   - integrity a well-formed SRI "sha512-<base64>" (the tarball is verified
//     against THE PIN, never the registry's own word).
func ValidateExtensionPins(pins []ExtensionPin) error {
	for _, p := range pins {
		if p.Name == "" {
			return fmt.Errorf("extension pin %q: name is required (it names the composed set and the on-disk extension dir)", p.Source)
		}
		if err := ValidateNPMSpec(p.Source); err != nil {
			return fmt.Errorf("extension pin %q: %w", p.Name, err)
		}
		if err := ValidateSRI(p.Integrity); err != nil {
			return fmt.Errorf("extension pin %q: %w", p.Name, err)
		}
	}
	return nil
}

// ValidateHarnessPin enforces the same contract on a profile's harness pin.
// All three fields are required together: a declared runtime without an
// exact source or an integrity hash cannot be verified, and a pin that
// cannot be verified is a silent no-op pretending to be a pin (#504).
// The zero value is valid — no pin means the image's baked harness stands
// (inert).
func ValidateHarnessPin(h HarnessRef) error {
	if h.Runtime == "" && h.Source == "" && h.Integrity == "" {
		return nil
	}
	if h.Runtime == "" {
		return fmt.Errorf("harness pin: runtime is required (e.g. \"pi@1.3.0\")")
	}
	if err := ValidateNPMSpec(h.Source); err != nil {
		return fmt.Errorf("harness pin %q: %w", h.Runtime, err)
	}
	if err := ValidateSRI(h.Integrity); err != nil {
		return fmt.Errorf("harness pin %q: %w", h.Runtime, err)
	}
	return nil
}

// ValidateNPMSpec accepts ONLY exact specs: "<path>@<bare semver>". Scoped
// names (@scope/pkg@1.2.3) split at the LAST @. A bare version must satisfy
// semver.IsValid — ranges (^ ~ > * x), tags (latest/next), and dist-tags all
// refuse.
func ValidateNPMSpec(spec string) error {
	if spec == "" {
		return fmt.Errorf("source is required (exact npm spec \"path@X.Y.Z\" — a range is a #502-class hazard)")
	}
	i := strings.LastIndex(spec, "@")
	if i <= 0 {
		return fmt.Errorf("source %q is not an exact npm spec \"path@X.Y.Z\"", spec)
	}
	version := spec[i+1:]
	if !strings.HasPrefix(version, "v") && !semver.IsValid("v"+version) {
		return fmt.Errorf("source %q carries version %q, which is not an exact semver — ranges, tags, and dist-tags refuse (pin the exact version)", spec, version)
	}
	return nil
}

// ValidateSRI checks the SRI shape: sha512-<base64 (43..88 chars)>. The
// materialization verifies the downloaded artifact against EXACTLY this
// hash — a wrong-shape hash would either never verify (safe but confusing)
// or be silently skipped (unsafe); both refuse here, before a pod exists.
func ValidateSRI(sri string) error {
	const prefix = "sha512-"
	if !strings.HasPrefix(sri, prefix) {
		return fmt.Errorf("integrity %q is not an SRI sha512 hash (want \"sha512-<base64>\")", sri)
	}
	b64 := strings.TrimPrefix(sri, prefix)
	if len(b64) < 43 || len(b64) > 88 {
		return fmt.Errorf("integrity %q: sha512 base64 body has %d chars (want 43..88)", sri, len(b64))
	}
	for _, r := range b64 {
		alpha := r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if !alpha && r != '+' && r != '/' && r != '=' {
			return fmt.Errorf("integrity %q: invalid base64 character %q", sri, string(r))
		}
	}
	return nil
}

// ExtensionManifest is the wire form of one composed extension pin (the
// HARMOSTES_EXTENSION_MANIFEST env value — a JSON array of these).
type ExtensionManifest struct {
	Name      string `json:"name"`
	Source    string `json:"source"`
	Integrity string `json:"integrity"`
}

// RenderExtensionManifest renders the composed set to its env form. Nil = ""
// (the env var simply is not set — inert). Deterministic: two dispatches of
// the same inputs render byte-identical manifests.
func RenderExtensionManifest(pins []ExtensionPin) (string, error) {
	if len(pins) == 0 {
		return "", nil
	}
	m := make([]ExtensionManifest, len(pins))
	for i, p := range pins {
		m[i] = ExtensionManifest(p)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("render extension manifest: %w", err)
	}
	return string(b), nil
}

// ParseExtensionManifest is the run form's (and the init's) inverse.
// Malformed JSON is an ERROR: a manifest that cannot be parsed cannot be
// honored, and a silently-empty extension set is exactly the #504
// degradation this contract exists to prevent.
func ParseExtensionManifest(raw string) ([]ExtensionManifest, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var m []ExtensionManifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("HARMOSTES_EXTENSION_MANIFEST: not valid JSON: %w", err)
	}
	for _, p := range m {
		if p.Name == "" || p.Source == "" || p.Integrity == "" {
			return nil, fmt.Errorf("HARMOSTES_EXTENSION_MANIFEST: incomplete entry %+v (want name, source, integrity)", p)
		}
	}
	return m, nil
}

// HarnessManifest is the wire form of the profile's harness pin
// (HARMOSTES_HARNESS_MANIFEST). One JSON object carries identity, exact npm
// spec, and integrity together — three sibling env vars could disagree; one
// document cannot.
type HarnessManifest struct {
	Runtime   string `json:"runtime"`
	Source    string `json:"source"`
	Integrity string `json:"integrity"`
}

// RenderHarnessManifest renders the pin to its env form. Zero pin = "".
func RenderHarnessManifest(h HarnessRef) (string, error) {
	if h.Runtime == "" && h.Source == "" && h.Integrity == "" {
		return "", nil
	}
	b, err := json.Marshal(HarnessManifest(h))
	if err != nil {
		return "", fmt.Errorf("render harness manifest: %w", err)
	}
	return string(b), nil
}

// ParseHarnessManifest is the inverse. Malformed or incomplete = error (#504).
func ParseHarnessManifest(raw string) (*HarnessManifest, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var m HarnessManifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("HARMOSTES_HARNESS_MANIFEST: not valid JSON: %w", err)
	}
	if m.Runtime == "" || m.Source == "" || m.Integrity == "" {
		return nil, fmt.Errorf("HARMOSTES_HARNESS_MANIFEST: incomplete pin %+v (want runtime, source, integrity)", m)
	}
	return &m, nil
}
