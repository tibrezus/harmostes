package graph

import (
	"encoding/json"
	"fmt"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// PluginNodeConfig is the config for a "plugin" node. It maps to the existing
// v1alpha1.PluginRef that the worker's PluginResolver understands.
type PluginNodeConfig struct {
	Name      string   `json:"name"`
	Args      []string `json:"args,omitempty"`
	ConfigMap string   `json:"configMap,omitempty"`
}

// ToPluginRef converts the graph-node config to the existing CRD PluginRef.
func (c PluginNodeConfig) ToPluginRef() v1alpha1.PluginRef {
	return v1alpha1.PluginRef{
		Name:      c.Name,
		Args:      c.Args,
		ConfigMap: c.ConfigMap,
	}
}

// GateNodeConfig is the config for a "gate" node. The gate wraps a plugin:
// exit 0 = green, stderr = feedback.
type GateNodeConfig struct {
	Plugin PluginNodeConfig `json:"plugin"`
}

// AgentNodeConfig is the config for an "agent" node (non-deterministic).
// The gate is optional: if absent, the agent runs a single prompt with no
// validation loop.
type AgentNodeConfig struct {
	Model    string          `json:"model"`
	Skill    string          `json:"skill"`
	Task     string          `json:"task"` // inline task text OR a TaskResolver ref
	Tools    []string        `json:"tools,omitempty"`
	Gate     *GateNodeConfig `json:"gate,omitempty"`
	MaxFixes int             `json:"maxFixes,omitempty"`
}

// BranchNodeConfig is the config for a "branch" node. The condition is a Go
// text/template expression evaluated against the node inputs. If it renders to
// "true" (case-insensitive), the branch outputs changed=true.
type BranchNodeConfig struct {
	Condition string `json:"condition"`
}

// VelaAppConfig configures a "vela-app" node — create/update/delete/wait a
// KubeVela Application CR. The application field is the raw spec (components,
// traits, policies) without metadata.
type VelaAppConfig struct {
	Action      string         `json:"action"`      // apply | delete | wait
	Name        string         `json:"name"`        // Application name
	Namespace   string         `json:"namespace"`   // target namespace (default: env.Namespace)
	Application map[string]any `json:"application"` // raw KubeVela Application spec
	Timeout     string         `json:"timeout"`     // for wait action (e.g. "600s")
}

// FluxReconcileConfig configures a "flux-reconcile" node — triggers Flux
// reconciliation of a resource by annotating it with fluxcd.io/reconcileAt,
// then optionally polls until Ready=True.
type FluxReconcileConfig struct {
	Resource  string `json:"resource"`  // kind/name (e.g. "helmrelease/my-service")
	Namespace string `json:"namespace"` // target namespace (default: env.Namespace)
	Wait      bool   `json:"wait"`      // poll until Ready
	Timeout   string `json:"timeout"`   // e.g. "600s" (default: "300s")
}

// parseConfig unmarshals a node's raw JSON config into the target type.
func parseConfig[T any](raw json.RawMessage) (T, error) {
	var cfg T
	if len(raw) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}
