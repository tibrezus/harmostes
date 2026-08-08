// Package v1alpha1 — WorkflowTemplate CRD.
//
// A WorkflowTemplate carries the common structure (prepare → agent+gate →
// deploy defaults) that Workflows can inherit via spec.templateRef. Templates
// are declared as YAML (GitOps) — the UI discovers them from the cluster and
// renders an auto-generated graph showing what each template does.
//
// This is the Kubernetes-native replacement for the hardcoded gateCatalog:
// adding a new workflow archetype is a YAML change, not a code change.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=wftemplate
// +kubebuilder:printcolumn:name=Description,type=string,JSONPath=.spec.description
// +kubebuilder:printcolumn:name=Prepare,type=string,JSONPath=.spec.prepare.plugin.name
// +kubebuilder:printcolumn:name=Gate,type=string,JSONPath=.spec.agent.gate.plugin.name
// +kubebuilder:printcolumn:name=Deploy,type=string,JSONPath=.spec.deploy.plugin.name
// +kubebuilder:printcolumn:name=Age,type=date,JSONPath=.metadata.creationTimestamp

// WorkflowTemplate defines the common structure for a class of Workflows.
type WorkflowTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec WorkflowTemplateSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// WorkflowTemplateList is a list of WorkflowTemplates.
type WorkflowTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkflowTemplate `json:"items"`
}

// WorkflowTemplateSpec defines the default prepare/agent/gate/deploy structure
// that Workflows using this template inherit.
type WorkflowTemplateSpec struct {
	// Description is a human-readable explanation of what this template does.
	Description string `json:"description"`

	// Prepare defines the prepare plugin that produces the working artifact.
	Prepare PrepareSpec `json:"prepare"`

	// Agent defines the agent configuration (skill, gate, maxFixes).
	Agent AgentSpec `json:"agent"`

	// Deploy defines the deploy plugin that ships the result.
	Deploy DeploySpec `json:"deploy"`
}

// ---------------------------------------------------------------------------
// runtime.Object + DeepCopy
// ---------------------------------------------------------------------------

func (in *WorkflowTemplate) DeepCopyInto(out *WorkflowTemplate) { deepCopy(in, out) }
func (in *WorkflowTemplate) DeepCopy() *WorkflowTemplate {
	if in == nil {
		return nil
	}
	out := new(WorkflowTemplate)
	deepCopy(in, out)
	return out
}
func (in *WorkflowTemplate) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *WorkflowTemplateList) DeepCopyInto(out *WorkflowTemplateList) { deepCopy(in, out) }
func (in *WorkflowTemplateList) DeepCopy() *WorkflowTemplateList {
	if in == nil {
		return nil
	}
	out := new(WorkflowTemplateList)
	deepCopy(in, out)
	return out
}
func (in *WorkflowTemplateList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// Compile-time assertions.
var (
	_ runtime.Object = (*WorkflowTemplate)(nil)
	_ runtime.Object = (*WorkflowTemplateList)(nil)
)

// WorkflowTemplateResource returns the GroupResource for WorkflowTemplate.
func WorkflowTemplateResource() schema.GroupResource {
	return SchemeGroupVersion.WithResource("workflowtemplates").GroupResource()
}
