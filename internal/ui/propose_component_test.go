package ui_test

// Component tier for the MR bridge (#420): a fake forge (httptest) plays the
// template's git source; the fixture server proposes against it. These pin
// the forge-call choreography (branch → commit → MR, right headers, right
// bodies) and the detail page's propose-panel DOM contract.

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/tibrezus/harmostes/internal/ui"
	"github.com/tibrezus/harmostes/internal/ui/fixture"
)

const committedValuesFile = `namespace: fixture-ns
workflowTemplates:
  # The PR review archetype.
  pr-review:
    description: PR review
    agent:
      model: mistral-small-latest
  other:
    description: untouched
`

// fakeForge records the bridge's calls in order and serves just enough of
// the GitHub REST surface: ref → contents → ref → contents → pulls.
type fakeForge struct {
	t          *testing.T
	secret     string
	calls      []string
	blobSHA    string
	baseSHA    string
	branchName string
	commitBody map[string]any
	prBody     map[string]any
	mrURL      string
}

func (f *fakeForge) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/golden-owner/golden-repo/git/ref/heads/main", func(w http.ResponseWriter, r *http.Request) {
		f.assertAuth(r)
		f.calls = append(f.calls, "ref")
		write(w, `{"object":{"sha":"`+f.baseSHA+`"}}`)
	})
	mux.HandleFunc("GET /repos/golden-owner/golden-repo/contents/chart/values.yaml", func(w http.ResponseWriter, r *http.Request) {
		f.assertAuth(r)
		f.calls = append(f.calls, "get-contents")
		b64 := encodeB64(committedValuesFile)
		write(w, `{"sha":"`+f.blobSHA+`","content":"`+b64+`"}`)
	})
	mux.HandleFunc("POST /repos/golden-owner/golden-repo/git/refs", func(w http.ResponseWriter, r *http.Request) {
		f.assertAuth(r)
		f.calls = append(f.calls, "create-branch")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.branchName, _ = body["ref"].(string)
		write(w, `{}`)
	})
	mux.HandleFunc("PUT /repos/golden-owner/golden-repo/contents/chart/values.yaml", func(w http.ResponseWriter, r *http.Request) {
		f.assertAuth(r)
		f.calls = append(f.calls, "put-contents")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.commitBody = body
		write(w, `{"commit":{"sha":"abc"}}`)
	})
	mux.HandleFunc("POST /repos/golden-owner/golden-repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		f.assertAuth(r)
		f.calls = append(f.calls, "pulls")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.prBody = body
		write(w, `{"html_url":"`+f.mrURL+`"}`)
	})
	return mux
}

func (f *fakeForge) assertAuth(r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Bearer "+f.secret {
		f.t.Errorf("forge call %s carried Authorization %q, want the server-side token", r.URL.Path, got)
	}
}

func write(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

func encodeB64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func decodeB64(s string) string {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "(b64 decode failed: " + err.Error() + ")"
	}
	return string(b)
}

func discardLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// newProposeServer builds a fixture server whose template source points at
// the fake forge. Returns the public server and the forge.
func newProposeServer(t *testing.T) (*httptest.Server, *fakeForge) {
	t.Helper()
	forge := &fakeForge{
		t:       t,
		secret:  "server-only-token",
		baseSHA: "basesha",
		blobSHA: "blobsha",
		mrURL:   "https://github.com/golden-owner/golden-repo/pull/7",
	}
	forgeSrv := httptest.NewServer(forge.handler())
	t.Cleanup(forgeSrv.Close)

	logger := discardLogger(t)
	srv, err := fixture.NewWorld(fixtureNamespace, logger)
	if err != nil {
		t.Fatalf("fixture server: %v", err)
	}
	srv.Server().SetTemplateSource(&ui.TemplateSource{
		Host: "github", Owner: "golden-owner", Repo: "golden-repo",
		BaseBranch: "main", Path: "chart/values.yaml", ValuesKey: "workflowTemplates",
		APIBase: forgeSrv.URL,
	})
	srv.Server().SetSourceToken(forge.secret)
	ts := httptest.NewServer(srv.Server().Routes())
	t.Cleanup(ts.Close)
	return ts, forge
}

