// Package worker runs a Workflow's pipeline: prepare → agent → deploy, emitting
// Dapr events at each boundary and reconciling the Workflow status.
//
// The deterministic phases (prepare, deploy, gate) invoke PLUGINS — scripts or
// images — under a fixed contract (see docs/plugin-interface.md). The agent
// phase is framework-native: it runs internal/agent (the Go harmostes primitive)
// with the Workflow's spec.agent.
package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/tibrezus/harmostes/internal/agent"
	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// PluginResult is the JSON a plugin emits as the LAST line of stdout.
type PluginResult struct {
	Artifact string         `json:"artifact,omitempty"`
	Changed  *bool          `json:"changed,omitempty"`
	Event    map[string]any `json:"event,omitempty"`
	Status   string         `json:"status,omitempty"`
}

// PluginEnv is the contract environment every plugin receives.
type PluginEnv struct {
	Workflow  string
	Namespace string
	Phase     string // prepare | gate | deploy
	Spec      string // JSON of this phase's config from the CR
	Source    string // resolved source (artifact path / ref / revision)
	Workdir   string // shared working directory
	State     string // Dapr state key prefix for this workflow
}

// EnvSlice renders PluginEnv as KEY=VALUE strings for exec.Cmd.Env.
func (e PluginEnv) EnvSlice(extra ...string) []string {
	return append([]string{
		"HARMOSTES_WORKFLOW=" + e.Workflow,
		"HARMOSTES_NAMESPACE=" + e.Namespace,
		"HARMOSTES_PHASE=" + e.Phase,
		"HARMOSTES_SPEC=" + e.Spec,
		"HARMOSTES_SOURCE=" + e.Source,
		"HARMOSTES_WORKDIR=" + e.Workdir,
		"HARMOSTES_STATE=" + e.State,
	}, extra...)
}

// RunPlugin executes a plugin command under the contract env. Returns the parsed
// result (last JSON stdout line), the combined stdout+stderr (the feedback), and
// the exec error (non-nil iff the command exited non-zero). The caller decides
// what a non-zero exit means: for a gate it is "not green"; for prepare/deploy it
// is a failure to report.
func RunPlugin(ctx context.Context, command string, args []string, env PluginEnv, extraEnv []string) (PluginResult, string, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = env.Workdir
	cmd.Env = env.EnvSlice(extraEnv...)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()
	return lastJSONLine(out.String()), out.String(), runErr
}

// GatePlugin adapts a gate plugin (exit 0 = green) to agent.Gate. Its combined
// stdout+stderr becomes the feedback the agent receives on failure.
type GatePlugin struct {
	Command  string
	Args     []string
	Env      PluginEnv
	ExtraEnv []string
}

func (g GatePlugin) Run(ctx context.Context) (bool, string, error) {
	cmd := exec.CommandContext(ctx, g.Command, g.Args...)
	cmd.Dir = g.Env.Workdir
	cmd.Env = g.Env.EnvSlice(g.ExtraEnv...)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return err == nil, strings.TrimSpace(out.String()), nil
}

// PluginResolver maps a v1alpha1.PluginRef to an executable command + args.
type PluginResolver interface {
	Resolve(ctx context.Context, ref v1alpha1.PluginRef, phase string) (command string, args []string, err error)
}

// BuiltinResolver serves plugins from a name→path map (built-ins in the worker
// image). Falls through to a ConfigMap mount root for ConfigMap plugins.
type BuiltinResolver struct {
	Builtins      map[string]string // plugin name → executable path
	ConfigMapRoot string            // where ConfigMap-plugin scripts are mounted (e.g. /plugins)
}

func (r BuiltinResolver) Resolve(_ context.Context, ref v1alpha1.PluginRef, _ string) (string, []string, error) {
	if ref.Image != "" {
		return "", nil, fmt.Errorf("image plugins not yet supported (ref %q); use a built-in or ConfigMap plugin", ref.Name)
	}
	if path, ok := r.Builtins[ref.Name]; ok {
		return path, ref.Args, nil
	}
	if ref.ConfigMap != "" && r.ConfigMapRoot != "" {
		return r.ConfigMapRoot + "/" + ref.ConfigMap + "/" + ref.Name + ".sh", ref.Args, nil
	}
	return "", nil, fmt.Errorf("plugin %q not found (no builtin, no configmap root)", ref.Name)
}

// lastJSONLine parses the JSON object from the last non-blank line of output, or
// returns a zero PluginResult if none parses.
func lastJSONLine(combined string) PluginResult {
	sc := bufio.NewScanner(strings.NewReader(combined))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var last string
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			last = line
		}
	}
	if last == "" {
		return PluginResult{}
	}
	var res PluginResult
	if json.Unmarshal([]byte(last), &res) == nil {
		return res
	}
	return PluginResult{}
}

// AgentRunner runs the agent loop (the harmostes primitive). Real impl wraps
// agent.NewRPC + agent.Task over one warm pi session; tests inject a fake.
type AgentRunner interface {
	Run(ctx context.Context, task string, gate agent.Gate, maxFixes int, log agent.Logger) (agent.Result, error)
}

// RPCAgentRunner is the production AgentRunner.
type RPCAgentRunner struct {
	Opts agent.RPCOptions
}

func (r RPCAgentRunner) Run(ctx context.Context, task string, gate agent.Gate, maxFixes int, log agent.Logger) (agent.Result, error) {
	rpc, err := agent.NewRPC(ctx, r.Opts)
	if err != nil {
		return agent.Result{}, err
	}
	defer rpc.Abort(ctx)
	return agent.Task(ctx, rpc, gate, task, maxFixes, log)
}

// PiArgs builds the pi --mode rpc extra args from a Workflow's agent spec.
func PiArgs(a v1alpha1.AgentSpec) []string {
	args := []string{"--skill", a.Skill, "--model", a.Model}
	if len(a.Tools) > 0 {
		args = append(args, "--tools", strings.Join(a.Tools, ","))
	}
	return args
}

// TaskResolver yields the agent's task text from its TaskTemplate (e.g. a
// ConfigMap key).
type TaskResolver interface {
	Get(ctx context.Context, tt v1alpha1.TaskTemplate) (string, error)
}

// logBridge adapts a worker-level printf into an agent.Logger.
func logBridge(logf func(format string, args ...any)) agent.Logger {
	if logf == nil {
		return nil
	}
	return func(ev agent.Event) {
		logf("agent: %s %s", ev.Type, ev.ToolName)
	}
}
