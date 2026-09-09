package v1alpha1

import (
	"encoding/json"
	"testing"
	"time"
)

func tmplSpec() WorkflowTemplateSpec {
	return WorkflowTemplateSpec{
		Description: "PR review (fetch PR → agent → pr-review → post review)",
		Prepare: PrepareSpec{
			Plugin: PluginRef{Name: "pr-fetch", ConfigMap: "harmostes-pr-review"},
			Detect: "changed",
			Config: json.RawMessage(`{"repos":["example/default"],"label":"needs-review"}`),
		},
		Agent: AgentSpec{
			Model:        "litellm/zai/glm-4.7",
			Skill:        "/skills/pr-review/SKILL.md",
			Tools:        []string{"read", "bash", "grep"},
			TaskTemplate: TaskTemplate{Name: "pr-review", ConfigMap: "harmostes-tasks", Key: "pr-review.txt"},
			Gate:         GateRef{Plugin: PluginRef{Name: "pr-review", ConfigMap: "harmostes-pr-review"}},
			MaxFixes:     1,
		},
		Deploy: DeploySpec{Plugin: PluginRef{Name: "post-review", ConfigMap: "harmostes-pr-review"}},
	}
}

func TestApplyTemplateDefaultsFullInheritance(t *testing.T) {
	wf := &Workflow{Spec: WorkflowSpec{TemplateRef: "pr-review"}}
	ApplyTemplateDefaults(wf, &WorkflowTemplate{Spec: tmplSpec()})

	s := wf.Spec
	if s.Prepare.Plugin.Name != "pr-fetch" || s.Prepare.Plugin.ConfigMap != "harmostes-pr-review" {
		t.Fatalf("prepare plugin not inherited: %+v", s.Prepare.Plugin)
	}
	if s.Prepare.Detect != "changed" {
		t.Fatalf("detect not inherited: %q", s.Prepare.Detect)
	}
	if s.Agent.Model != "litellm/zai/glm-4.7" || s.Agent.Skill != "/skills/pr-review/SKILL.md" {
		t.Fatalf("agent model/skill not inherited: %+v", s.Agent)
	}
	if s.Agent.TaskTemplate.Key != "pr-review.txt" {
		t.Fatalf("task template not inherited: %+v", s.Agent.TaskTemplate)
	}
	if s.Agent.Gate.Plugin.Name != "pr-review" {
		t.Fatalf("gate not inherited: %+v", s.Agent.Gate)
	}
	if s.Agent.MaxFixes != 1 {
		t.Fatalf("maxFixes not inherited: %d", s.Agent.MaxFixes)
	}
	if s.Deploy.Plugin.Name != "post-review" {
		t.Fatalf("deploy plugin not inherited: %+v", s.Deploy.Plugin)
	}
	// Template default config inherited when the instance sets none.
	if string(s.Prepare.Config) == "" || s.Prepare.Config == nil {
		t.Fatal("prepare config not inherited")
	}
}

func TestApplyTemplateDefaultsInstanceWins(t *testing.T) {
	wf := &Workflow{Spec: WorkflowSpec{
		TemplateRef: "pr-review",
		Prepare:     PrepareSpec{Config: json.RawMessage(`{"repos":["tibrezus/harmostes"],"label":"needs-review"}`)},
		Agent:       AgentSpec{Model: "litellm/zai/glm-4.8"},
		Config:      json.RawMessage(`{"repos":["tibrezus/harmostes"],"label":"needs-review","wiki":""}`),
	}}
	ApplyTemplateDefaults(wf, &WorkflowTemplate{Spec: tmplSpec()})

	if wf.Spec.Agent.Model != "litellm/zai/glm-4.8" {
		t.Fatalf("instance model override lost: %q", wf.Spec.Agent.Model)
	}
	// spec.config overlays prepare.config after the template merge.
	var cfg map[string]any
	if err := json.Unmarshal(wf.Spec.Prepare.Config, &cfg); err != nil {
		t.Fatalf("prepare config not JSON after overlay: %v", err)
	}
	if repos, _ := cfg["repos"].([]any); len(repos) != 1 || repos[0] != "tibrezus/harmostes" {
		t.Fatalf("spec.config did not overlay prepare.config: %s", wf.Spec.Prepare.Config)
	}
	// Untouched fields still inherit.
	if wf.Spec.Agent.Skill != "/skills/pr-review/SKILL.md" {
		t.Fatalf("skill inheritance broken by override: %q", wf.Spec.Agent.Skill)
	}
	if wf.Spec.Deploy.Plugin.Name != "post-review" {
		t.Fatalf("deploy inheritance broken by override: %+v", wf.Spec.Deploy.Plugin)
	}
}