// postPropose posts a document to the propose route as the fixture user.
func postPropose(t *testing.T, ts *httptest.Server, name, document string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/templates/"+name+"/propose",
		strings.NewReader(`{"document":`+mustQuote(document)+`}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Harmostes-Dev-User", fixture.DevUser)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST propose: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(strings.SplitN(s, "— ", 2)[len(strings.SplitN(s, "— ", 2))-1])
}

func mustQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

const editedDocument = `apiVersion: harmostes.dev/v1alpha1
kind: WorkflowTemplate
metadata:
  name: pr-review
  labels:
    harmostes.dev/owner: whatever
spec:
  description: PR review
  agent:
    model: llama3:8b
`

// TestComponent_Propose_HappyPath: the whole choreography against the fake
// forge — branch named after the template, the spliced values file
// committed (other templates byte-intact), an MR body carrying the diff,
// and the MR URL handed back. The token never appears in the response.
func TestComponent_Propose_HappyPath(t *testing.T) {
	ts, forge := newProposeServer(t)
	code, body := postPropose(t, ts, "pr-review", editedDocument)
	if code != http.StatusOK {
		t.Fatalf("propose status = %d: %s", code, firstLine(body))
	}

	// Choreography order: ref → contents → branch → commit → MR.
	want := []string{"ref", "get-contents", "create-branch", "put-contents", "pulls"}
	if strings.Join(forge.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("forge calls = %v, want %v", forge.calls, want)
	}
	if forge.branchName == "" || !strings.HasPrefix(forge.branchName, "refs/heads/harmostes-ui/pr-review-") {
		t.Errorf("branch ref = %q, want refs/heads/harmostes-ui/pr-review-*", forge.branchName)
	}

	// The committed content: the new model in, the old out, the untouched
	// template + comment preserved (the surgical edit, end to end).
	contentB64, _ := forge.commitBody["content"].(string)
	content := decodeB64(contentB64)
	if !strings.Contains(content, "llama3:8b") || strings.Contains(content, "mistral-small-latest") {
		t.Error("committed file does not carry the edit")
	}
	if !strings.Contains(content, "# The PR review archetype.") || !strings.Contains(content, "other:") {
		t.Error("committed file lost untouched content — the edit was not surgical")
	}
	if forge.commitBody["sha"] != "blobsha" {
		t.Error("commit must carry the blob sha (fast-forward guard)")
	}

	// The MR: targets main, body names the author + diff lines.
	if forge.prBody["base"] != "main" || forge.prBody["head"] != strings.TrimPrefix(forge.branchName, "refs/heads/") {
		t.Errorf("MR head/base = %v/%v", forge.prBody["head"], forge.prBody["base"])
	}
	prBodyStr, _ := forge.prBody["body"].(string)
	if !strings.Contains(prBodyStr, "fixture-user") {
		t.Errorf("MR body lacks the author: %q", prBodyStr)
	}
	hasMarked := func(mark, needle string) bool {
		for _, line := range strings.Split(prBodyStr, "\n") {
			if strings.HasPrefix(line, mark) && strings.Contains(line, needle) {
				return true
			}
		}
		return false
	}
	if !hasMarked("-", "model: mistral-small-latest") {
		t.Errorf("MR body lacks the removed old model: %q", prBodyStr)
	}
	if !hasMarked("+", "model: llama3:8b") {
		t.Errorf("MR body lacks the added new model: %q", prBodyStr)
	}

	// The response links the MR; the token never leaves the server.
	var out struct {
		URL    string `json:"url"`
		Branch string `json:"branch"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("response not JSON: %s", body)
	}
	if out.URL != forge.mrURL {
		t.Errorf("response url = %q, want %q", out.URL, forge.mrURL)
	}
	if strings.Contains(body, forge.secret) {
		t.Fatal("THE TOKEN LEAKED INTO THE RESPONSE — server-side credentials must never be rendered")
	}
}

// TestComponent_Propose_Rejections: the guard chain in order.
func TestComponent_Propose_Rejections(t *testing.T) {
	ts, _ := newProposeServer(t)

	// Read-only identity: rejected BEFORE anything else runs.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/templates/pr-review/propose",
		strings.NewReader(`{"document":"kind: WorkflowTemplate"}`))
	req.Header.Set("X-Forwarded-User", "reader")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("read-only identity: status = %d, want 403", resp.StatusCode)
	}

	// Document names a different template than the page.
	if code, _ := postPropose(t, ts, "other", editedDocument); code != http.StatusBadRequest {
		t.Errorf("name mismatch: status = %d, want 400", code)
	}
	// Broken YAML.
	if code, _ := postPropose(t, ts, "pr-review", "not: [valid"); code != http.StatusBadRequest {
		t.Errorf("broken YAML: status = %d, want 400", code)
	}
	// Identical document — nothing to propose.
	if code, _ := postPropose(t, ts, "pr-review", `apiVersion: harmostes.dev/v1alpha1
kind: WorkflowTemplate
metadata:
  name: pr-review
spec:
  description: PR review
  agent:
    model: mistral-small-latest
`); code != http.StatusBadRequest {
		t.Errorf("identical document: status = %d, want 400", code)
	}
}

// TestComponent_Propose_Unconfigured: no source configured → 501 and the
// detail page renders NO propose panel (absent surface, not an error page).
// The fixture world ships a fictional source by default, so this test
// unsets it explicitly.
func TestComponent_Propose_Unconfigured(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := fixture.NewWorld(fixtureNamespace, logger)
	if err != nil {
		t.Fatalf("fixture server: %v", err)
	}
	srv.Server().SetTemplateSource(nil)
	ts := httptest.NewServer(srv.Server().Routes())
	t.Cleanup(ts.Close)

	if code, body := postPropose(t, ts, "pr-review", editedDocument); code != http.StatusNotImplemented {
		t.Errorf("unconfigured: status = %d (%s), want 501", code, body)
	}
	doc := getAsFixtureUser(t, ts, "/templates/pr-review")
	if n := doc.Find(`[data-testid="propose-panel"]`).Length(); n != 0 {
		t.Error("unconfigured server rendered a propose panel")
	}
}

// TestComponent_ProposePanel_DOM: with a source configured, the panel shows
// the target coordinates and the editable state gates it.
func TestComponent_ProposePanel_DOM(t *testing.T) {
	ts, _ := newProposeServer(t)
	doc := getAsFixtureUser(t, ts, "/templates/pr-review")
	if n := doc.Find(`[data-testid="propose-panel"]`).Length(); n != 1 {
		t.Fatal("propose panel missing")
	}
	if src := doc.Find(`[data-testid="propose-source"]`).Text(); !strings.Contains(src, "golden-owner/golden-repo") || !strings.Contains(src, "chart/values.yaml") {
		t.Errorf("source label = %q, want repo + path", src)
	}
	if n := doc.Find(`[data-testid="propose-button"]`).Length(); n != 1 {
		t.Error("propose button missing")
	}
	// Historical revisions never propose — the past is read-only.
	doc = getAsFixtureUser(t, ts, "/templates/pr-review?rev=1")
	if n := doc.Find(`[data-testid="propose-panel"]`).Length(); n != 0 {
		t.Error("historical revision rendered the propose panel")
	}
}
