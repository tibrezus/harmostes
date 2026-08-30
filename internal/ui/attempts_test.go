package ui

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func boolPtr(b bool) *bool { return &b }

// newAttemptTestServer creates a Server with a fake k8s client pre-loaded
// with the given objects.
func newAttemptTestServer(t *testing.T, objs ...runtime.Object) *Server {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		Build()

	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}

	return &Server{
		namespace: "test-ns",
		logger:    nil,
		templates: tmpl,
		hub:       NewEventHub(),
		k8sClient: fakeClient,
		platforms: newPlatformRegistry(nil),
	}
}

func TestAttemptDetail_HidesSessionLinkForDeterministicWorkflow(t *testing.T) {
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name: "fork-maintenance-forgejo", Namespace: "test-ns",
			Labels: map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec: v1alpha1.WorkflowSpec{
			Agent: v1alpha1.AgentSpec{Enabled: boolPtr(false)},
		},
	}
	att := &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name: "attempt-fork-maintenance-forgejo-abc123", Namespace: "test-ns",
			Labels: map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec: v1alpha1.AttemptSpec{
			WorkflowRef: "fork-maintenance-forgejo",
			Owner:       "alice",
		},
		Status: v1alpha1.AttemptStatus{
			Phase: "reconciling",
			Runs: []v1alpha1.RunRecord{
				{Name: "worker-pool-pod-abc"},
			},
		},
	}

	srv := newAttemptTestServer(t, wf, att)
	req := httptest.NewRequest("GET", "/attempts/attempt-fork-maintenance-forgejo-abc123", nil)
	req = req.WithContext(withTestIdentity(context.Background()))
	req.SetPathValue("name", "attempt-fork-maintenance-forgejo-abc123")

	rec := httptest.NewRecorder()
	srv.handleAttemptDetail(rec, req)

	body := rec.Body.String()
	sessionURL := "/attempts/attempt-fork-maintenance-forgejo-abc123/runs/worker-pool-pod-abc/session"
	if strings.Contains(body, sessionURL) {
		t.Error("Session link should NOT be shown for deterministic workflows (agent disabled)")
	}
}

func TestAttemptDetail_ShowsSessionLinkForAgentWorkflow(t *testing.T) {
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name: "wiki-lint-harmostes", Namespace: "test-ns",
			Labels: map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec: v1alpha1.WorkflowSpec{
			Agent: v1alpha1.AgentSpec{Enabled: boolPtr(true)},
		},
	}
	att := &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name: "attempt-wiki-lint-harmostes-abc123", Namespace: "test-ns",
			Labels: map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec: v1alpha1.AttemptSpec{
			WorkflowRef: "wiki-lint-harmostes",
			Owner:       "alice",
		},
		Status: v1alpha1.AttemptStatus{
			Phase: "succeeded",
			Runs: []v1alpha1.RunRecord{
				{Name: "worker-pool-pod-xyz"},
			},
		},
	}

	srv := newAttemptTestServer(t, wf, att)
	req := httptest.NewRequest("GET", "/attempts/attempt-wiki-lint-harmostes-abc123", nil)
	req = req.WithContext(withTestIdentity(context.Background()))
	req.SetPathValue("name", "attempt-wiki-lint-harmostes-abc123")

	rec := httptest.NewRecorder()
	srv.handleAttemptDetail(rec, req)

	body := rec.Body.String()
	sessionURL := "/attempts/attempt-wiki-lint-harmostes-abc123/runs/worker-pool-pod-xyz/session"
	if !strings.Contains(body, sessionURL) {
		t.Error("Session link should be shown for agent-enabled workflows")
	}
}

