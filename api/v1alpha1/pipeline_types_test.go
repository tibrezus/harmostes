package v1alpha1

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestPipelineSerialization(t *testing.T) {
	original := &Pipeline{
		TypeMeta: metav1.TypeMeta{APIVersion: "harmostes.dev/v1alpha1", Kind: "Pipeline"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pipeline",
			Namespace: "harmostes",
			Labels:    map[string]string{"harmostes.dev/owner": "alice"},
		},
		Spec: PipelineSpec{
			Trigger: TriggerSpec{
				Type:   "webhook",
				Config: json.RawMessage(`{"secretRef":{"name":"webhook-secret"}}`),
			},
			Graph: GraphSpec{
				Nodes: []NodeSpec{
					{ID: "prepare", Type: "plugin", Outputs: []string{"artifact"}},
					{ID: "agent", Type: "agent", Config: json.RawMessage(`{"model":"zai/glm-5.2"}`)},
					{ID: "gate", Type: "gate"},
					{ID: "deploy", Type: "plugin"},
				},
				Edges: []EdgeSpec{
					{From: "prepare", To: "agent"},
					{From: "agent", To: "gate"},
					{From: "gate", To: "deploy", When: "green"},
					{From: "gate", To: "agent", When: "failed", MaxRetries: 3},
				},
			},
		},
	}

	// Marshal → JSON
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Unmarshal back
	var roundtrip Pipeline
	if err := json.Unmarshal(data, &roundtrip); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Verify fields
	if roundtrip.Name != "test-pipeline" {
		t.Errorf("name = %q, want test-pipeline", roundtrip.Name)
	}
	if roundtrip.Spec.Trigger.Type != "webhook" {
		t.Errorf("trigger type = %q, want webhook", roundtrip.Spec.Trigger.Type)
	}
	if len(roundtrip.Spec.Graph.Nodes) != 4 {
		t.Fatalf("nodes = %d, want 4", len(roundtrip.Spec.Graph.Nodes))
	}
	if len(roundtrip.Spec.Graph.Edges) != 4 {
		t.Fatalf("edges = %d, want 4", len(roundtrip.Spec.Graph.Edges))
	}

	// Verify the gate feedback loop edge
	loopEdge := roundtrip.Spec.Graph.Edges[3]
	if loopEdge.From != "gate" || loopEdge.To != "agent" {
		t.Errorf("loop edge = %s→%s, want gate→agent", loopEdge.From, loopEdge.To)
	}
	if loopEdge.When != "failed" {
		t.Errorf("loop edge when = %q, want failed", loopEdge.When)
	}
	if loopEdge.MaxRetries != 3 {
		t.Errorf("loop edge maxRetries = %d, want 3", loopEdge.MaxRetries)
	}
}

func TestPipelineDeepCopy(t *testing.T) {
	original := &Pipeline{
		ObjectMeta: metav1.ObjectMeta{Name: "deepcopy-test"},
		Spec: PipelineSpec{
			Graph: GraphSpec{
				Nodes: []NodeSpec{
					{ID: "a", Type: "plugin", Outputs: []string{"x"}},
				},
				Edges: []EdgeSpec{
					{From: "a", To: "b", When: "green", MaxRetries: 2},
				},
			},
		},
	}

	copy := original.DeepCopy()
	if copy == nil {
		t.Fatal("DeepCopy returned nil")
	}
	if copy.Name != original.Name {
		t.Errorf("name mismatch")
	}
	if len(copy.Spec.Graph.Nodes) != 1 {
		t.Errorf("nodes count mismatch")
	}
	// Mutate copy, verify original unchanged
	copy.Spec.Graph.Nodes[0].ID = "modified"
	if original.Spec.Graph.Nodes[0].ID == "modified" {
		t.Error("DeepCopy did not create independent copy — mutation leaked to original")
	}
}

