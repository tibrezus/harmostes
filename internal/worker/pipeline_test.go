package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tibrezus/harmostes/internal/agent"
	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// --- fakes ---

type fakeResolver struct{ paths map[string]string }

func (r fakeResolver) Resolve(_ context.Context, ref v1alpha1.PluginRef, _ string) (string, []string, error) {
	if p, ok := r.paths[ref.Name]; ok {
		return p, ref.Args, nil
	}
	return "", nil, &os.PathError{Op: "resolve", Path: ref.Name, Err: os.ErrNotExist}
}

type fakeTasks struct{ task string }

func (f fakeTasks) Get(_ context.Context, _ v1alpha1.TaskTemplate) (string, error) { return f.task, nil }

type fakeAgent struct{ green bool; attempts int }

func (f fakeAgent) Run(_ context.Context, _ string, _ agent.Gate, _ int, _ agent.Logger) (agent.Result, error) {
	return agent.Result{Green: f.green, Attempts: f.attempts}, nil
}

type fakeDapr struct{ published []string }

func (f *fakeDapr) GetState(_ context.Context, _, _ string) (string, error) { return "", nil }
func (f *fakeDapr) SaveState(_ context.Context, _, _, _ string) error       { return nil }
func (f *fakeDapr) DeleteState(_ context.Context, _, _ string) error        { return nil }
func (f *fakeDapr) Publish(_ context.Context, _, topic, _ string) error {
	f.published = append(f.published, topic)
	return nil
}

type fakeStatus struct{ last v1alpha1.WorkflowStatus }

func (f *fakeStatus) PatchStatus(_ context.Context, _ string, mutate func(*v1alpha1.WorkflowStatus)) error {
	mutate(&f.last)
	return nil
}

// writeScript writes an executable shell script returning a given JSON line.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "plugin.sh")
	if err := os.WriteFile(p, []byte("#!/usr/bin/env sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func newWorkflow() *v1alpha1.Workflow {
	return &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "ns"},
		Spec: v1alpha1.WorkflowSpec{
			Prepare: v1alpha1.PrepareSpec{Plugin: v1alpha1.PluginRef{Name: "prepare"}},
			Agent: v1alpha1.AgentSpec{
				TaskTemplate: v1alpha1.TaskTemplate{Name: "t"},
				Gate:         v1alpha1.GateRef{Plugin: v1alpha1.PluginRef{Name: "gate"}},
			},
			Deploy:  v1alpha1.DeploySpec{Plugin: v1alpha1.PluginRef{Name: "deploy"}},
			Events:  &v1alpha1.EventsSpec{OnPrepare: "p", OnResolved: "r", OnFailed: "f"},
		},
	}
}

func TestPipelineGreen(t *testing.T) {
	resolver := fakeResolver{paths: map[string]string{
		"prepare": writeScript(t, `echo '{"changed":true,"artifact":"rig.json"}'`),
		"gate":    writeScript(t, `exit 0`),
		"deploy":  writeScript(t, `echo '{"artifact":"pushed","event":{"commit":"abc123"}}'`),
	}}
	d := &fakeDapr{}
	st := &fakeStatus{}
	deps := Deps{
		Plugins: resolver, Tasks: fakeTasks{task: "do it"},
		Dapr: d, Status: st, Agent: fakeAgent{green: true, attempts: 1},
		Log: func(format string, a ...any) { t.Logf("pipeline: "+format, a...) },
	}
	res, err := Run(context.Background(), deps, Options{Workflow: newWorkflow(), Workdir: t.TempDir(), Source: "rev1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeGreen {
		t.Fatalf("expected green, got %s (%s)", res.Outcome, res.Message)
	}
	if st.last.GateStatus != "green" || st.last.Message != "deployed" {
		t.Fatalf("status = %+v", st.last)
	}
	if st.last.LastAgentCommit != "abc123" {
		t.Fatalf("expected commit abc123 from deploy event, got %q", st.last.LastAgentCommit)
	}
	if st.last.LastProcessedRevision != "rev1" {
		t.Fatalf("expected revision rev1, got %q", st.last.LastProcessedRevision)
	}
	// events: prepare published, resolved published (no failed)
	want := map[string]bool{"p": true, "r": true}
	for _, topic := range d.published {
		delete(want, topic)
	}
	if len(want) != 0 {
		t.Fatalf("missing publishes: %v (got %v)", want, d.published)
	}
}

func TestPipelineDeterministicSkip(t *testing.T) {
	resolver := fakeResolver{paths: map[string]string{
		"prepare": writeScript(t, `echo '{"changed":false}'`),
	}}
	d := &fakeDapr{}
	st := &fakeStatus{}
	agentCalled := false
	deps := Deps{
		Plugins: resolver, Tasks: fakeTasks{task: "x"},
		Dapr: d, Status: st,
		Agent: fakeAgentRunnerFunc(func() (bool, int) { agentCalled = true; return true, 1 }),
	}
	res, err := Run(context.Background(), deps, Options{Workflow: newWorkflow(), Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeSkipped {
		t.Fatalf("expected skipped, got %s", res.Outcome)
	}
	if agentCalled {
		t.Fatal("agent must NOT run when prepare reports changed=false")
	}
	if st.last.GateStatus != "green" || st.last.Message != "no change (deterministic skip)" {
		t.Fatalf("status = %+v", st.last)
	}
}

func TestPipelineAgentFailed(t *testing.T) {
	resolver := fakeResolver{paths: map[string]string{
		"prepare": writeScript(t, `echo '{"changed":true}'`),
		"gate":    writeScript(t, `exit 0`),
		// deploy is intentionally OMITTED: the pipeline must not reach it on agent failure.
	}}
	d := &fakeDapr{}
	st := &fakeStatus{}
	deps := Deps{
		Plugins: resolver, Tasks: fakeTasks{task: "x"},
		Dapr: d, Status: st, Agent: fakeAgent{green: false, attempts: 4},
	}
	res, err := Run(context.Background(), deps, Options{Workflow: newWorkflow(), Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeFailed {
		t.Fatalf("expected failed, got %s", res.Outcome)
	}
	if st.last.GateStatus != "failed" {
		t.Fatalf("status gate = %q, want failed", st.last.GateStatus)
	}
	// onFailed published; onResolved NOT
	for _, topic := range d.published {
		if topic == "r" {
			t.Fatal("onResolved must not publish on agent failure")
		}
	}
}

// fakeAgentRunnerFunc adapts a closure into an AgentRunner (for the skip test's
// "agent must not be called" assertion).
type fakeAgentRunnerFunc func() (green bool, attempts int)

func (f fakeAgentRunnerFunc) Run(_ context.Context, _ string, _ agent.Gate, _ int, _ agent.Logger) (agent.Result, error) {
	g, a := f()
	return agent.Result{Green: g, Attempts: a}, nil
}