func TestAttemptSession_DeterministicEmptyState(t *testing.T) {
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name: "fork-maintenance-forgejo", Namespace: "test-ns",
			Labels: map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec: v1alpha1.WorkflowSpec{
			Agent: v1alpha1.AgentSpec{Enabled: boolPtr(false)},
		},
	}
	att := &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name: "attempt-fork-maintenance-forgejo-abc123", Namespace: "test-ns",
			Labels: map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec: v1alpha1.AttemptSpec{
			WorkflowRef: "fork-maintenance-forgejo",
			Owner:       "alice",
		},
	}

	srv := newAttemptTestServer(t, wf, att)
	req := httptest.NewRequest("GET", "/attempts/attempt-fork-maintenance-forgejo-abc123/runs/worker-pod-abc/session", nil)
	req = req.WithContext(withTestIdentity(context.Background()))
	req.SetPathValue("name", "attempt-fork-maintenance-forgejo-abc123")
	req.SetPathValue("job", "worker-pod-abc")

	rec := httptest.NewRecorder()
	srv.handleAttemptSession(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "deterministic workflow") {
		t.Errorf("expected deterministic workflow message, got: %s", body)
	}
	// Should NOT blame Dapr config for deterministic workflows
	if strings.Contains(body, "Dapr state store is not configured") {
		t.Error("should not mention Dapr config for deterministic workflows")
	}
}

func TestAttemptSession_AgentWorkflowNotFoundState(t *testing.T) {
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name: "wiki-lint-harmostes", Namespace: "test-ns",
			Labels: map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec: v1alpha1.WorkflowSpec{
			Agent: v1alpha1.AgentSpec{Enabled: boolPtr(true)},
		},
	}
	att := &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name: "attempt-wiki-lint-harmostes-abc123", Namespace: "test-ns",
			Labels: map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec: v1alpha1.AttemptSpec{
			WorkflowRef: "wiki-lint-harmostes",
			Owner:       "alice",
		},
	}

	srv := newAttemptTestServer(t, wf, att)
	req := httptest.NewRequest("GET", "/attempts/attempt-wiki-lint-harmostes-abc123/runs/worker-pod-xyz/session", nil)
	req = req.WithContext(withTestIdentity(context.Background()))
	req.SetPathValue("name", "attempt-wiki-lint-harmostes-abc123")
	req.SetPathValue("job", "worker-pod-xyz")

	rec := httptest.NewRecorder()
	srv.handleAttemptSession(rec, req)

	body := rec.Body.String()
	// Agent workflow with no session: should show "not available" with Dapr hint
	if !strings.Contains(body, "not available") {
		t.Errorf("expected 'not available' message for agent workflow with no session, got: %s", body)
	}
}