func TestApplyTemplateDefaultsNilSafety(t *testing.T) {
	ApplyTemplateDefaults(nil, nil) // must not panic
	wf := &Workflow{Spec: WorkflowSpec{}}
	ApplyTemplateDefaults(wf, nil)
	if wf.Spec.Agent.Model != "" {
		t.Fatal("nil template must leave spec untouched")
	}
}

// The exactly-once floor (#248 review finding 2): a configured
// dispatchTimeout at or below the one-shot run bound must degrade to the
// default — honoring it would re-dispatch while a run may still be alive.
func TestReviewReadyDispatchTimeoutFloor(t *testing.T) {
	def := OneShotRunBound + 15*time.Minute
	cases := []struct {
		cfg  string
		want time.Duration
	}{
		{"", def},                       // unset → default
		{"garbage", def},                // unparsable → default
		{"0s", def},                     // non-positive → default
		{"10m", def},                    // below the run bound → default
		{OneShotRunBound.String(), def}, // AT the run bound (zero margin) → default
		{(OneShotRunBound + 4*time.Minute).String(), def}, // 4m margin < MinDispatchMargin → default
		{"31m", def}, // the #255 case: 1m margin reads as run+1m → default
		{(OneShotRunBound + MinDispatchMargin).String(), OneShotRunBound + MinDispatchMargin}, // exactly the floor (5m margin) → honored
		{(OneShotRunBound + 6*time.Minute).String(), OneShotRunBound + 6*time.Minute},         // above the floor → honored
		{"2h", 2 * time.Hour}, // custom above the floor → honored
	}
	for _, c := range cases {
		got := (&ReviewReadySpec{DispatchTimeout: c.cfg}).DispatchTimeoutDuration()
		if got != c.want {
			t.Errorf("DispatchTimeoutDuration(%q) = %s, want %s", c.cfg, got, c.want)
		}
	}
	// Nil spec and zero-value spec both take the default.
	if got := (*ReviewReadySpec)(nil).DispatchTimeoutDuration(); got != def {
		t.Errorf("nil spec = %s, want %s", got, def)
	}
}

// The per-workflow wall clock (#348/#333): RunBound parsing + the enforced
// margin interplay — DispatchTimeout must leave MinDispatchMargin over the
// EFFECTIVE bound, not just the fleet default, or the gate would
// re-dispatch while a raised-wall run may still be alive.
func TestReviewReadyRunBound(t *testing.T) {
	cases := []struct {
		cfg  string
		want time.Duration
	}{
		{"", OneShotRunBound},                                   // unset → fleet default
		{"garbage", OneShotRunBound},                            // unparsable → default
		{"0s", OneShotRunBound},                                 // non-positive → default
		{"-5m", OneShotRunBound},                                // negative → default
		{"31m", 31 * time.Minute},                               // above default → honored
		{"45m", 45 * time.Minute},                               // the #333 pr-review recommendation
		{MaxRunBound.String(), MaxRunBound},                     // at the cap → honored
		{(MaxRunBound + time.Minute).String(), OneShotRunBound}, // above cap → default
	}
	for _, c := range cases {
		got := (&ReviewReadySpec{RunBound: c.cfg}).RunBoundDuration()
		if got != c.want {
			t.Errorf("RunBoundDuration(%q) = %s, want %s", c.cfg, got, c.want)
		}
	}

	// The margin invariant over the EFFECTIVE bound: runBound=45m raises
	// the wall, so dispatchTimeout=45m (fine vs the old 30m default) is
	// now INSIDE the margin → degrades to bound+15m=60m. 49m leaves a 4m
	// margin (< MinDispatchMargin) → default; 50m leaves exactly 5m →
	// honored at the floor.
	const rb = "45m"
	dt := func(cfg string) time.Duration {
		return (&ReviewReadySpec{RunBound: rb, DispatchTimeout: cfg}).DispatchTimeoutDuration()
	}
	if got, want := dt("45m"), 60*time.Minute; got != want {
		t.Errorf("dispatchTimeout=runBound (zero margin) = %s, want default %s", got, want)
	}
	if got, want := dt("49m"), 60*time.Minute; got != want {
		t.Errorf("dispatchTimeout=49m (4m margin < 5m) = %s, want default %s", got, want)
	}
	if got, want := dt("50m"), 50*time.Minute; got != want {
		t.Errorf("dispatchTimeout=50m (exactly the margin floor) = %s, want honored %s", got, want)
	}
	// Unset dispatchTimeout with a raised wall → default follows the wall.
	if got, want := dt(""), 60*time.Minute; got != want {
		t.Errorf("unset dispatchTimeout with 45m wall = %s, want wall+15m %s", got, want)
	}
}
