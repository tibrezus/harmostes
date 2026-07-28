package v1alpha1

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TestExternalSystemBinding_JSONRoundTrip verifies the binding value type
// serializes losslessly (the contract every CRD value type must satisfy so the
// JSON DeepCopy helper stays valid).
func TestExternalSystemBinding_JSONRoundTrip(t *testing.T) {
	in := ExternalSystemBinding{
		Name:              "sourceRepo",
		BindingRole:       BindingRoleSourceRepo,
		SurfaceKind:       SurfaceKindRepository,
		ConnectionProfile: "github-com",
		Target: BindingTarget{
			Host:   "github.com",
			Object: "tibrezus/harmostes",
			Branch: "main",
			Extra:  map[string]string{"visibility": "public"},
		},
		Granted: []string{"repository.read", "repository.push"},
		AuthRef: &SecretRef{Name: "gh-token", Key: "token"},
	}

	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out ExternalSystemBinding
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Name != in.Name || out.SurfaceKind != in.SurfaceKind {
		t.Errorf("round-trip mismatch: got %+v, want %+v", out, in)
	}
	if out.AuthRef == nil || out.AuthRef.Name != "gh-token" {
		t.Errorf("AuthRef lost in round-trip: %+v", out.AuthRef)
	}
	if len(out.Granted) != 2 || out.Granted[1] != "repository.push" {
		t.Errorf("Granted lost in round-trip: %+v", out.Granted)
	}
	if out.Target.Extra["visibility"] != "public" {
		t.Errorf("Target.Extra lost in round-trip: %+v", out.Target.Extra)
	}
}

// TestCapabilityRequirement_JSONRoundTrip verifies the node-side capability
// request serializes losslessly.
func TestCapabilityRequirement_JSONRoundTrip(t *testing.T) {
	in := CapabilityRequirement{Binding: "issueTracker", Capability: "issue-tracker.comment.write"}
	b, _ := json.Marshal(in)
	var out CapabilityRequirement
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch: got %+v, want %+v", out, in)
	}
}

// TestWorkflowSpec_BindingsOptional verifies backward compatibility: a
// Workflow without bindings still round-trips (existing production Workflows).
func TestWorkflowSpec_BindingsOptional(t *testing.T) {
	// Legacy workflow spec (no bindings) — must validate.
	legacy := WorkflowSpec{
		Source: SourceSpec{Kind: "git", Repo: "x"},
	}
	b, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	var out WorkflowSpec
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if len(out.Bindings) != 0 {
		t.Errorf("legacy workflow should have no bindings, got %d", len(out.Bindings))
	}

	// New workflow spec with bindings.
	withBindings := WorkflowSpec{
		Source: SourceSpec{Kind: "git"},
		Bindings: []ExternalSystemBinding{
			{Name: "sourceRepo", SurfaceKind: SurfaceKindRepository, BindingRole: BindingRoleSourceRepo},
		},
	}
	b, _ = json.Marshal(withBindings)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal with bindings: %v", err)
	}
	if len(out.Bindings) != 1 || out.Bindings[0].Name != "sourceRepo" {
		t.Errorf("bindings round-trip mismatch: %+v", out.Bindings)
	}
}

// TestNodeSpec_Requires verifies the extended NodeSpec carries capability
// requirements through a round-trip.
func TestNodeSpec_Requires(t *testing.T) {
	in := NodeSpec{
		ID:   "comment-on-issue",
		Type: "plugin",
		Requires: []CapabilityRequirement{
			{Binding: "issueTracker", Capability: "issue-tracker.comment.write"},
		},
	}
	b, _ := json.Marshal(in)
	var out NodeSpec
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Requires) != 1 || out.Requires[0].Capability != "issue-tracker.comment.write" {
		t.Errorf("Requires lost in round-trip: %+v", out.Requires)
	}
}