func TestWorkflowCRNameStripsPlatform(t *testing.T) {
	cases := []struct{ in, want string }{
		{"harmostes/pr-review-rhesadox", "pr-review-rhesadox"},
		{"pr-review-rhesadox", "pr-review-rhesadox"},
		{"", ""},
	}
	for _, c := range cases {
		if got := workflowCRName(c.in); got != c.want {
			t.Errorf("workflowCRName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Production refs are platform-prefixed (controller writes ns + "/" + name);
// these fixtures reproduce the live shape so regressions in the strip can't
// hide behind bare-name fixtures (review finding on #234).
func TestAttemptDetail_PrefixedWorkflowRef(t *testing.T) {
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pr-review-x", Namespace: "test-ns",
			Labels: map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec: v1alpha1.WorkflowSpec{
			Agent: v1alpha1.AgentSpec{Enabled: boolPtr(true)},
		},
	}
	att := &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name: "attempt-pr-review-x-1", Namespace: "test-ns",
			Labels: map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec: v1alpha1.AttemptSpec{
			WorkflowRef: "test-ns/pr-review-x", // prefixed, as the controller writes it
			Owner:       "alice",
		},
		Status: v1alpha1.AttemptStatus{
			Phase: "succeeded",
			Runs:  []v1alpha1.RunRecord{{Name: "worker-pool-pod-xyz"}},
		},
	}

	srv := newAttemptTestServer(t, wf, att)

	// Detail page: the agentEnabled Get must resolve the bare CR name —
	// with the raw ref the Get fails and the Session link never renders.
	req := httptest.NewRequest("GET", "/attempts/attempt-pr-review-x-1", nil)
	req = req.WithContext(withTestIdentity(context.Background()))
	req.SetPathValue("name", "attempt-pr-review-x-1")
	rec := httptest.NewRecorder()
	srv.handleAttemptDetail(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/attempts/attempt-pr-review-x-1/runs/worker-pool-pod-xyz/session") {
		t.Error("Session link missing: agentEnabled Get failed on prefixed ref")
	}
	// Run links and header link must carry the bare CR name.
	if !strings.Contains(body, `href="/workflows/pr-review-x/runs/worker-pool-pod-xyz"`) {
		t.Error("run link not stripped of platform prefix")
	}
	if strings.Contains(body, `href="/workflows/test-ns/`) {
		t.Error("header link still carries the platform prefix")
	}

	// Session page: dapr is nil here so the no-record branch renders —
	// the deterministic empty state prints the workflow name; it must be
	// the bare CR name, never the prefixed ref.
	req = httptest.NewRequest("GET", "/attempts/attempt-pr-review-x-1/runs/worker-pool-pod-xyz/session", nil)
	req = req.WithContext(withTestIdentity(context.Background()))
	req.SetPathValue("name", "attempt-pr-review-x-1")
	req.SetPathValue("job", "worker-pool-pod-xyz")
	rec = httptest.NewRecorder()
	srv.handleAttemptSession(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session status = %d, body: %s", rec.Code, rec.Body.String())
	}
	sessBody := rec.Body.String()
	if strings.Contains(sessBody, "test-ns/pr-review-x") {
		t.Error("session page leaks the platform-prefixed ref")
	}

	// With a session present the back-to-run link must use the bare name:
	// stub the state store to return one and assert the href.
	srv.logger = slog.Default() // render logs execution errors
	srv.dapr = &stubSessionDapr{key: "pr-review-x:worker-pool-pod-xyz:session"}
	req = httptest.NewRequest("GET", "/attempts/attempt-pr-review-x-1/runs/worker-pool-pod-xyz/session", nil)
	req = req.WithContext(withTestIdentity(context.Background()))
	req.SetPathValue("name", "attempt-pr-review-x-1")
	req.SetPathValue("job", "worker-pool-pod-xyz")
	rec = httptest.NewRecorder()
	srv.handleAttemptSession(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `href="/workflows/pr-review-x/runs/worker-pool-pod-xyz"`) {
		t.Error("session back-link not stripped of platform prefix")
	}
	// The writer keys sessions by the bare name; the stub only answers that
	// key, so a hit also proves the read key matches the writer's.
	if srv.dapr.(*stubSessionDapr).misses != 0 {
		t.Errorf("read used the wrong key: %d misses against the bare-name key", srv.dapr.(*stubSessionDapr).misses)
	}
}

// stubSessionDapr answers exactly one state key (the bare-name session key)
// and counts reads against any other key.
type stubSessionDapr struct {
	DaprClient // embed: only GetStateFromStore is expected on this page
	key        string
	misses     int
}

func (d *stubSessionDapr) GetStateFromStore(_ context.Context, _, key string, value any) (bool, error) {
	if key != d.key {
		// The pi-session availability probe (#243) is an expected second key;
		// only other keys count as misses (wrong transcript key).
		if !strings.HasSuffix(key, ":pi-session") {
			d.misses++
		}
		return false, nil
	}
	// Marshal-then-unmarshal keeps the stub honest about the target type.
	b, _ := json.Marshal(map[string]any{
		"workflow": "pr-review-x", "runId": "worker-pool-pod-xyz",
		"model": "test-model", "skill": "pr-review",
		"startedAt": "2026-08-25T10:00:00Z", "endedAt": "2026-08-25T10:05:00Z",
	})
	_ = json.Unmarshal(b, value)
	return true, nil
}

// #239: template-delegated workflows (nil Agent.Enabled on instance AND
// template — the live pr-review shape) must resolve agent-capable, matching
// the kernel's tri-state rule. The UI previously collapsed nil → false,
// hiding the Session link and mislabeling the empty state "deterministic".
func TestTemplateDelegatedAgentResolution(t *testing.T) {
	enabled := true
	disabled := false

	// Instance nil, template nil → agent-capable (kernel rule).
	wfNil := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf-a", Namespace: "test-ns", Labels: map[string]string{v1alpha1.OwnerLabel: "alice"}},
		Spec:       v1alpha1.WorkflowSpec{TemplateRef: "tmpl"},
	}
	tmplNil := &v1alpha1.WorkflowTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: "test-ns"},
		Spec:       v1alpha1.WorkflowTemplateSpec{},
	}
	// Instance nil, template explicitly false → deterministic.
	tmplOff := &v1alpha1.WorkflowTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: "test-ns"},
		Spec:       v1alpha1.WorkflowTemplateSpec{Agent: v1alpha1.AgentSpec{Enabled: &disabled}},
	}
	// Instance explicitly true (template absent) → agent-capable.
	wfOn := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf-b", Namespace: "test-ns", Labels: map[string]string{v1alpha1.OwnerLabel: "alice"}},
		Spec:       v1alpha1.WorkflowSpec{Agent: v1alpha1.AgentSpec{Enabled: &enabled}},
	}

	s := newAttemptTestServer(t, wfNil, tmplNil, wfOn)
	if !s.agentEnabledFor(t.Context(), wfNil) {
		t.Error("nil instance + nil template must resolve agent-capable (kernel tri-state)")
	}
	s2 := newAttemptTestServer(t, wfNil, tmplOff)
	if s2.agentEnabledFor(t.Context(), wfNil) {
		t.Error("nil instance + template disabled must resolve deterministic")
	}
	if !s.agentEnabledFor(t.Context(), wfOn) {
		t.Error("explicit instance enable must resolve agent-capable")
	}
}

