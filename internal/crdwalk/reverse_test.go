package crdwalk

import (
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The reverse direction (#704 class, #709/#710): the forward walk derives
// everything from the Go type, so a schema property no Go field can receive
// is invisible to every tier — the API server stores the declaration, the
// typed decode silently drops it on read, and the UI's typed splice writes
// it back OUT of the values file. These tests pin the detector on synthetic
// schemas; the real four roots are covered green by the fast tier
// (api/v1alpha1/crd_conformance_test.go).

type ReverseInner struct {
	Name string `json:"name"`
}

type reverseSubject struct {
	ReverseInner `json:",inline"`  // true anonymous embed: merges into the parent node
	Known        string            `json:"known"`
	Numbers      int               `json:"numbers"`
	Bag          map[string]string `json:"bag,omitempty"`
}

func reverseSchema() map[string]interface{} {
	return map[string]interface{}{
		"properties": map[string]interface{}{
			"known":   map[string]interface{}{"type": "string"},
			"numbers": map[string]interface{}{"type": "integer", "minimum": 1.0},
			"name":    map[string]interface{}{"type": "string"}, // satisfied by the inline embed
			"bag": map[string]interface{}{
				"type":                 "object",
				"additionalProperties": map[string]interface{}{"type": "string"},
				"properties": map[string]interface{}{
					"anything": map[string]interface{}{"type": "string"}, // maps receive arbitrary keys
				},
			},
			"ghost": map[string]interface{}{ // the #704 shape: declared, untypeable
				"type": "object",
				"properties": map[string]interface{}{
					"phantom": map[string]interface{}{"type": "string"},
				},
			},
			"status": map[string]interface{}{ // the conventional empty-status shape
				"type":                                 "object",
				"x-kubernetes-preserve-unknown-fields": true,
			},
			"metadata": map[string]interface{}{"type": "object"},
		},
	}
}

func TestCheckReverseDetectsSchemaOnlyProperty(t *testing.T) {
	issues := CheckReverse(reflect.TypeOf(reverseSubject{}), reverseSchema())
	if len(issues) != 1 {
		t.Fatalf("exactly one finding (the ghost), got %d: %v", len(issues), issues)
	}
	if !strings.Contains(issues[0], "ghost") || !strings.Contains(issues[0], "#704") {
		t.Fatalf("the finding must name the property and the class: %s", issues[0])
	}
}

func TestCheckReverseCleanWhenEveryPropertyIsTypeable(t *testing.T) {
	schema := reverseSchema()
	props := schema["properties"].(map[string]interface{})
	delete(props, "ghost") // every remaining property is receivable
	if issues := CheckReverse(reflect.TypeOf(reverseSubject{}), schema); len(issues) != 0 {
		t.Fatalf("embeds, maps and the preserve-unknown status must all pass: %v", issues)
	}
}

type reverseStatusful struct {
	Spec   ReverseInner        `json:"spec"`
	Status reverseStatusStruct `json:"status,omitempty"`
}

type reverseStatusStruct struct {
	Phase string `json:"phase"`
}

func TestCheckReverseWalksStatusWhenTyped(t *testing.T) {
	schema := func(ghost bool) map[string]interface{} {
		props := map[string]interface{}{"phase": map[string]interface{}{"type": "string"}}
		if ghost {
			props["ghost"] = map[string]interface{}{"type": "string"} // declared, untypeable
		}
		return map[string]interface{}{
			"properties": map[string]interface{}{
				"spec":   map[string]interface{}{"properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}}},
				"status": map[string]interface{}{"properties": props},
			},
		}
	}
	if issues := CheckReverse(reflect.TypeOf(reverseStatusful{}), schema(false)); len(issues) != 0 {
		t.Fatalf("a typed status is walked like any subtree: %v", issues)
	}
	issues := CheckReverse(reflect.TypeOf(reverseStatusful{}), schema(true))
	if len(issues) != 1 || !strings.Contains(issues[0], "status.ghost") {
		t.Fatalf("a schema-only leaf under a TYPED status must be found: %v", issues)
	}
}

func TestSchemaLeavesProbes(t *testing.T) {
	schema := map[string]interface{}{
		"properties": map[string]interface{}{
			"patterned": map[string]interface{}{"type": "string", "pattern": "^([01]\\d|2[0-3]):[0-5]\\d$"},
			"picked":    map[string]interface{}{"type": "string", "enum": []interface{}{"first", "second"}},
			"defaulted": map[string]interface{}{"type": "string", "default": "the-default"},
			"capped":    map[string]interface{}{"type": "integer", "maximum": 5.0},
			"flag":      map[string]interface{}{"type": "boolean"}, // false is the zero value → skipped
			"zeroed":    map[string]interface{}{"type": "integer", "maximum": 0.0},
			"wait":      map[string]interface{}{"type": "string"}, // metav1.Duration pairing
			"list": map[string]interface{}{"type": "array", "items": map[string]interface{}{
				"type": "object", "properties": map[string]interface{}{"leaf": map[string]interface{}{"type": "string"}},
			}},
		},
	}
	leaves := SchemaLeaves(reflect.TypeOf(struct {
		Patterned string           `json:"patterned"`
		Picked    string           `json:"picked"`
		Defaulted string           `json:"defaulted"`
		Capped    int              `json:"capped"`
		Flag      bool             `json:"flag"`
		Zeroed    int              `json:"zeroed"`
		Wait      *metav1.Duration `json:"wait"`
		List      []ReverseInner   `json:"list"`
	}{}), schema)
	got := map[string]string{}
	for _, l := range leaves {
		got[l.Path] = l.Value
	}
	if got["patterned"] != "16:00" {
		t.Fatalf("a registered pattern must take its probe, got %q", got["patterned"])
	}
	if got["picked"] != "first" || got["defaulted"] != "the-default" {
		t.Fatalf("enum/default probes wrong: %q %q", got["picked"], got["defaulted"])
	}
	if got["capped"] != "5" {
		t.Fatalf("integer probes clamp to the schema maximum, got %q", got["capped"])
	}
	if _, ok := got["flag"]; ok {
		t.Fatal("zero-valued probes are skipped — the splice prunes them by design")
	}
	if _, ok := got["zeroed"]; ok {
		t.Fatal("a maximum-0 integer has only the zero probe — skipped")
	}
	if _, err := time.ParseDuration(got["wait"]); err != nil {
		t.Fatalf("a well-known pairing must produce a PARSEABLE probe (the typed decode reads it), got %q", got["wait"])
	}
	if got["list[].leaf"] == "" {
		t.Fatalf("array/object recursion must reach nested leaves, got %v", got)
	}
}
