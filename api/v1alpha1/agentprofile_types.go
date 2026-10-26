package v1alpha1

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/semver"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// AgentProfile is the per-environment agentic configuration (ADR-0011
// amendment 2026-10-26, the three-tier split): the deployment decides how the
// agent thinks and runs; the kernel only reads the decision. One profile per
// namespace/env — dev decides for dev, prod for prod.
//
// Phase 1a (#785) is INERT by default: with no profile wired
// (HARMOSTES_AGENT_PROFILE unset, nothing referencing it) the fleet behaves
// byte-identically to a kernel without this type. The first consuming surface
// is model resolution (profile windows → template fallback); harness pin,
// extensions, and the skills pin are CARRIED by the schema and consumed by
// later phases (#786 materialization, skills pin consumption) — declaring them
// today does nothing, by design.
type AgentProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AgentProfileSpec `json:"spec,omitempty"`
}

// AgentProfileList is the list kind (scheme registration + client List).
type AgentProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentProfile `json:"items"`
}

// AgentProfileSpec carries the profile's decision surface. Every field is
// optional: a profile that only declares model windows is legal and is the
// first useful shape (resolution precedes materialization).
type AgentProfileSpec struct {
	// Harness pins the agent runtime this environment runs: e.g.
	// runtime "pi@1.3.0" + its npm integrity hash. Carried, not consumed,
	// until the materialization seam (#786).
	Harness HarnessRef `json:"harness,omitempty"`
	// Extensions is the UNIVERSAL extension set (ADR-0011 amendment):
	// exact npm versions + integrity hashes, never ranges (#502 — an
	// unverified pin is a fleet-wide stall waiting to happen). Carried,
	// not consumed, until #786.
	Extensions []ExtensionPin `json:"extensions,omitempty"`
	// Models carries the time-windowed model schedule for this
	// environment (#494 semantics, per-env decision surface per the
	// 2026-10-25 Configuration Contract + the three-tier amendment).
	// Consumed NOW (phase 1a): the worker resolves profile windows first;
	// when no window matches, the template's own agent model (and its
	// windows, if any) applies — profile-windows → template-fallback.
	Models []ModelWindow `json:"models,omitempty"`
	// Skills pins the served skills repo + revision (full 40-hex object
	// name — fetch-by-SHA refuses abbreviations, the #524 lesson).
	// Carried, not consumed, until the skills pin migrates.
	Skills *SkillsPin `json:"skills,omitempty"`
	// Compatibility is the environment's kernel compatibility statement,
	// the single documented form `kernel >=X` (X a semantic version, e.g.
	// "kernel >=1.5.0"). The controller refuses to dispatch and the worker
	// refuses to run, both LOUDLY with the statement named, while the
	// running kernel does not satisfy it (#504: fail loud, never silently
	// degrade a declared constraint). Absent = no statement = never
	// refuses. Present-but-malformed refuses everywhere: a constraint that
	// cannot be parsed cannot be honored.
	Compatibility string `json:"compatibility,omitempty"`
}

// HarnessRef pins the runtime binary: name@version plus its integrity hash.
type HarnessRef struct {
	// Runtime is the pinned runtime identity, e.g. "pi@1.3.0". pi holds
	// the one privileged runtime slot (ADR-0011 amendment); everything
	// else in the agentic world is an extension.
	Runtime string `json:"runtime,omitempty"`
	// Source is the exact npm spec of the pinned runtime artifact, e.g.
	// "@earendil-works/pi-coding-agent@1.3.0" — the identity (Runtime) and
	// the fetchable artifact (Source) are deliberately separate: the
	// package name may change without the runtime identity changing.
	// Consumed by the materialization seam (#786); a declared Runtime
	// without an exact Source and an Integrity hash refuses at dispatch
	// (a pin that cannot be verified is a silent no-op pretending to be a
	// pin, #504).
	Source string `json:"source,omitempty"`
	// Integrity is the npm integrity hash of the pinned runtime artifact
	// (e.g. "sha512-..."). Consumed by the materialization seam (#786):
	// the tarball is verified against THIS hash, never the registry's own
	// word. Declared hashes must be exact — no ranges by design.
	Integrity string `json:"integrity,omitempty"`
}

// ExtensionPin declares one npm extension: exact version, integrity hash.
type ExtensionPin struct {
	// Name is the extension's identity in logs and the composed set.
	Name string `json:"name,omitempty"`
	// Source is the exact npm spec, e.g. "@tibrezus/sol-pi@1.2.3" — a
	// version range is a #502-class hazard and is rejected by the
	// materialization contract (#786).
	Source string `json:"source,omitempty"`
	// Integrity is the npm integrity hash of the exact artifact.
	Integrity string `json:"integrity,omitempty"`
}

// SkillsPin pins the served skills content: repo + full-length revision.
type SkillsPin struct {
	// Repo is the skills git repository (e.g. the agents fork).
	Repo string `json:"repo,omitempty"`
	// Rev is the pinned revision: a full 40-hex object name (GitHub
	// refuses abbreviations for fetch-by-SHA — #524), or empty for the
	// repo default.
	Rev string `json:"rev,omitempty"`
}