// The session page's deterministic flag follows the same resolution: a
// template-delegated workflow with no session record must show the neutral
// "transcript not available" state, never "deterministic workflow".
func TestSessionPageTemplateDelegatedNotDeterministic(t *testing.T) {
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "pr-review-x", Namespace: "test-ns", Labels: map[string]string{v1alpha1.OwnerLabel: "alice"}},
		Spec:       v1alpha1.WorkflowSpec{TemplateRef: "pr-review"},
	}
	tmpl := &v1alpha1.WorkflowTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "pr-review", Namespace: "test-ns"},
		Spec:       v1alpha1.WorkflowTemplateSpec{Agent: v1alpha1.AgentSpec{Model: "litellm/zai/glm-5.3", Skill: "/skills/pr-review/SKILL.md"}},
	}
	att := &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name: "attempt-pr-review-x-1", Namespace: "test-ns",
			Labels: map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec:   v1alpha1.AttemptSpec{WorkflowRef: "test-ns/pr-review-x", Owner: "alice"},
		Status: v1alpha1.AttemptStatus{Runs: []v1alpha1.RunRecord{{Name: "pool-pod-1", Phase: "skipped"}}},
	}
	srv := newAttemptTestServer(t, wf, tmpl, att)
	srv.logger = slog.Default()

	req := httptest.NewRequest("GET", "/attempts/attempt-pr-review-x-1/runs/pool-pod-1/session", nil)
	req = req.WithContext(withTestIdentity(req.Context()))
	req.SetPathValue("name", "attempt-pr-review-x-1")
	req.SetPathValue("job", "pool-pod-1")
	rec := httptest.NewRecorder()
	srv.handleAttemptSession(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "deterministic workflow") {
		t.Error("template-delegated workflow mislabeled as deterministic")
	}
	if !strings.Contains(body, "Session transcript not available") {
		t.Error("expected the neutral no-record state")
	}
}