func TestPipelineDeepCopyObject(t *testing.T) {
	p := &Pipeline{ObjectMeta: metav1.ObjectMeta{Name: "obj-test"}}
	obj := p.DeepCopyObject()
	if obj == nil {
		t.Fatal("DeepCopyObject returned nil")
	}
	pp, ok := obj.(*Pipeline)
	if !ok {
		t.Fatalf("DeepCopyObject returned %T, want *Pipeline", obj)
	}
	if pp.Name != p.Name {
		t.Errorf("name mismatch after DeepCopyObject")
	}
}

func TestPipelineListSerialization(t *testing.T) {
	list := &PipelineList{
		TypeMeta: metav1.TypeMeta{APIVersion: "harmostes.dev/v1alpha1", Kind: "PipelineList"},
		Items: []Pipeline{
			{ObjectMeta: metav1.ObjectMeta{Name: "pipe-1"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pipe-2"}},
		},
	}

	data, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var roundtrip PipelineList
	if err := json.Unmarshal(data, &roundtrip); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(roundtrip.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(roundtrip.Items))
	}
	if roundtrip.Items[0].Name != "pipe-1" {
		t.Errorf("item 0 name = %q, want pipe-1", roundtrip.Items[0].Name)
	}
}

func TestPipelineRegisteredInScheme(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	gvk := SchemeGroupVersion.WithKind("Pipeline")
	if !scheme.Recognizes(gvk) {
		t.Error("scheme does not recognize Pipeline GVK")
	}

	gvkList := SchemeGroupVersion.WithKind("PipelineList")
	if !scheme.Recognizes(gvkList) {
		t.Error("scheme does not recognize PipelineList GVK")
	}
}

func TestPipelineResource(t *testing.T) {
	gr := PipelineResource()
	if gr.Group != "harmostes.dev" {
		t.Errorf("group = %q, want harmostes.dev", gr.Group)
	}
	if gr.Resource != "pipelines" {
		t.Errorf("resource = %q, want pipelines", gr.Resource)
	}
}

func TestEdgeConditions(t *testing.T) {
	// Verify edge condition values are preserved correctly
	edges := []EdgeSpec{
		{From: "a", To: "b"},                                       // sequential (no condition)
		{From: "gate", To: "deploy", When: "green"},                // conditional
		{From: "gate", To: "agent", When: "failed", MaxRetries: 3}, // loop-back
		{From: "branch", To: "skip", When: "unchanged"},            // branch
	}

	data, err := json.Marshal(edges)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var roundtrip []EdgeSpec
	if err := json.Unmarshal(data, &roundtrip); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for i, expected := range edges {
		got := roundtrip[i]
		if got.From != expected.From || got.To != expected.To {
			t.Errorf("edge %d: %s→%s, want %s→%s", i, got.From, got.To, expected.From, expected.To)
		}
		if got.When != expected.When {
			t.Errorf("edge %d: when=%q, want %q", i, got.When, expected.When)
		}
		if got.MaxRetries != expected.MaxRetries {
			t.Errorf("edge %d: maxRetries=%d, want %d", i, got.MaxRetries, expected.MaxRetries)
		}
	}
}

func TestNodeConfigPreservation(t *testing.T) {
	// Config is raw JSON — verify it survives serialization round-trip
	node := NodeSpec{
		ID:      "agent",
		Type:    "agent",
		Config:  json.RawMessage(`{"model":"zai/glm-5.2","skill":"llm-wiki","tools":["read","write"]}`),
		Outputs: []string{"commitSha", "filesChanged"},
	}

	data, err := json.Marshal(node)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var roundtrip NodeSpec
	if err := json.Unmarshal(data, &roundtrip); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Parse config to verify it's preserved
	var cfg map[string]any
	if err := json.Unmarshal(roundtrip.Config, &cfg); err != nil {
		t.Fatalf("config parse: %v", err)
	}
	if cfg["model"] != "zai/glm-5.2" {
		t.Errorf("config model = %v, want zai/glm-5.2", cfg["model"])
	}
	if len(roundtrip.Outputs) != 2 {
		t.Errorf("outputs count = %d, want 2", len(roundtrip.Outputs))
	}
}
