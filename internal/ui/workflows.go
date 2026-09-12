package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// workflowNameRe restricts Workflow CR names to DNS-compatible identifiers.
// Prevents path traversal and ensures k8s naming compliance.
var workflowNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

const maxWorkflowNameLen = 63

// handleWorkflowNew renders the creation form (GET /workflows/new — ADR-0012
// §5): a template catalog (WorkflowTemplate CRs — the reusable pipeline
// shapes) plus per-template scope fields. The form is the human view of the
// same declaration the create handler enforces: only parameters the selected
// template declares (spec.scope) are ever stored in spec.config.
func (s *Server) handleWorkflowNew(w http.ResponseWriter, r *http.Request) {
	// The form exists to create: a read-only identity gets the catalog
	// instead of a form that can only 403 on submit (the CTA is likewise
	// hidden — #436).
	if !s.mayWrite(identityFromContext(r.Context())) {
		http.Redirect(w, r, "/workflows", http.StatusSeeOther)
		return
	}
	templates, err := s.listTemplates(r)
	if err != nil {
		s.logger.Error("list templates for workflow form", "err", err)
		s.renderError(w, r, "Failed to load templates: "+err.Error())
		return
	}

	s.render(w, r, "pages/workflow_new.html", map[string]any{
		"Templates":        templates,
		"SelectedTemplate": r.URL.Query().Get("template"),
	})
}

// resolveWorkflow returns wf with its effective spec: when spec.templateRef
// names a WorkflowTemplate, the template defaults are overlaid (instance-set
// fields win; spec.config overlays prepare.config). Read paths (list
// grouping, detail, graph) render the merged shape while the stored CR stays
// thin. A missing template degrades to the thin spec (rendered as-is).
func (s *Server) resolveWorkflow(ctx context.Context, wf *v1alpha1.Workflow) v1alpha1.Workflow {
	if wf.Spec.TemplateRef == "" {
		return *wf
	}
	var tmpl v1alpha1.WorkflowTemplate
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: wf.Spec.TemplateRef}, &tmpl); err != nil {
		return *wf
	}
	merged := wf.DeepCopy()
	v1alpha1.ApplyTemplateDefaults(merged, &tmpl)
	return *merged
}

// scopeConfigJSON builds the instance's spec.config from the form, using
// ONLY the parameters the selected template declares (spec.scope). The
// template owns its configuration dialect end-to-end: the form renders from
// the same declaration, and keys the template does not declare are never
// stored — a client cannot smuggle undeclared config into the instance.
// Defaults apply when a field is left empty.
func scopeConfigJSON(r *http.Request, tmpl *v1alpha1.WorkflowTemplate) ([]byte, error) {
	cfg := map[string]any{}
	for _, p := range tmpl.Spec.Scope {
		// Inputs are name-prefixed per template (see workflow_new.html) so a
		// hidden fieldset's fields can never shadow the selected template's —
		// every fieldset renders into the form, hidden ones included.
		formName := "scope-" + tmpl.Name + "-" + p.Name
		switch p.Kind {
		case "list":
			items := []string{}
			for _, v := range strings.Split(r.FormValue(formName), ",") {
				if v = strings.TrimSpace(v); v != "" {
					items = append(items, v)
				}
			}
			if len(items) == 0 && p.Default != "" {
				for _, v := range strings.Split(p.Default, ",") {
					if v = strings.TrimSpace(v); v != "" {
						items = append(items, v)
					}
				}
			}
			cfg[p.Name] = items
		default: // "string" (and undeclared kinds degrade to string)
			v := strings.TrimSpace(r.FormValue(formName))
			if v == "" {
				v = p.Default
			}
			cfg[p.Name] = v
		}
	}
	if len(cfg) == 0 {
		// Scope-less template: store NO config key at all — an empty `{}` is
		// length-2 truthiness and reads as "the instance made scope choices",
		// which it did not (PR #427 review P2). The CR stays thin.
		return nil, nil
	}
	return json.Marshal(cfg)
}