// #243: the forkable pi session download. Owner-gated, gz1-decoded, sensible
// filename; 404 when the run predates session persistence.
func TestPiSessionDownload(t *testing.T) {
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "pr-review-x", Namespace: "test-ns", Labels: map[string]string{v1alpha1.OwnerLabel: "alice"}},
	}
	att := &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name: "attempt-pr-review-x-1", Namespace: "test-ns",
			Labels: map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec:   v1alpha1.AttemptSpec{WorkflowRef: "test-ns/pr-review-x", Owner: "alice"},
		Status: v1alpha1.AttemptStatus{Runs: []v1alpha1.RunRecord{{Name: "pool-pod-1"}}},
	}
	srv := newAttemptTestServer(t, wf, att)
	srv.logger = slog.Default()

	get := func(user string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/x", nil)
		req = req.WithContext(withIdentity(req.Context(), &Identity{Username: user}))
		req.SetPathValue("name", "attempt-pr-review-x-1")
		req.SetPathValue("job", "pool-pod-1")
		rec := httptest.NewRecorder()
		srv.handleAttemptPiSession(rec, req)
		return rec
	}

	// No stored session → 404.
	if rec := get("alice"); rec.Code != http.StatusNotFound {
		t.Fatalf("absent session: code=%d, want 404", rec.Code)
	}
	// Cross-owner → 404 (before any state read).
	if rec := get("bob"); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-owner: code=%d, want 404", rec.Code)
	}

	// Store a real payload through the worker's encoder (round-trip contract).
	srv.dapr = &piSessionDapr{payload: mustPiPayload(t, `{"type":"message_end","ok":true}`)}
	rec := get("alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("present session: code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson; charset=utf-8" {
		t.Errorf("content-type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, `filename="pi-session-pool-pod-1.jsonl"`) {
		t.Errorf("content-disposition = %q", cd)
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Error("decoded body wrong")
	}
}

type piSessionDapr struct {
	DaprClient
	payload string
}

func (d *piSessionDapr) GetStateFromStore(_ context.Context, _, key string, value any) (bool, error) {
	// metadata key: the O(1) probe target
	if strings.HasSuffix(key, ":pi-session") && !strings.HasSuffix(key, ":pi-session/data") {
		b, _ := json.Marshal(map[string]any{"bytes": 10, "savedAt": "2026-08-26T00:00:00Z"})
		if err := json.Unmarshal(b, value); err != nil {
			return false, err
		}
		return true, nil
	}
	// data key: the gz1 blob
	if strings.HasSuffix(key, ":pi-session/data") {
		if s, ok := value.(*string); ok {
			*s = d.payload
			return true, nil
		}
		return false, nil
	}
	// transcript key: hydrate whatever struct the handler passed
	b, _ := json.Marshal(map[string]any{
		"workflow": "pr-review-x", "runId": "pool-pod-1",
		"model": "litellm/zai/glm-5.3", "green": true,
		"turns": []map[string]any{{"label": "initial task", "response": "done"}},
	})
	if err := json.Unmarshal(b, value); err != nil {
		return false, err
	}
	return true, nil
}

func mustPiPayload(t *testing.T, body string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return "gz1:" + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// The session page reveals the fork button exactly when a session exists.
func TestSessionPageForkButtonVisibility(t *testing.T) {
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "pr-review-x", Namespace: "test-ns", Labels: map[string]string{v1alpha1.OwnerLabel: "alice"}},
	}
	att := &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name: "attempt-pr-review-x-1", Namespace: "test-ns",
			Labels: map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec:   v1alpha1.AttemptSpec{WorkflowRef: "test-ns/pr-review-x", Owner: "alice"},
		Status: v1alpha1.AttemptStatus{Runs: []v1alpha1.RunRecord{{Name: "pool-pod-1", Phase: "succeeded"}}},
	}

	render := func(daprSet *piSessionDapr) string {
		srv := newAttemptTestServer(t, wf, att)
		srv.logger = slog.Default()
		if daprSet != nil {
			srv.dapr = daprSet
		}
		req := httptest.NewRequest("GET", "/x", nil)
		req = req.WithContext(withTestIdentity(req.Context()))
		req.SetPathValue("name", "attempt-pr-review-x-1")
		req.SetPathValue("job", "pool-pod-1")
		rec := httptest.NewRecorder()
		srv.handleAttemptSession(rec, req)
		return rec.Body.String()
	}

	if strings.Contains(render(nil), "Fork session") {
		t.Error("fork button rendered without a stored session")
	}
	if body := render(&piSessionDapr{payload: mustPiPayload(t, "{}")}); !strings.Contains(body, "Fork session") {
		t.Error("fork button missing when a session exists")
	}
}

