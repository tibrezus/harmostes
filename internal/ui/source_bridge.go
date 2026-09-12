package ui

// The MR round-trip bridge (ADR-0012 §5, #420): templates are never mutated
// in-cluster. An inspector/Workflow-Code edit leaves the template detail
// page as a PROPOSAL — the server performs a surgical, structure-preserving
// values edit on the template's git source file, commits it to a working
// branch, and opens an MR/PR. Flux reconciliation (chart release) stays the
// delivery path; the template-history recorder then records what landed.
//
// Security posture:
//   - the source token is a server-side env (ExternalSecret); it is never
//     rendered into any page, fragment, or error message;
//   - the write gate is the same mayWrite chain as every mutating route;
//   - the values edit is structural (yaml.Node): everything except the one
//     workflowTemplates.<name> key is preserved byte-for-byte in spirit —
//     comments, ordering, and quoting survive, so review diffs show only
//     the template.
//
// Two hosts are implemented because those are the two real sources
// (kernel chart on GitHub, k8s-config overrides on GitLab); the client
// surface is deliberately tiny (branch, commit, MR) so adding a host is
// ~40 lines, and the REST calls stay raw net/http — no SDK dependency.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	goyaml "gopkg.in/yaml.v3"
	sigsyaml "sigs.k8s.io/yaml"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/graph"
)

// TemplateSource describes where the environment's template values live.
// One environment, one source: the file this environment's HelmRelease
// draws workflowTemplates values from (dev = kernel chart values, prod =
// whatever its pinned release overrides).
type TemplateSource struct {
	Host       string `json:"host"`       // "github" | "gitlab"
	Owner      string `json:"owner"`      // repo owner or namespace
	Repo       string `json:"repo"`       // repository name
	BaseBranch string `json:"baseBranch"` // the branch MRs target
	Path       string `json:"path"`       // values file inside the repo
	ValuesKey  string `json:"valuesKey"`  // top-level key holding templates ("workflowTemplates")
	APIBase    string `json:"apiBase"`    // override the REST root (self-hosted hosts); empty = host default
}

// ParseTemplateSource decodes the HARMOSTES_TEMPLATE_SOURCE env JSON.
// An empty/unset value means "no source configured" — the propose surface
// degrades to absent, never to an error page.
func ParseTemplateSource(raw string) (*TemplateSource, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var ts TemplateSource
	if err := json.Unmarshal([]byte(raw), &ts); err != nil {
		return nil, fmt.Errorf("parse template source: %w", err)
	}
	if ts.Host == "" || ts.Owner == "" || ts.Repo == "" || ts.Path == "" || ts.ValuesKey == "" {
		return nil, fmt.Errorf("template source needs host, owner, repo, path and valuesKey")
	}
	if ts.BaseBranch == "" {
		ts.BaseBranch = "main"
	}
	return &ts, nil
}

// SetTemplateSource wires the configured source (server tests use this).
func (s *Server) SetTemplateSource(ts *TemplateSource) { s.templateSource = ts }

// SetSourceToken wires the forge token (branch + MR creation only). It is
// injected by the chart from an ExternalSecret and held server-side only —
// never logged, never rendered, never sent to any client.
func (s *Server) SetSourceToken(token string) { s.sourceToken = token }

// SourceTokenEnv names the env the main binary reads the token from.
const SourceTokenEnv = "HARMOSTES_TEMPLATE_SOURCE_TOKEN"

// proposeRequest is the JSON body the template page posts: the island's
// current document (same shape templateYAML renders).
type proposeRequest struct {
	Document string `json:"document"`
}

// proposeResponse carries the created MR/PR — the page links to it.
type proposeResponse struct {
	URL    string `json:"url"`
	Branch string `json:"branch"`
}

// sourceSummary is the detail-page's view of the configured source.
type sourceSummary struct {
	Configured bool
	Label      string // "github:tibrezus/harmostes · chart/values.yaml"
}

func sourceSummaryOf(ts *TemplateSource) *sourceSummary {
	if ts == nil {
		return nil
	}
	return &sourceSummary{
		Configured: true,
		Label:      ts.Host + ":" + ts.Owner + "/" + ts.Repo + " · " + ts.Path,
	}
}