// handleWorkflowCreate handles POST /workflows — instance creation under
// ADR-0012 §5: a thin template instance (name + templateRef + schedule + the
// template's declared scope params). Templates themselves are chart values
// (ADR-0011) and are never mutated in-cluster; template changes compose MRs
// against their git source.
//
// The write is gated on identity provenance (mayWrite) and the owner label
// is stamped via StampOwnerLabel from the session identity — never from
// client input — so every created workflow is visible to its creator by
// construction.
func (s *Server) handleWorkflowCreate(w http.ResponseWriter, r *http.Request) {
	const action = "create"
	// One funnel for every rejection/success — harmostes_ui_writes_total
	// (#436): rejections stopped being log-only when the lifecycle verbs
	// re-armed (#418).
	fail := func(code int, msg string) {
		recordWrite(action, "error")
		s.renderErrorStatus(w, r, code, msg)
	}
	id := identityFromContext(r.Context())
	if !s.mayWrite(id) {
		if id == nil {
			s.logger.Warn("write rejected — no identity in context")
		} else {
			s.logger.Warn("write rejected — identity provenance", "user", id.Username, "dev", id.Dev)
		}
		// Generic body: no signal about WHICH provenance check failed or what
		// the server's dev-write configuration is (PR #427 review, round 4).
		recordWrite(action, "forbidden")
		http.Error(w, "403 Forbidden — this identity may not take write actions", http.StatusForbidden)
		return
	}
	owner := id.Username
	if id.Dev {
		// Reserved namespace for dev-stamped owners (PR #427 review P6): a dev
		// identity is structurally unable to occupy a real user's owner label —
		// worst case it squats a dev-* label only admins see. The full trust
		// chain for production: the outpost strips client identity headers,
		// devWrite is never rendered under default values (golden-guarded), and
		// this prefix keeps even an enabled dev server off real owner labels.
		owner = DevOwnerPrefix + owner
	}

	// Same-origin guard (CSRF): the write is cookie-authenticated and
	// urlencoded, so a cross-site form post is the classic forgery vector.
	// Browsers send Origin on cross-site POSTs — when present, its host must
	// be ours. (The outpost's SameSite cookie is the first line; this is the
	// in-repo second.)
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			s.logger.Warn("write rejected — cross-origin", "origin", origin, "host", r.Host)
			recordWrite(action, "forbidden")
			http.Error(w, "403 Forbidden — cross-origin write", http.StatusForbidden)
			return
		}
	}
	if err := r.ParseForm(); err != nil {
		fail(http.StatusBadRequest, "Invalid form data")
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))

	// Validation (status-coded: 400 client mistake, 409 conflict, 500 fault)
	if name == "" {
		fail(http.StatusBadRequest, "Workflow name is required")
		return
	}
	if !workflowNameRe.MatchString(name) || len(name) > maxWorkflowNameLen {
		fail(http.StatusBadRequest, "Invalid workflow name: must be lowercase, alphanumeric with hyphens, max 63 characters")
		return
	}

	templateRef := strings.TrimSpace(r.FormValue("templateRef"))
	if templateRef == "" {
		fail(http.StatusBadRequest, "A template must be selected — workflows are template instances")
		return
	}

	var tmpl v1alpha1.WorkflowTemplate
	if err := s.k8sClient.Get(r.Context(), client.ObjectKey{Namespace: s.namespace, Name: templateRef}, &tmpl); err != nil {
		fail(http.StatusBadRequest, "Unknown template: "+templateRef)
		return
	}

	// Cadence: the two source kinds the self-service surface supports
	// (ADR-0012 §7). "schedule" runs on the platform poll interval;
	// "webhook" waits for a push wake. Anything else (git/event, cron
	// strings) is an operator-level dialect the UI form must not pretend to
	// offer — the controller never parses a cron string (PR #427 review).
	sourceKind := r.FormValue("sourceKind")
	if sourceKind == "" {
		sourceKind = "schedule" // the form's checked default — absent on old posts too
	}
	if sourceKind != "schedule" && sourceKind != "webhook" {
		fail(http.StatusBadRequest, "Invalid source kind: "+sourceKind+" (schedule or webhook)")
		return
	}

	cfg, err := scopeConfigJSON(r, &tmpl)
	if err != nil {
		fail(http.StatusBadRequest, "Failed to build config: "+err.Error())
		return
	}

	// Source.Kind "schedule" marks the instance as poll-triggered (the
	// claim sweep treats non-webhook kinds as non-wake) — but it is also the
	// kind the controller DUES every PollInterval: creation must not arm an
	// unattended agent loop nobody asked to run. So every UI-created instance
	// starts DISABLED: it is inert until armed through the run controls on
	// its detail page (#418 lifecycle routes — enable, then trigger or let
	// the poll cadence take over). Creation is composition; arming is a
	// separate, deliberate act.
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.namespace,
		},
		Spec: v1alpha1.WorkflowSpec{
			TemplateRef: templateRef,
			Source:      v1alpha1.SourceSpec{Kind: sourceKind},
			Config:      cfg,
			Disabled:    true,
		},
	}
	if err := v1alpha1.StampOwnerLabel(wf, owner); err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	if err := s.k8sClient.Create(r.Context(), wf); err != nil {
		if errors.IsAlreadyExists(err) {
			fail(http.StatusConflict, "A workflow with that name already exists")
			return
		}
		s.logger.Error("create workflow", "owner", owner, "name", name, "err", err)
		fail(http.StatusInternalServerError, "Failed to create workflow")
		return
	}
	// Per the form contract (same reason no CSRF token beyond the origin
	// check): the form posts same-site, urlencoded, and the owner is
	// server-stamped — a forgery can at worst create a DISABLED workflow
	// under the victim's own identity, visible to them. Removal is one POST
	// away on the detail page (#418 lifecycle routes).
	s.logger.Info("workflow created (template instance, disabled)", "owner", owner, "name", name, "template", templateRef, "source", sourceKind)
	recordWrite(action, "created")
	http.Redirect(w, r, "/workflows/"+name, http.StatusSeeOther)
}