// ADR-0007 phase 5: review attempts roll up per PR; scheduled classes
// collapse to a last-run row per workflow.
func TestGroupAttemptsRollup(t *testing.T) {
	now := time.Now()
	mk := func(name, wf, kind, subject string, lastRun time.Time, review *v1alpha1.ReviewClaimStatus) v1alpha1.Attempt {
		a := v1alpha1.Attempt{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "harmostes",
				CreationTimestamp: metav1.NewTime(lastRun),
			},
			Spec: v1alpha1.AttemptSpec{
				WorkflowRef: "harmostes/" + wf,
				Objective: v1alpha1.ObjectiveSpec{
					Kind:           kind,
					PrimarySubject: v1alpha1.Subject{Binding: "review", Object: subject},
				},
			},
			Status: v1alpha1.AttemptStatus{LastRunAt: metav1.NewTime(lastRun)},
		}
		if review != nil {
			a.Status.Review = review
		}
		return a
	}

	inFlight := &v1alpha1.ReviewClaimStatus{PR: "github.com/tibrezus/harmostes#99", HeadSHA: "aaa", DispatchedAt: &metav1.Time{Time: now.Add(-30 * time.Minute)}}
	consumed := &v1alpha1.ReviewClaimStatus{PR: "github.com/tibrezus/harmostes#99", HeadSHA: "bbb", Released: true, ReleaseReason: "consumed"}
	attempts := []v1alpha1.Attempt{
		mk("a1", "pr-review-harmostes", v1alpha1.ObjectiveKindPRReview, "tibrezus/harmostes", now.Add(-1*time.Hour), inFlight),
		mk("a2", "pr-review-harmostes", v1alpha1.ObjectiveKindPRReview, "tibrezus/harmostes", now.Add(-3*time.Hour), consumed),
		mk("a3", "wiki-lint", "documentation-sync", "rezuscloud/llm-wiki", now.Add(-2*time.Hour), nil),
		mk("a4", "wiki-lint", "documentation-sync", "rezuscloud/llm-wiki", now.Add(-30*24*time.Hour), nil), // outside 24h
	}

	groups := groupAttempts(attempts, now.Add(-24*time.Hour))
	if len(groups) != 2 {
		t.Fatalf("two rollup rows (PR group + workflow last-run), got %d: %+v", len(groups), groups)
	}
	bySubject := map[string]attemptGroup{}
	for _, g := range groups {
		bySubject[g.Subject] = g
	}
	pr := bySubject["github.com/tibrezus/harmostes#99"]
	if !pr.IsReview || pr.Count != 2 || pr.ClaimState != "review in flight" {
		t.Fatalf("PR rollup wrong: %+v", pr)
	}
	if len(pr.Attempts) != 2 || pr.Attempts[0].Name != "a1" {
		t.Fatalf("PR group must list newest attempt first: %+v", pr.Attempts)
	}
	wl := bySubject["rezuscloud/llm-wiki"]
	if wl.IsReview || wl.Count != 1 {
		t.Fatalf("scheduled class must collapse to last-run within window, got %+v", wl)
	}
}

// The claim state vocabulary (ADR-0007).
func TestClaimStateWords(t *testing.T) {
	cases := map[string]*v1alpha1.ReviewClaimStatus{
		"review in flight":         {DispatchedAt: &metav1.Time{Time: time.Now()}},
		"queued":                   {},
		"verdict posted":           {Released: true, ReleaseReason: "consumed"},
		"superseded by newer head": {Released: true, ReleaseReason: "superseded"},
		"run expired":              {Released: true, ReleaseReason: "dispatch-timeout"},
		"horizon reached":          {Released: true, ReleaseReason: "horizon"},
	}
	for want, r := range cases {
		if got := claimState(r); got != want {
			t.Errorf("claimState(%+v) = %q, want %q", r, got, want)
		}
	}
}
