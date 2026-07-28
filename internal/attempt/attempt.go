// Package attempt implements the Canonical Orchestration History lifecycle
// (ADR-0005): deriving an Implementation Objective from a Workflow + trigger,
// computing its Objective Identity, resolving or creating the Attempt CR that
// realizes it, and recording run outcomes into it.
//
// The Objective is DERIVED deterministically from the Workflow spec + trigger
// context — no explicit spec.objective field is required, so existing
// declarative Workflows get canonical history without migration. Same (kind +
// primary subject + targeted state) always yields the same Attempt name, so a
// new trigger for an in-flight objective CONTINUES the same Attempt rather than
// fragmenting history.
package attempt

import (
	"hash/fnv"
	"net/url"
	"strings"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// TriggerContext carries what the controller knows at the moment a run is due.
type TriggerContext struct {
	// Revision is the targeted source revision (webhook head SHA), or empty for
	// schedule/spec-change triggers where the head is discovered at runtime.
	Revision string
	// Source is the trigger channel: webhook | schedule | controller.
	Source string
}

// DeriveObjective computes the Implementation Objective (ADR-0005) for a
// Workflow run. Deterministic: the same Workflow + targeted state always yields
// the same Objective Identity, so repeated triggers continue the same Attempt.
func DeriveObjective(wf *v1alpha1.Workflow, trigger TriggerContext) v1alpha1.ObjectiveSpec {
	kind := DeriveKind(wf)
	primary, related := deriveSubjects(wf, kind)
	return v1alpha1.ObjectiveSpec{
		Kind:            kind,
		PrimarySubject:  primary,
		RelatedSubjects: related,
		DesiredOutcome:  DesiredOutcome(kind),
		TargetedState:   DeriveTargetedState(wf, trigger),
	}
}

// DeriveKind maps a Workflow to its Objective Kind. A fork source forces
// fork-sync; otherwise the gate plugin name determines the kind (the gate IS
// the workflow archetype). Falls back to documentation-sync (the dominant
// fleet archetype) when neither signal is conclusive.
func DeriveKind(wf *v1alpha1.Workflow) string {
	if wf.Spec.Source.Fork != nil && wf.Spec.Source.Fork.URL != "" {
		return v1alpha1.ObjectiveKindForkSync
	}
	switch wf.Spec.Agent.Gate.Plugin.Name {
	case "wiki-lint":
		return v1alpha1.ObjectiveKindDocumentationSync
	case "review-validate":
		return v1alpha1.ObjectiveKindPRReview
	case "fork-resolved":
		return v1alpha1.ObjectiveKindForkSync
	default:
		return v1alpha1.ObjectiveKindDocumentationSync
	}
}

// DeriveTargetedState resolves the Targeted State that makes this objective a
// DISTINCT implementation (ADR-0005): the webhook revision when available,
// else a pinned source revision, else "head" for schedule/poll triggers whose
// exact head is discovered at runtime. All three are deterministic for a given
// trigger, so Objective Identity is stable.
func DeriveTargetedState(wf *v1alpha1.Workflow, trigger TriggerContext) string {
	if trigger.Revision != "" {
		return trigger.Revision
	}
	if wf.Spec.Source.Revision != "" {
		return wf.Spec.Source.Revision
	}
	return "head"
}

// DesiredOutcome returns the intended terminal result for an Objective Kind.
func DesiredOutcome(kind string) string {
	switch kind {
	case v1alpha1.ObjectiveKindDocumentationSync:
		return "source documentation reflected in the wiki KB"
	case v1alpha1.ObjectiveKindPRReview:
		return "PR reviewed with structured feedback posted to the host"
	case v1alpha1.ObjectiveKindForkSync:
		return "fork release branch synced to upstream with a green build"
	case v1alpha1.ObjectiveKindDeploymentChange:
		return "cluster reaches the declared desired state"
	default:
		return "objective achieved"
	}
}

// deriveSubjects resolves the Primary Subject (and any Related Subjects) for an
// objective. The binding names are derivation conventions (source/fork) used
// until Workflows carry explicit External System Bindings (ADR-0003); the
// object identifiers come from the Workflow's source config.
func deriveSubjects(wf *v1alpha1.Workflow, kind string) (v1alpha1.Subject, []v1alpha1.Subject) {
	if kind == v1alpha1.ObjectiveKindForkSync && wf.Spec.Source.Fork != nil {
		// The objective is about keeping the FORK in sync; the upstream is the
		// related subject that drives each attempt.
		primary := v1alpha1.Subject{Binding: "fork", Object: RepoID(wf.Spec.Source.Fork.URL)}
		var related []v1alpha1.Subject
		if wf.Spec.Source.Repo != "" {
			related = append(related, v1alpha1.Subject{Binding: "source", Object: wf.Spec.Source.Repo})
		}
		return primary, related
	}
	return v1alpha1.Subject{Binding: "source", Object: wf.Spec.Source.Repo}, nil
}

// RepoID reduces a git URL to an owner/repo (or path) identifier suitable for a
// Subject.Object. "git@github.com:rezuscloud/signoz.git" → "rezuscloud/signoz";
// "https://github.com/x/y" → "x/y"; an already-short identifier is returned
// unchanged.
func RepoID(gitURL string) string {
	s := strings.TrimSpace(gitURL)
	if s == "" {
		return ""
	}
	// SSH form: git@host:path[.git]
	if i := strings.Index(s, ":"); i > 0 && strings.Contains(s[:i], "@") {
		path := strings.TrimSuffix(s[i+1:], ".git")
		return path
	}
	// HTTPS form
	if u, err := url.Parse(s); err == nil && u.Path != "" {
		return strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git")
	}
	return s
}

// Identity returns the deterministic Objective Identity string
// (kind|primarySubject.object|targetedState). It is the dedup/merge key: two
// objectives with the same identity realize the SAME Attempt (ADR-0005). Used
// for the Attempt name; not stored raw (it may contain '/').
func Identity(obj v1alpha1.ObjectiveSpec) string {
	return obj.Kind + "|" + obj.PrimarySubject.Object + "|" + obj.TargetedState
}

// AttemptName returns a DNS-1123-safe, deterministic Attempt name for a
// (workflow, identity) pair. Same pair → same name → ResolveOrCreate finds the
// existing Attempt instead of creating a duplicate. The hash suffix
// distinguishes distinct targeted states of the same workflow.
func AttemptName(workflowName, identity string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(identity))
	hash := toHex(h.Sum(nil))[:12] // 12 lowercase hex chars; collision-resistant per workflow
	base := sanitizeDNS(workflowName)
	const maxBase = 63 - len("attempt-") - len("-") - 12
	if len(base) > maxBase {
		base = base[:maxBase]
	}
	return "attempt-" + base + "-" + hash
}

// sanitizeDNS lowercases and replaces every non-[a-z0-9-] rune with '-',
// collapsing repeats and trimming leading/trailing hyphens — the DNS-1123 label
// rules a Kubernetes object name must satisfy.
func sanitizeDNS(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteRune('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// toHex returns the lowercase hex encoding of b.
func toHex(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[2*i] = digits[v>>4]
		out[2*i+1] = digits[v&0x0f]
	}
	return string(out)
}
