package ui

import (
	"context"
	"regexp"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// workflowNameRe restricts Workflow CR names to DNS-compatible identifiers.
// Prevents path traversal and ensures k8s naming compliance.
var workflowNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

const maxWorkflowNameLen = 63

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