// ResolveModel returns the model for `now`: the first profile window
// containing it, else "" (the caller falls back to the template's own
// resolution). One resolution per run at run start (#494).
func (p AgentProfileSpec) ResolveModel(now time.Time) string {
	return resolveModelWindows(p.Models, now)
}

// CompatibilityCause returns "" when the statement is absent or satisfied by
// kernelVersion; otherwise the named cause: why the environment refuses to
// run. Malformed statements and unparseable kernel versions are causes, not
// panics — the caller surfaces the string loudly (#504).
func (p AgentProfileSpec) CompatibilityCause(kernelVersion string) string {
	stmt := strings.TrimSpace(p.Compatibility)
	if stmt == "" {
		return ""
	}
	rest, ok := strings.CutPrefix(stmt, "kernel")
	if !ok {
		return fmt.Sprintf("AgentProfile compatibility statement %q is malformed — the only documented form is %q", stmt, "kernel >=X.Y.Z")
	}
	rest = strings.TrimSpace(rest)
	want, ok := strings.CutPrefix(rest, ">=")
	if !ok {
		return fmt.Sprintf("AgentProfile compatibility statement %q is malformed — the only documented operator is >= (%q)", stmt, "kernel >=X.Y.Z")
	}
	want = normalizeSemver(strings.TrimSpace(want))
	if !semver.IsValid(want) {
		return fmt.Sprintf("AgentProfile compatibility statement %q is malformed — %q is not a semantic version", stmt, strings.TrimSpace(rest))
	}
	got := normalizeSemver(strings.TrimSpace(kernelVersion))
	if !semver.IsValid(got) {
		return fmt.Sprintf("kernel version %q is not a semantic version — cannot evaluate the profile's compatibility statement %q", kernelVersion, stmt)
	}
	if semver.Compare(got, want) < 0 {
		return fmt.Sprintf("kernel %s does not satisfy the profile's compatibility statement %q — upgrade the kernel or relax the profile", kernelVersion, stmt)
	}
	return ""
}

// AgentProfileEnvVar is the deployment knob naming the namespace's profile:
// HARMOSTES_AGENT_PROFILE. Empty/unset = no profile = the inert default.
const AgentProfileEnvVar = "HARMOSTES_AGENT_PROFILE"

// AgentProfileRefFromEnv reports the profile name the deployment wired —
// "" means no profile. Reads os.Getenv by default; tests inject a getter.
func AgentProfileRefFromEnv(getenv func(string) string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	return getenv(AgentProfileEnvVar)
}

// normalizeSemver adapts bare release versions ("1.4.1", the GoReleaser/
// GitVersion shape) to x/mod/semver's required "v" prefix. Already-valid
// inputs pass through untouched.
func normalizeSemver(v string) string {
	if strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}

// resolveModelWindows is the ONE window matcher (first match in order wins,
// midnight-wrap allowed, malformed windows never match — #494). AgentSpec and
// AgentProfile share it so the two surfaces cannot drift.
func resolveModelWindows(windows []ModelWindow, now time.Time) string {
	for _, w := range windows {
		loc, err := time.LoadLocation(w.tzOrUTC())
		if err != nil {
			continue // unknown zone: the window cannot match, by design
		}
		start, err1 := minutesOfDay(w.Start)
		end, err2 := minutesOfDay(w.End)
		if err1 != nil || err2 != nil {
			continue
		}
		local := now.In(loc)
		nowMin := local.Hour()*60 + local.Minute()
		if w.wrapsMidnight() {
			// "16:00"→"02:00": the window is [start, 24:00) ∪ [0, end).
			if nowMin >= start || nowMin < end {
				return w.Model
			}
		} else if nowMin >= start && nowMin < end {
			return w.Model
		}
	}
	return ""
}

// SortedProfileNames is a test/dev convenience (deterministic listings).
func SortedProfileNames(items []AgentProfile) []string {
	names := make([]string, 0, len(items))
	for i := range items {
		names = append(names, items[i].Name)
	}
	sort.Strings(names)
	return names
}

// ---------------------------------------------------------------------------
// runtime.Object + DeepCopy (JSON round-trip, the package's v1alpha1 pattern)
// ---------------------------------------------------------------------------

func (in *AgentProfile) DeepCopyInto(out *AgentProfile) { deepCopy(in, out) }
func (in *AgentProfile) DeepCopy() *AgentProfile {
	if in == nil {
		return nil
	}
	out := new(AgentProfile)
	deepCopy(in, out)
	return out
}
func (in *AgentProfile) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *AgentProfileList) DeepCopyInto(out *AgentProfileList) { deepCopy(in, out) }
func (in *AgentProfileList) DeepCopy() *AgentProfileList {
	if in == nil {
		return nil
	}
	out := new(AgentProfileList)
	deepCopy(in, out)
	return out
}
func (in *AgentProfileList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// Compile-time assertions.
var (
	_ runtime.Object = (*AgentProfile)(nil)
	_ runtime.Object = (*AgentProfileList)(nil)
)