// handleTemplatePropose POSTs an edited document to the template's git
// source as a branch + MR. Guard order mirrors the lifecycle routes:
// identity gate first, then validation, then the forge calls.
func (s *Server) handleTemplatePropose(w http.ResponseWriter, r *http.Request) {
	id := identityFromContext(r.Context())
	if !s.mayWrite(id) {
		s.renderErrorStatus(w, r, http.StatusForbidden, "You do not have write access — template changes are proposed via MR by writers only")
		return
	}
	ts := s.templateSource
	if ts == nil {
		s.renderErrorStatus(w, r, http.StatusNotImplemented, "No template source is configured for this environment")
		return
	}
	var req proposeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Document) == "" {
		s.renderErrorStatus(w, r, http.StatusBadRequest, "A document is required (JSON body {document: <yaml>})")
		return
	}

	// The document must be the real thing: parse through the same typed
	// shape the page renders from (sigsyaml — the JSON-tag projection the
	// page marshals from), name pinned to the URL's template.
	var doc templateDocument
	if err := sigsyaml.Unmarshal([]byte(req.Document), &doc); err != nil {
		s.renderErrorStatus(w, r, http.StatusBadRequest, "Document is not valid YAML: "+err.Error())
		return
	}
	name := r.PathValue("name")
	if doc.Kind != "WorkflowTemplate" || doc.APIVersion != v1alpha1.SchemeGroupVersion.Identifier() {
		s.renderErrorStatus(w, r, http.StatusBadRequest, "Document must be a WorkflowTemplate ("+v1alpha1.SchemeGroupVersion.Identifier()+")")
		return
	}
	if doc.Metadata.Name != name {
		s.renderErrorStatus(w, r, http.StatusBadRequest,
			fmt.Sprintf("Document names %q but this is %q's page — templates are proposed one page at a time", doc.Metadata.Name, name))
		return
	}

	// The spec must compile: an MR that would never reconcile helps nobody.
	// One compiler, one defaults policy — the same graph.CompileTemplate the
	// projections and the controller use.
	graph.CompileTemplate(&v1alpha1.WorkflowTemplate{Spec: doc.Spec})

	ctx := r.Context()
	forge := s.forgeFor(ts, s.sourceToken)

	baseSHA, err := forge.branchHead(ctx, ts.BaseBranch)
	if err != nil {
		s.renderErrorStatus(w, r, http.StatusBadGateway, "Cannot read "+ts.Host+" source: "+err.Error())
		return
	}
	blobSHA, currentRaw, err := forge.getFile(ctx, ts.Path, ts.BaseBranch)
	if err != nil {
		s.renderErrorStatus(w, r, http.StatusBadGateway, "Cannot read "+ts.Path+" on "+ts.Host+": "+err.Error())
		return
	}

	updated, oldSpecYAML, newSpecYAML, err := spliceTemplateIntoValues(string(currentRaw), ts.ValuesKey, name, doc.Spec)
	if err != nil {
		s.renderErrorStatus(w, r, http.StatusConflict, err.Error())
		return
	}
	// Semantic no-op check: canonical new spec vs canonical committed spec.
	// A formatting-only document still says the same thing about the
	// template — proposing it would open an empty MR.
	if strings.TrimSpace(newSpecYAML) == strings.TrimSpace(oldSpecYAML) {
		s.renderErrorStatus(w, r, http.StatusBadRequest, "The document is identical to the committed template — nothing to propose")
		return
	}

	branch := fmt.Sprintf("harmostes-ui/%s-%d", name, time.Now().UnixNano()%1_000_000)
	commitMsg := fmt.Sprintf("templates(%s): propose via harmostes UI\n\nAuthored in the harmostes UI by %s; applies the edited\ntemplate to the chart values (ADR-0012 §5: git stays the source of\ntruth — the cluster receives this through Flux reconciliation).", name, id.Username)
	if err := forge.createBranch(ctx, branch, baseSHA); err != nil {
		s.renderErrorStatus(w, r, http.StatusBadGateway, "Cannot create branch on "+ts.Host+": "+err.Error())
		return
	}
	if err := forge.commitFile(ctx, ts.Path, branch, commitMsg, []byte(updated), blobSHA); err != nil {
		s.renderErrorStatus(w, r, http.StatusBadGateway, "Cannot commit to "+branch+" on "+ts.Host+": "+err.Error())
		return
	}

	title := fmt.Sprintf("templates: update %s (via harmostes UI)", name)
	body := fmt.Sprintf(
		"**Template:** `%s`\n**Author:** %s\n**Source:** proposed from the harmostes UI (template detail page)\n\n```diff\n%s\n```",
		name, id.Username, diffBody(oldSpecYAML, newSpecYAML))
	mrURL, err := forge.openMR(ctx, branch, title, body)
	if err != nil {
		s.renderErrorStatus(w, r, http.StatusBadGateway, "Cannot open the merge request on "+ts.Host+": "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(proposeResponse{URL: mrURL, Branch: branch})
}

// diffBody renders a compact line diff for the MR body (the LCS line diff
// the revisions page uses — one diff machinery, two surfaces).
func diffBody(a, b string) string {
	var out []string
	for _, l := range lineDiff(a, b) {
		mark := " "
		switch l.Kind {
		case diffLineAdded:
			mark = "+"
		case diffLineRemoved:
			mark = "-"
		}
		out = append(out, mark+" "+l.Text)
	}
	return strings.Join(out, "\n")
}

// pruneEmpty drops zero-value leaves from a spec node so the committed
// values file stays as sparse as hand-written values: empty strings, empty
// mappings and sequences go; meaningful scalars (false, 0 — an explicit
// "disabled" is a real setting) survive.
func pruneEmpty(n *goyaml.Node) {
	if n.Kind != goyaml.MappingNode {
		if n.Kind == goyaml.SequenceNode {
			for _, c := range n.Content {
				pruneEmpty(c)
			}
		}
		return
	}
	kept := make([]*goyaml.Node, 0, len(n.Content))
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i], n.Content[i+1]
		pruneEmpty(val)
		if isEmptyLeaf(val) {
			continue
		}
		kept = append(kept, key, val)
	}
	n.Content = kept
}