// TestClaim_JSONRoundTrip verifies the reference-backed-fact type serializes
// losslessly, including the trust class.
func TestClaim_JSONRoundTrip(t *testing.T) {
	in := Claim{
		Type:        "repository.commit.created",
		Binding:     "workspaceRepo",
		ExternalID:  "abc123",
		URL:         "https://github.com/x/y/commit/abc123",
		TrustClass:  ClaimTrustObserved,
		ValidatedBy: "",
	}
	b, _ := json.Marshal(in)
	var out Claim
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch: got %+v, want %+v", out, in)
	}

	// Promote to validated.
	in.TrustClass = ClaimTrustValidated
	in.ValidatedBy = "validate-commit-exists"
	b, _ = json.Marshal(in)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal validated: %v", err)
	}
	if out.TrustClass != ClaimTrustValidated || out.ValidatedBy != "validate-commit-exists" {
		t.Errorf("validated claim round-trip mismatch: %+v", out)
	}
}

// TestNodeResultEnvelope_JSONRoundTrip verifies the universal envelope carries
// claims, artifacts, payload, provenance, and references losslessly.
func TestNodeResultEnvelope_JSONRoundTrip(t *testing.T) {
	in := NodeResultEnvelope{
		NodeID:  "agent-1",
		RunID:   "run-42",
		Status:  "ok",
		Summary: "docs synced",
		Artifacts: []Artifact{
			{Name: "rig", Path: "raw/arch/x/rig.json", Hash: "deadbeef"},
		},
		Claims: []Claim{
			{Type: "wiki.page.updated", Binding: "wiki", ExternalID: "concepts/foo", TrustClass: ClaimTrustObserved},
		},
		Payload:    []byte(`{"turns":3}`),
		References: []EvidenceReference{{Binding: "workspaceRepo", Kind: "commit", Identifier: "abc123"}},
		Provenance: Provenance{TriggeredBy: "tibrez", TriggerSource: "webhook"},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out NodeResultEnvelope
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.NodeID != in.NodeID || out.Status != in.Status {
		t.Errorf("round-trip mismatch: got %+v", out)
	}
	if len(out.Claims) != 1 || out.Claims[0].ExternalID != "concepts/foo" {
		t.Errorf("Claims lost: %+v", out.Claims)
	}
	if string(out.Payload) != `{"turns":3}` {
		t.Errorf("Payload lost: %s", out.Payload)
	}
	if out.Provenance.TriggeredBy != "tibrez" {
		t.Errorf("Provenance lost: %+v", out.Provenance)
	}
}

// TestAttempt_DeepCopy verifies the Attempt CRD type deep-copies without
// sharing references (slices, pointers are independent).
func TestAttempt_DeepCopy(t *testing.T) {
	in := &Attempt{
		ObjectMeta: metav1.ObjectMeta{Name: "docs-sync-abc123", Namespace: "harmostes"},
		Spec: AttemptSpec{
			WorkflowRef: "harmostes/wiki-harmostes",
			Owner:       "tibrez",
			Objective: ObjectiveSpec{
				Kind:           ObjectiveKindDocumentationSync,
				DesiredOutcome: "wiki docs reflect revision abc123",
				TargetedState:  "abc123",
				PrimarySubject: Subject{Binding: "sourceRepo", Object: "tibrezus/harmostes"},
				RelatedSubjects: []Subject{
					{Binding: "wiki", Object: "rezuscloud/llm-wiki"},
				},
			},
			Bindings: []ExternalSystemBinding{
				{Name: "sourceRepo", SurfaceKind: SurfaceKindRepository, Granted: []string{"repository.read"}},
			},
		},
		Status: AttemptStatus{
			Phase:       AttemptPhaseReconciling,
			Runs:        []RunRecord{{Name: "run-1", Phase: "succeeded"}},
			NodeResults: []NodeResultEnvelope{{NodeID: "agent-1", Status: "ok"}},
		},
	}

	out := in.DeepCopy()
	if out == nil {
		t.Fatal("DeepCopy returned nil")
	}
	if out.Name != in.Name || out.Spec.Objective.Kind != in.Spec.Objective.Kind {
		t.Errorf("DeepCopy mismatch: got %+v", out)
	}
	// Mutate the copy; original must be unaffected (no shared slice backing).
	out.Spec.Bindings[0].Granted[0] = "tampered"
	out.Spec.Objective.RelatedSubjects[0].Object = "tampered"
	out.Status.Runs[0].Name = "tampered"
	if in.Spec.Bindings[0].Granted[0] != "repository.read" {
		t.Errorf("DeepCopy shared Granted slice backing array")
	}
	if in.Spec.Objective.RelatedSubjects[0].Object != "rezuscloud/llm-wiki" {
		t.Errorf("DeepCopy shared RelatedSubjects slice backing array")
	}
	if in.Status.Runs[0].Name != "run-1" {
		t.Errorf("DeepCopy shared Runs slice backing array")
	}
}

// TestAttempt_DeepCopyObject verifies the runtime.Object interface round-trips.
func TestAttempt_DeepCopyObject(t *testing.T) {
	in := &Attempt{ObjectMeta: metav1.ObjectMeta{Name: "a"}}
	obj := in.DeepCopyObject()
	out, ok := obj.(*Attempt)
	if !ok {
		t.Fatalf("DeepCopyObject did not return *Attempt: %T", obj)
	}
	if out == in {
		t.Error("DeepCopyObject returned the same pointer")
	}
	if out.Name != "a" {
		t.Errorf("DeepCopyObject lost name: %q", out.Name)
	}
}

// TestConnectionProfile_DeepCopy verifies the ConnectionProfile CRD type
// deep-copies without sharing references.
func TestConnectionProfile_DeepCopy(t *testing.T) {
	in := &ConnectionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "github-com", Namespace: "harmostes"},
		Spec: ConnectionProfileSpec{
			HostFamily:       HostFamilyGitHub,
			APIBaseURL:       "https://api.github.com",
			AuthTransport:    AuthBearerHeader,
			WebhookVerify:    WebhookVerifyHMACSHA256Header,
			WebhookSecretRef: &SecretRef{Name: "gh-webhook", Key: "secret"},
		},
	}
	out := in.DeepCopy()
	if out == nil || out.Spec.APIBaseURL != in.Spec.APIBaseURL {
		t.Fatalf("DeepCopy mismatch: %+v", out)
	}
	if out.Spec.WebhookSecretRef == in.Spec.WebhookSecretRef {
		t.Error("DeepCopy shared WebhookSecretRef pointer")
	}
	out.Spec.WebhookSecretRef.Name = "tampered"
	if in.Spec.WebhookSecretRef.Name != "gh-webhook" {
		t.Error("DeepCopy shared WebhookSecretRef backing")
	}
}

