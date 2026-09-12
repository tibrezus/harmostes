package ui

import (
	"encoding/json"
	"fmt"
	"net/http"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"sigs.k8s.io/yaml"
)

// Node inspector (#419, ADR-0012 §2/§7 — Windmill step-settings pattern).
//
// The inspector edits a template document through STRUCTURED, typed edits —
// never text diffs on YAML. The document lives in the Workflow Code island
// (the browser holds the working copy); this side owns the transform: a
// whitelisted set of typed paths ("the supported param space") applied to
// the typed spec, re-marshaled deterministically. YAML → form-model → YAML
// is lossless by construction (same Go type both ways) and pinned by test.
//
// Persistence is deliberately NOT here: the head template is chart/git-owned
// (ADR-0011) and the MR-bridge (#420) will carry the edited document to its
// git source. This endpoint is a pure document transform — no cluster
// access, no state.

// inspectField is one editable path in the template document. The set of
// fields IS the supported param space: a path not listed here cannot be
// written by the inspector, whatever a client posts. Types mirror the CRD
// (lockstep-pinned by TestInspectFieldsMatchChartCRD).
type inspectField struct {
	Path  string // dot path under spec, e.g. "agent.model"
	Label string
	Kind  string // "string" | "bool" | "int"
	Node  string // which topology node's inspector panel shows it: template|prepare|agent|deploy
}

// inspectFields is the supported param space, grouped by node in topology
// order. agent.enabled uses the field's EFFECTIVE value in forms (nil reads
// as enabled — the compile default); editing always writes an explicit bool.
var inspectFields = []inspectField{
	{"description", "Description", "string", "template"},
	{"prepare.plugin.name", "Prepare plugin", "string", "prepare"},
	{"agent.enabled", "Agent enabled", "bool", "agent"},
	{"agent.model", "Model", "string", "agent"},
	{"agent.skill", "Skill", "string", "agent"},
	{"agent.gate.plugin.name", "Gate plugin", "string", "agent"},
	{"agent.maxFixes", "Max fix attempts", "int", "agent"},
	{"deploy.plugin.name", "Deploy plugin", "string", "deploy"},
}

// inspectEdit is one structured change: a whitelisted path and a typed
// value. Empty string on a string path clears the field (omitempty drops it
// from the document — "unset" is a first-class state).
type inspectEdit struct {
	Path  string `json:"path"`
	Value any    `json:"value"`
}

// inspectRequest is the POST /api/inspect body: the caller's current
// document plus the edits to apply in order.
type inspectRequest struct {
	Document string        `json:"document"`
	Edits    []inspectEdit `json:"edits"`
}

// applyTemplateEdits parses the document into the SAME typed shape
// templateYAML marshals (round-trip by construction), applies the edits,
// and re-marshals. Every rejection is a typed error message (the inspector
// surfaces it verbatim); the document is never partially applied — parse
// and type errors abort before the first mutation lands in output.
func applyTemplateEdits(doc string, edits []inspectEdit) (string, error) {
	var d templateDocument
	if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
		return "", fmt.Errorf("document is not a valid template: %w", err)
	}
	if d.Kind != "" && d.Kind != "WorkflowTemplate" {
		return "", fmt.Errorf("document kind %q is not a WorkflowTemplate", d.Kind)
	}
	for i, e := range edits {
		if err := applyOne(&d.Spec, e); err != nil {
			return "", fmt.Errorf("edit %d (%s): %w", i+1, e.Path, err)
		}
	}
	out, err := yaml.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("marshal document: %w", err)
	}
	return string(out), nil
}

// applyOne applies a single edit to the typed spec. The switch is the
// whitelist: an unknown path is an error, not a silent no-op — a typo'd
// path must never look like a successful edit.
func applyOne(spec *v1alpha1.WorkflowTemplateSpec, e inspectEdit) error {
	switch e.Path {
	case "description":
		v, err := stringOf(e)
		if err != nil {
			return err
		}
		spec.Description = v
		return nil
	case "prepare.plugin.name":
		v, err := stringOf(e)
		if err != nil {
			return err
		}
		spec.Prepare.Plugin.Name = v
		return nil
	case "agent.enabled":
		v, err := boolOf(e)
		if err != nil {
			return err
		}
		spec.Agent.Enabled = &v
		return nil
	case "agent.model":
		v, err := stringOf(e)
		if err != nil {
			return err
		}
		spec.Agent.Model = v
		return nil
	case "agent.skill":
		v, err := stringOf(e)
		if err != nil {
			return err
		}
		spec.Agent.Skill = v
		return nil
	case "agent.gate.plugin.name":
		v, err := stringOf(e)
		if err != nil {
			return err
		}
		spec.Agent.Gate.Plugin.Name = v
		return nil
	case "agent.maxFixes":
		v, err := intOf(e)
		if err != nil {
			return err
		}
		spec.Agent.MaxFixes = v
		return nil
	case "deploy.plugin.name":
		v, err := stringOf(e)
		if err != nil {
			return err
		}
		spec.Deploy.Plugin.Name = v
		return nil
	default:
		return fmt.Errorf("unsupported path %q — the inspector edits only the declared param space", e.Path)
	}
}

// stringOf coerces the edit value to string: JSON strings only. Empty means
// clear.
func stringOf(e inspectEdit) (string, error) {
	v, ok := e.Value.(string)
	if !ok {
		return "", fmt.Errorf("expects a string value, got %T", e.Value)
	}
	return v, nil
}

func boolOf(e inspectEdit) (bool, error) {
	v, ok := e.Value.(bool)
	if !ok {
		return false, fmt.Errorf("expects a boolean value, got %T", e.Value)
	}
	return v, nil
}

func intOf(e inspectEdit) (int, error) {
	f, ok := e.Value.(float64) // encoding/json decodes numbers as float64
	if !ok || f != float64(int(f)) {
		return 0, fmt.Errorf("expects an integer value, got %v", e.Value)
	}
	return int(f), nil
}

// handleInspectAPI is POST /api/inspect: the pure document transform behind
// the inspector's Apply. Inside auth (a console API), stateless by design —
// the working document lives in the editor; persistence is #420's bridge.
func (s *Server) handleInspectAPI(w http.ResponseWriter, r *http.Request) {
	var req inspectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Edits) == 0 {
		http.Error(w, "no edits — nothing to apply", http.StatusBadRequest)
		return
	}
	out, err := applyTemplateEdits(req.Document, req.Edits)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"document": out})
}