func isEmptyLeaf(n *goyaml.Node) bool {
	switch n.Kind {
	case goyaml.ScalarNode:
		return n.Tag == "!!null" || (n.Tag == "!!str" && n.Value == "")
	case goyaml.MappingNode, goyaml.SequenceNode:
		return len(n.Content) == 0
	}
	return false
}

// marshalSpecPlainOf marshals an already-parsed YAML node (the committed
// spec as the file holds it) — same plain-entry shape as marshalSpecPlain.
func marshalSpecPlainOf(node *goyaml.Node) string {
	var out strings.Builder
	enc := goyaml.NewEncoder(&out)
	enc.SetIndent(2) // sigsyaml's canonical indent — both diff sides match
	if err := enc.Encode(node); err != nil {
		return "(spec unrenderable: " + err.Error() + ")"
	}
	_ = enc.Close()
	return out.String()
}

// spliceTemplateIntoValues performs the surgical values edit: locate the
// <ValuesKey>.<name> entry, replace ONLY those source lines with the new
// spec, and leave every other byte of the file untouched. The parse tree
// (yaml.v3 nodes carry Line) is used purely for LOCATING the entry — the
// file is spliced as text, because a node-tree re-marshal is lossy: it
// drops blank lines, normalizes inline-comment padding, and rewrites block
// scalars (caught live on harmostes-dev, #420). Everything outside the
// entry — header comments, sibling templates, task prompts — survives
// byte-for-byte.
// Returns the updated file text plus the old and new spec in canonical
// form (sorted keys, sparse) for the semantic no-op check and MR-body diff.
func spliceTemplateIntoValues(raw, valuesKey, name string, spec v1alpha1.WorkflowTemplateSpec) (updated, oldSpecYAML, newSpecYAML string, err error) {
	var root goyaml.Node
	if err := goyaml.Unmarshal([]byte(raw), &root); err != nil {
		return "", "", "", fmt.Errorf("source file is not valid YAML: %w", err)
	}
	if len(root.Content) == 0 {
		return "", "", "", fmt.Errorf("source file %s is empty", valuesKey)
	}
	top := root.Content[0] // the document mapping
	if top.Kind != goyaml.MappingNode {
		return "", "", "", fmt.Errorf("source file is not a mapping — cannot splice templates")
	}

	// Locate the values-key mapping and its next sibling key's line (the
	// parent block's end when the entry is its last child).
	parentBlockEnd := len(strings.Split(raw, "\n")) + 1
	var templatesNode *goyaml.Node
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value == valuesKey {
			templatesNode = top.Content[i+1]
			if i+2 < len(top.Content) {
				parentBlockEnd = top.Content[i+2].Line // 1-based Line of the next top-level key
			}
			break
		}
	}
	if templatesNode == nil || templatesNode.Kind != goyaml.MappingNode {
		return "", "", "", fmt.Errorf("no %q mapping in the source file", valuesKey)
	}

	// Marshal the new spec through the JSON-tag projection (the values
	// file's keys are the struct's json names), prune zero leaves, and
	// render its canonical text (sorted keys, 2-space indent).
	specYAML, err := sigsyaml.Marshal(spec)
	if err != nil {
		return "", "", "", fmt.Errorf("marshal spec: %w", err) // typed struct; unreachable
	}
	var specNode goyaml.Node
	if err := goyaml.Unmarshal(specYAML, &specNode); err != nil {
		return "", "", "", fmt.Errorf("re-parse spec: %w", err) // our own output
	}
	newMapping := specNode.Content[0]
	pruneEmpty(newMapping)
	newSpecYAML = marshalSpecPlainOf(newMapping)

	// Locate the target entry: its key line, and the first line before its
	// next sibling (or the parent block's end when it is the last entry).
	entryStart, entryEnd := -1, -1 // 0-based inclusive start, exclusive end
	var oldValueNode *goyaml.Node
	for i := 0; i+1 < len(templatesNode.Content); i += 2 {
		key := templatesNode.Content[i]
		nextStart := parentBlockEnd
		if i+2 < len(templatesNode.Content) {
			nextStart = templatesNode.Content[i+2].Line
		}
		if key.Value != name {
			continue
		}
		entryStart = key.Line - 1 // Line is 1-based
		entryEnd = nextStart - 1  // exclusive
		oldValueNode = templatesNode.Content[i+1]
		break
	}
	if oldValueNode == nil {
		return "", "", "", fmt.Errorf("template %q is not present in the source file — the source has drifted; reconcile first", name)
	}

	// Canonical old spec for the semantic no-op check and the MR-body diff.
	var prev map[string]any
	if err := oldValueNode.Decode(&prev); err == nil {
		if b, err := sigsyaml.Marshal(prev); err == nil {
			oldSpecYAML = string(b)
		}
	}
	if oldSpecYAML == "" {
		oldSpecYAML = marshalSpecPlainOf(oldValueNode)
	}

	// Byte-range splice: everything before the entry and from the entry's
	// end on stays VERBATIM. Trailing blank lines inside the replaced range
	// are kept so block separation survives.
	lines := strings.Split(raw, "\n")
	if entryEnd > len(lines) {
		entryEnd = len(lines)
	}
	indentLen := len(lines[entryStart]) - len(strings.TrimLeft(lines[entryStart], " "))
	indent := strings.Repeat(" ", indentLen)

	var body strings.Builder
	for _, line := range strings.Split(strings.TrimSuffix(newSpecYAML, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			body.WriteString("\n")
		} else {
			body.WriteString(indent + "  " + line + "\n")
		}
	}
	trailing := ""
	for entryEnd-1 > entryStart && strings.TrimSpace(lines[entryEnd-1]) == "" {
		trailing = "\n" + trailing
		entryEnd--
	}

	var out strings.Builder
	if entryStart > 0 {
		out.WriteString(strings.Join(lines[:entryStart], "\n") + "\n")
	}
	out.WriteString(indent + name + ":\n")
	out.WriteString(body.String())
	out.WriteString(trailing)
	if entryEnd < len(lines) {
		out.WriteString(strings.Join(lines[entryEnd:], "\n"))
	}
	return out.String(), oldSpecYAML, newSpecYAML, nil
}