// TestScheme_RegistersNewKinds verifies Attempt and ConnectionProfile register
// in the group scheme (required for controller-runtime clients to type them).
func TestScheme_RegistersNewKinds(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	for _, tc := range []struct {
		obj      runtime.Object
		wantKind string
	}{
		{&Attempt{}, "Attempt"},
		{&AttemptList{}, "AttemptList"},
		{&ConnectionProfile{}, "ConnectionProfile"},
		{&ConnectionProfileList{}, "ConnectionProfileList"},
	} {
		gvks, _, _ := scheme.ObjectKinds(tc.obj)
		found := false
		for _, gvk := range gvks {
			if gvk.Kind == tc.wantKind {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s not registered in scheme (got gvks: %+v)", tc.wantKind, gvks)
		}
	}
}

// TestAttemptResource ensures the GroupResource helper resolves.
func TestAttemptResource(t *testing.T) {
	gr := AttemptResource()
	if gr.Group != GroupName || gr.Resource != "attempts" {
		t.Errorf("AttemptResource = %+v, want group=%s resource=attempts", gr, GroupName)
	}
}

// TestConnectionProfileResource ensures the GroupResource helper resolves.
func TestConnectionProfileResource(t *testing.T) {
	gr := ConnectionProfileResource()
	if gr.Group != GroupName || gr.Resource != "connectionprofiles" {
		t.Errorf("ConnectionProfileResource = %+v, want group=%s resource=connectionprofiles", gr, GroupName)
	}
}