// ── forge client surface ─────────────────────────────────────────────────────
// Three operations per host: read a branch head, read/write one file, open
// a merge request. Everything else (issues, releases, …) is out of scope by
// design — the token needs nothing more.

type forgeClient interface {
	branchHead(ctx context.Context, branch string) (string, error)
	getFile(ctx context.Context, path, ref string) (blobSHA string, content []byte, err error)
	createBranch(ctx context.Context, name, sha string) error
	commitFile(ctx context.Context, path, branch, message string, content []byte, blobSHA string) error
	openMR(ctx context.Context, branch, title, body string) (string, error)
}

func (s *Server) forgeFor(ts *TemplateSource, token string) forgeClient {
	switch ts.Host {
	case "gitlab":
		return &gitlabClient{ts: ts, token: token, hc: http.DefaultClient}
	default:
		return &githubClient{ts: ts, token: token, hc: http.DefaultClient}
	}
}

// githubClient — REST v3, raw net/http.
type githubClient struct {
	ts    *TemplateSource
	token string
	hc    *http.Client
}

func (g *githubClient) apiBase() string {
	if g.ts.APIBase != "" {
		return strings.TrimSuffix(g.ts.APIBase, "/")
	}
	return "https://api.github.com"
}

func (g *githubClient) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, g.apiBase()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (g *githubClient) branchHead(ctx context.Context, branch string) (string, error) {
	var out struct {
		Object struct{ SHA string } `json:"object"`
	}
	if err := g.do(ctx, http.MethodGet, "/repos/"+g.ts.Owner+"/"+g.ts.Repo+"/git/ref/heads/"+url.PathEscape(branch), nil, &out); err != nil {
		return "", err
	}
	return out.Object.SHA, nil
}

func (g *githubClient) getFile(ctx context.Context, path, ref string) (string, []byte, error) {
	var out struct {
		SHA     string `json:"sha"`
		Content string `json:"content"`
	}
	p := "/repos/" + g.ts.Owner + "/" + g.ts.Repo + "/contents/" + path + "?ref=" + url.QueryEscape(ref)
	if err := g.do(ctx, http.MethodGet, p, nil, &out); err != nil {
		return "", nil, err
	}
	content, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
	if err != nil {
		return "", nil, fmt.Errorf("decode file content: %w", err)
	}
	return out.SHA, content, nil
}

func (g *githubClient) createBranch(ctx context.Context, name, sha string) error {
	body := map[string]any{"ref": "refs/heads/" + name, "sha": sha}
	return g.do(ctx, http.MethodPost, "/repos/"+g.ts.Owner+"/"+g.ts.Repo+"/git/refs", body, nil)
}

func (g *githubClient) commitFile(ctx context.Context, path, branch, message string, content []byte, blobSHA string) error {
	body := map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(content),
		"branch":  branch,
		"sha":     blobSHA,
	}
	return g.do(ctx, http.MethodPut, "/repos/"+g.ts.Owner+"/"+g.ts.Repo+"/contents/"+path, body, nil)
}

func (g *githubClient) openMR(ctx context.Context, branch, title, body string) (string, error) {
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	payload := map[string]any{"title": title, "body": body, "head": branch, "base": g.ts.BaseBranch}
	if err := g.do(ctx, http.MethodPost, "/repos/"+g.ts.Owner+"/"+g.ts.Repo+"/pulls", payload, &out); err != nil {
		return "", err
	}
	return out.HTMLURL, nil
}

// gitlabClient — REST v4. GitLab is project-path based and forks MR and
// branch APIs under /projects/<url-encoded path>.
type gitlabClient struct {
	ts    *TemplateSource
	token string
	hc    *http.Client
}

func (g *gitlabClient) apiBase() string {
	if g.ts.APIBase != "" {
		return strings.TrimSuffix(g.ts.APIBase, "/")
	}
	return "https://gitlab.com/api/v4"
}

func (g *gitlabClient) project() string {
	return url.PathEscape(g.ts.Owner + "/" + g.ts.Repo)
}

func (g *gitlabClient) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, g.apiBase()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", g.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (g *gitlabClient) branchHead(ctx context.Context, branch string) (string, error) {
	var out struct {
		Commit struct{ ID string } `json:"commit"`
	}
	p := "/projects/" + g.project() + "/repository/branches/" + url.PathEscape(branch)
	if err := g.do(ctx, http.MethodGet, p, nil, &out); err != nil {
		return "", err
	}
	return out.Commit.ID, nil
}

func (g *gitlabClient) getFile(ctx context.Context, path, ref string) (string, []byte, error) {
	var out struct {
		Content string `json:"content"`
	}
	p := "/projects/" + g.project() + "/repository/files/" + url.PathEscape(path) + "?ref=" + url.QueryEscape(ref)
	if err := g.do(ctx, http.MethodGet, p, nil, &out); err != nil {
		return "", nil, err
	}
	content, err := base64.StdEncoding.DecodeString(out.Content)
	if err != nil {
		return "", nil, fmt.Errorf("decode file content: %w", err)
	}
	return "", content, nil // GitLab commits address the file by path, no blob sha
}

func (g *gitlabClient) createBranch(ctx context.Context, name, sha string) error {
	p := "/projects/" + g.project() + "/repository/branches?branch=" + url.QueryEscape(name) + "&ref=" + url.QueryEscape(sha)
	return g.do(ctx, http.MethodPost, p, nil, nil)
}

func (g *gitlabClient) commitFile(ctx context.Context, path, branch, message string, content []byte, _ string) error {
	body := map[string]any{
		"branch":         branch,
		"commit_message": message,
		"actions": []map[string]string{{
			"action":    "update",
			"file_path": path,
			"content":   string(content),
		}},
	}
	return g.do(ctx, http.MethodPost, "/projects/"+g.project()+"/repository/commits", body, nil)
}

func (g *gitlabClient) openMR(ctx context.Context, branch, title, body string) (string, error) {
	var out struct {
		WebURL string `json:"web_url"`
	}
	payload := map[string]any{
		"source_branch":        branch,
		"target_branch":        g.ts.BaseBranch,
		"title":                title,
		"description":          body,
		"remove_source_branch": true,
	}
	if err := g.do(ctx, http.MethodPost, "/projects/"+g.project()+"/merge_requests", payload, &out); err != nil {
		return "", err
	}
	return out.WebURL, nil
}
