// Package crdwalk holds the CRD-conformance walk: it traverses a Go CRD root
// type and the chart's CRD schema together, reporting (a) every Go field the
// schema cannot hold — the API server would prune it on every write, the
// #277/#289 class — and (b) a deterministic PROBE instance: every schema-legal
// leaf populated with a distinct, schema-derived value (enums take their first
// legal value, so the probe is always writable).
//
// Two consumers, one walk:
//
//   - the fast tier (api/v1alpha1/crd_conformance_test.go) fails on issues —
//     Go↔YAML drift is caught in seconds, no cluster;
//   - the integration tier (test/integration) writes the probe through the
//     REAL API server, reads it back, COLLECTS the stored leaf values, and
//     compares them against the probe's — proving each field SURVIVES a
//     round-trip. The acceptance matrix is derived from the same walk, so new
//     fields inherit the canary automatically instead of waiting for a
//     hand-picked test (#315).
package crdwalk

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

// Root pairs a chart CRD file with the Go root type whose JSON shape it must
// carry. Callers own the table (crdwalk stays dependency-free so API tests
// and integration tests can both import it without a cycle); new CRDs must
// be registered in each caller's table.
type Root struct {
	File string
	Type reflect.Type
}

// Leaf is one schema leaf: its dotted Go path and the canonical value string
// (probe-assigned in generate mode, stored on the server in collect mode).
type Leaf struct {
	Path  string
	Value string
}

// probeTime is fixed so probes are deterministic — a read-back must produce
// identical leaf values for every field that survived.
var probeTime = metav1.NewTime(time.Unix(1700000000, 0).UTC())

// patternProbes maps CRD `pattern` constraints to conforming probe values —
// the generic string probe ("probe.<path>") violates pinned patterns and the
// real apiserver rejects it (caught live by the acceptance matrix, #494). An
// unregistered pattern surfaces as a conformance issue (add the pattern + a
// value here).
var patternProbes = map[string]string{
	`^([01]\d|2[0-3]):[0-5]\d$`: "16:00", // HH:MM — agent.models windows (#494)
}

// LoadSchema extracts the v1alpha1 openAPIV3Schema root node from a chart
// CRD file.
func LoadSchema(crdDir, file string) (map[string]interface{}, error) {
	raw, err := os.ReadFile(crdDir + file)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	var doc struct {
		Spec struct {
			Versions []struct {
				Name   string `json:"name"`
				Schema struct {
					OpenAPIV3Schema map[string]interface{} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	for _, v := range doc.Spec.Versions {
		if v.Name == "v1alpha1" {
			if v.Schema.OpenAPIV3Schema == nil {
				return nil, fmt.Errorf("%s: v1alpha1 has no openAPIV3Schema", file)
			}
			return v.Schema.OpenAPIV3Schema, nil
		}
	}
	return nil, fmt.Errorf("%s: no v1alpha1 version", file)
}

// Analyze builds a fresh probe instance of root and returns it together with
// the leaf values it assigned and the conformance issues (Go fields the
// schema cannot hold).
func Analyze(root reflect.Type, schema map[string]interface{}) (probe any, leaves []Leaf, issues []string) {
	p := reflect.New(deref(root))
	leaves, issues = walkType(root, schema, "", false, p.Elem())
	return p.Interface(), leaves, issues
}

// Check reports the conformance issues for one root (the fast-tier surface).
func Check(root reflect.Type, schema map[string]interface{}) []string {
	_, _, issues := Analyze(root, schema)
	return issues
}

// Collect walks an EXISTING instance (e.g. an object read back from the API
// server) and records the stored value at every leaf the walk visits. It
// never mutates dst. Leaves absent from dst (empty slice, nil pointer — i.e.
// pruned by the server) simply do not appear, which is how the acceptance
// comparison detects pruning.
func Collect(root reflect.Type, schema map[string]interface{}, dst any) []Leaf {
	leaves, _ := walkType(root, schema, "", true, reflect.ValueOf(dst).Elem())
	return leaves
}

// wellKnownLeaf: Go types whose JSON encoding is scalar/arbitrary and which
// CRD schemas represent as leaf schemas (string, anyOf, x-kubernetes-*).
func wellKnownLeaf(t reflect.Type) bool {
	s := t.String()
	switch {
	case strings.HasSuffix(s, ".Time"), // metav1.Time, v1.Time
		strings.HasSuffix(s, ".Duration"), // metav1.Duration
		strings.HasSuffix(s, ".Quantity"), // resource.Quantity
		strings.HasSuffix(s, ".IntOrString"),
		strings.HasSuffix(s, ".RawExtension"),
		strings.HasSuffix(s, ".JSON"):
		return true
	}
	return false
}

func jsonName(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	name := strings.Split(tag, ",")[0]
	if name == "" {
		if f.Anonymous {
			return "", true // inline embed: merge into the parent's schema node
		}
		return f.Name, false // untagged exported field: treat as drift if reached
	}
	return name, true
}

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// walkType traverses a Go type and its schema node together. In generate
// mode (collect=false, dst non-nil) it builds the probe instance in place and
// records the generated values; in collect mode it records the STORED values
// without mutating anything.
func walkType(t reflect.Type, schema map[string]interface{}, path string, collect bool, rv reflect.Value) ([]Leaf, []string) {
	leaves := []Leaf{}
	issues := []string{}
	record := func(p, v string) {
		if p != "" {
			leaves = append(leaves, Leaf{Path: p, Value: v})
		}
	}
	missing := func(format string, args ...any) {
		issues = append(issues, fmt.Sprintf(format, args...))
	}

	t = deref(t)

	// Indirect through pointer values in generate mode so the
	// struct/slice/map cases always hold an addressable value of the deref'd
	// type. In collect mode a nil pointer IS the finding (nothing stored).
	if rv.IsValid() {
		for rv.Kind() == reflect.Pointer {
			if collect {
				if rv.IsNil() {
					record(path, "<nil>")
					return leaves, issues
				}
				rv = rv.Elem()
				break
			}
			if rv.IsNil() {
				rv.Set(reflect.New(rv.Type().Elem()))
			}
			rv = rv.Elem()
		}
	}

	if wellKnownLeaf(t) {
		if !collect {
			setWellKnown(t, rv)
		}
		record(path, leafValue(rv))
		return leaves, issues
	}
	if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
		if !collect && rv.IsValid() {
			rv.Set(reflect.ValueOf([]byte(`{"probe":true}`)).Convert(t))
		}
		record(path, leafValue(rv))
		return leaves, issues // []byte and named byte-slice types are JSON leaves
	}

	switch t.Kind() {
	case reflect.Struct:
		name := t.String()
		if name == "metav1.ObjectMeta" || name == "v1.ObjectMeta" || name == "metav1.TypeMeta" || name == "v1.TypeMeta" {
			return leaves, issues // metadata/type meta are preserved wholesale by CRDs
		}
		props, _ := schema["properties"].(map[string]interface{})
		if props == nil {
			if preserve, _ := schema["x-kubernetes-preserve-unknown-fields"].(bool); preserve {
				return leaves, issues // nothing under this subtree can be pruned
			}
			missing("%s: Go struct %s but schema has no properties (would prune everything under %s)", path, name, path)
			return leaves, issues
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			child, ok := jsonName(f)
			if !ok {
				continue
			}
			childPath := child
			if path != "" {
				childPath = path + "." + child
			}
			if f.Anonymous && child == "" { // inline embed: same schema node
				sub, iss := walkType(f.Type, schema, path, collect, rv.Field(i))
				leaves = append(leaves, sub...)
				issues = append(issues, iss...)
				continue
			}
			ft := deref(f.Type).String()
			if strings.HasSuffix(ft, ".ObjectMeta") || strings.HasSuffix(ft, ".TypeMeta") {
				continue // preserved wholesale by CRDs, never pruned
			}
			node, ok := props[child].(map[string]interface{})
			if !ok {
				missing("%s: field %q (Go %s) missing from CRD schema — the API server will prune it; add it to the chart CRD", childPath, child, f.Type)
				continue
			}
			sub, iss := walkType(f.Type, node, childPath, collect, rv.Field(i))
			leaves = append(leaves, sub...)
			issues = append(issues, iss...)
		}
	case reflect.Slice, reflect.Array:
		items, ok := schema["items"].(map[string]interface{})
		if !ok {
			missing("%s: Go %s but schema has no items schema", path, t)
			return leaves, issues
		}
		if t.Kind() == reflect.Slice && rv.IsValid() {
			if collect {
				if rv.Len() == 0 {
					return leaves, issues // pruned/emptied: leaves just don't appear
				}
				sub, iss := walkType(t.Elem(), items, path+"[]", collect, rv.Index(0))
				leaves = append(leaves, sub...)
				issues = append(issues, iss...)
				return leaves, issues
			}
			rv.Set(reflect.MakeSlice(t, 1, 1))
			sub, iss := walkType(t.Elem(), items, path+"[]", collect, rv.Index(0))
			leaves = append(leaves, sub...)
			issues = append(issues, iss...)
			return leaves, issues
		}
		sub, iss := walkType(t.Elem(), items, path+"[]", collect, reflect.Value{})
		leaves = append(leaves, sub...)
		issues = append(issues, iss...)
	case reflect.Map:
		// Preserve-unknown maps carry no elem schema; walk the elem against
		// the PARENT schema node. additionalProperties maps carry the elem
		// schema. Either way: generate builds the single deterministic
		// entry, collect reads the stored entry through a mutable COPY
		// (map values are unaddressable in reflection).
		elemSchema := schema
		if _, preserve := schema["x-kubernetes-preserve-unknown-fields"].(bool); !preserve {
			ap, ok := schema["additionalProperties"].(map[string]interface{})
			if !ok {
				missing("%s: Go %s but schema has no additionalProperties", path, t)
				return leaves, issues
			}
			elemSchema = ap
		}
		if !rv.IsValid() || t.Key().Kind() != reflect.String {
			sub, iss := walkType(t.Elem(), elemSchema, path+"{}", collect, reflect.Value{})
			leaves = append(leaves, sub...)
			issues = append(issues, iss...)
			return leaves, issues
		}
		if collect {
			if rv.Len() == 0 {
				return leaves, issues // pruned/emptied: leaves just don't appear
			}
			cv := reflect.New(t.Elem()).Elem()
			cv.Set(firstMapValue(rv))
			sub, iss := walkType(t.Elem(), elemSchema, path+"{}", collect, cv)
			leaves = append(leaves, sub...)
			issues = append(issues, iss...)
			return leaves, issues
		}
		ev := reflect.New(t.Elem()).Elem()
		sub, iss := walkType(t.Elem(), elemSchema, path+"{}", collect, ev)
		leaves = append(leaves, sub...)
		issues = append(issues, iss...)
		m := reflect.MakeMap(t)
		m.SetMapIndex(reflect.ValueOf("probe"), ev)
		rv.Set(m)
		return leaves, issues
	case reflect.Interface:
		if !collect && rv.IsValid() {
			rv.Set(reflect.ValueOf(map[string]interface{}{"probe": true}))
		}
		record(path, leafValue(rv))
	default:
		// string/number/bool leaf
		if !collect && rv.IsValid() {
			if pattern, ok := schema["pattern"].(string); ok && pattern != "" {
				if _, known := patternProbes[pattern]; !known {
					missing("%s: schema pattern %q has no registered probe value — add it to crdwalk.patternProbes", path, pattern)
				}
			}
			sv := scalarValue(t, schema, path)
			if pattern, ok := schema["pattern"].(string); ok && pattern != "" {
				if pv, known := patternProbes[pattern]; known {
					sv = reflect.ValueOf(pv).Convert(t)
				} else {
					missing("%s: schema pattern %q has no registered probe value — add it to crdwalk.patternProbes", path, pattern)
				}
			}
			rv.Set(sv)
		}
		record(path, leafValue(rv))
	}
	return leaves, issues
}

// ── The reverse direction (#704 class, #709/#710): schema → Go ──
//
// The forward walk derives everything from the Go type, so a schema
// property no Go field can receive is invisible to every tier: the API
// server stores the declaration, kubectl shows it, CI is green, and the
// typed decode silently drops it on every read — while the UI's typed
// splice writes it back OUT of the values file (production-inert feature,
// found only by reading a compiled Job by hand). CheckReverse walks the
// SCHEMA and asks whether the Go side can hold each property.

// CheckReverse reports the reverse-conformance issues for one root (the
// same Check surface the fast tier fails on). Metadata is preserved
// wholesale by CRDs and skipped in both directions; a Go map or interface
// holds arbitrary keys, so properties under it cannot be dropped; nodes
// whose properties are absent have nothing to pair and belong to the
// forward walk's domain.
func CheckReverse(root reflect.Type, schema map[string]interface{}) []string {
	issues := []string{}
	checkReverse(deref(root), schema, "", &issues)
	return issues
}

func checkReverse(t reflect.Type, node map[string]interface{}, path string, issues *[]string) {
	if node == nil {
		return // no schema node here: the forward walk's finding, not ours
	}
	if preserve, _ := node["x-kubernetes-preserve-unknown-fields"].(bool); preserve {
		return // the API server keeps everything under this subtree — nothing to prune
	}
	props, hasProps := node["properties"].(map[string]interface{})
	if !hasProps {
		if items, ok := node["items"].(map[string]interface{}); ok {
			checkReverse(elemTypeOf(t), items, path+"[]", issues)
		}
		if ap, ok := node["additionalProperties"].(map[string]interface{}); ok {
			checkReverse(elemTypeOf(t), ap, path+"{}", issues)
		}
		return
	}
	if t == nil {
		return // interface/RawExtension side: holds any shape, nothing to drop
	}
	t = deref(t)
	if wellKnownLeaf(t) || t.Kind() == reflect.Interface {
		return // the Go side is a leaf too; its shape is the forward walk's business
	}
	switch t.Kind() {
	case reflect.Struct:
		names := jsonNamesOf(t)
		for _, prop := range sortedKeys(props) {
			if prop == "metadata" {
				continue // preserved wholesale by CRDs, never decoded field-wise
			}
			childPath := prop
			if path != "" {
				childPath = path + "." + prop
			}
			ft, ok := names[prop]
			if !ok {
				if pn, _ := props[prop].(map[string]interface{}); pn != nil {
					if preserve, _ := pn["x-kubernetes-preserve-unknown-fields"].(bool); preserve {
						continue // the conventional empty-status shape: preserved server-side, deliberately untyped
					}
				}
				*issues = append(*issues, fmt.Sprintf("%s: schema property %q has no Go field — the API server stores the declaration, the typed decode silently drops it (#704 class); add the field or remove the property", childPath, prop))
				continue
			}
			pn, _ := props[prop].(map[string]interface{})
			checkReverse(ft, pn, childPath, issues)
		}
	case reflect.Map:
		for _, prop := range sortedKeys(props) {
			if prop == "metadata" {
				continue
			}
			childPath := prop
			if path != "" {
				childPath = path + "." + prop
			}
			pn, _ := props[prop].(map[string]interface{})
			checkReverse(t.Elem(), pn, childPath, issues)
		}
	default:
		// scalar Go type under an object schema node: a shape mismatch the
		// forward walk reports from its side; nothing to pair here.
	}
}

// elemTypeOf returns the element type of a slice/array/map type (nil when
// t is nil or of another kind — recursion then runs untyped and reports
// nothing, the same courtesy the forward walk extends to any{}).
func elemTypeOf(t reflect.Type) reflect.Type {
	if t == nil {
		return nil
	}
	t = deref(t)
	switch t.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		return t.Elem()
	}
	return nil
}

// jsonNamesOf collects the json names a struct exposes at its schema node —
// exactly the set the forward walk walks: inline embeds merged into the
// parent, untagged and json:"-" fields excluded.
func jsonNamesOf(t reflect.Type) map[string]reflect.Type {
	names := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		child, ok := jsonName(f)
		if f.Anonymous && child == "" { // inline embed: same schema node
			if ok {
				for k, v := range jsonNamesOf(f.Type) {
					names[k] = v
				}
			}
			continue
		}
		if !ok {
			continue
		}
		names[child] = f.Type
	}
	return names
}

func sortedKeys(m map[string]interface{}) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// SchemaLeaf is a leaf the SCHEMA declares, with a deterministic probe value
// satisfying its constraints (enum → default → registered pattern probe →
// generic, mirroring the forward walk's probe discipline). Type is the
// schema's declared scalar type — consumers generating YAML documents need
// it to emit NATIVE values ("7", not "7" as a string: the typed decode
// rejects quoted numbers).
type SchemaLeaf struct {
	Path  string
	Value string
	Type  string
}

// SchemaLeaves enumerates the declared scalar leaves of a schema subtree,
// paired with the Go type that must hold them (nil-typed recursion runs
// schema-only). It is the generator for the UI typed round-trip guard
// (#710): a document carrying every declared leaf must survive
// templateDocument → spliceTemplateIntoValues, because CheckReverse
// guarantees the Go side can hold each of them. Zero-valued probes are
// skipped: the splice prunes zero leaves by design (pruneEmpty), so they
// cannot survive ANY round-trip and are a different class entirely.
func SchemaLeaves(root reflect.Type, node map[string]interface{}) []SchemaLeaf {
	leaves := []SchemaLeaf{}
	schemaLeaves(deref(root), node, "", &leaves)
	return leaves
}

func schemaLeaves(t reflect.Type, node map[string]interface{}, path string, leaves *[]SchemaLeaf) {
	if node == nil {
		return
	}
	if preserve, _ := node["x-kubernetes-preserve-unknown-fields"].(bool); preserve {
		return // unprunable subtree: its declared leaves are covered by the pairing, its unknown keys are unenumerable
	}
	props, hasProps := node["properties"].(map[string]interface{})
	if !hasProps {
		if items, ok := node["items"].(map[string]interface{}); ok {
			schemaLeaves(elemTypeOf(t), items, path+"[]", leaves)
			return
		}
		if ap, ok := node["additionalProperties"].(map[string]interface{}); ok {
			schemaLeaves(elemTypeOf(t), ap, path+"{}", leaves)
			return
		}
		if path == "" {
			return
		}
		if v, ok := schemaProbe(t, node, path); ok {
			typ, _ := node["type"].(string)
			*leaves = append(*leaves, SchemaLeaf{Path: path, Value: v, Type: typ})
		}
		return
	}
	for _, prop := range sortedKeys(props) {
		if prop == "metadata" {
			continue
		}
		childPath := prop
		if path != "" {
			childPath = path + "." + prop
		}
		pn, _ := props[prop].(map[string]interface{})
		schemaLeaves(childTypeOf(t, prop), pn, childPath, leaves)
	}
}

// childTypeOf descends the Go pairing alongside the schema: struct fields by
// json name, maps/slices by element. A nil result means "unpaired" — the
// probe then relies on the schema node alone.
func childTypeOf(t reflect.Type, prop string) reflect.Type {
	if t == nil {
		return nil
	}
	t = deref(t)
	switch t.Kind() {
	case reflect.Struct:
		return jsonNamesOf(t)[prop]
	case reflect.Map, reflect.Slice, reflect.Array:
		return t.Elem()
	}
	return nil
}

// schemaProbe picks a probe value for one scalar schema node; ok=false when
// the only legal probe is a zero value (pruned by design) or the node is a
// pure container the walker should have descended into. A well-known Go
// pairing (duration, quantity, int-or-string) takes the canonical well-known
// probe — the typed decode must be able to PARSE the probe, not just store it.
func schemaProbe(t reflect.Type, node map[string]interface{}, path string) (string, bool) {
	if t != nil {
		dt := deref(t)
		if wellKnownLeaf(dt) {
			rv := reflect.New(dt).Elem()
			setWellKnown(dt, rv)
			return leafValue(rv), true
		}
		if dt.Kind() == reflect.Interface {
			return `{"probe":true}`, true
		}
	}
	if enums, ok := node["enum"].([]interface{}); ok && len(enums) > 0 {
		if e, ok := enums[0].(string); ok && e != "" {
			return e, true
		}
	}
	if d, ok := node["default"]; ok {
		if s := fmt.Sprintf("%v", d); s != "" && s != "0" && s != "false" && s != "<nil>" {
			return s, true
		}
	}
	if p, ok := node["pattern"].(string); ok && p != "" {
		if pv, known := patternProbes[p]; known {
			return pv, true
		}
	}
	switch node["type"] {
	case "boolean":
		return "", false // false is the zero value; pruned by design
	case "integer", "number":
		n := 7.0
		if mn, ok := node["minimum"].(float64); ok && n < mn {
			n = mn
		}
		if mx, ok := node["maximum"].(float64); ok && n > mx {
			n = mx
		}
		if n == 0 {
			return "", false
		}
		if n == float64(int64(n)) {
			return strconv.FormatInt(int64(n), 10), true
		}
		return strconv.FormatFloat(n, 'f', -1, 64), true
	case "string":
		s := "probe." + path
		if ml, ok := node["maxLength"].(float64); ok && ml >= 1 && int(ml) < len(s) {
			s = s[:int(ml)]
		}
		return s, s != ""
	}
	return "probe." + path, true // untyped leaf (int-or-string, any): a string probe
}

// firstMapValue returns the alphabetically-first entry's value
// (deterministic pick for single-entry round-trips).
func firstMapValue(m reflect.Value) reflect.Value {
	keys := m.MapKeys()
	sort.Slice(keys, func(i, j int) bool {
		return fmt.Sprint(keys[i].Interface()) < fmt.Sprint(keys[j].Interface())
	})
	return m.MapIndex(keys[0])
}

// setWellKnown populates a well-known leaf (through pointer fields too).
func setWellKnown(t reflect.Type, rv reflect.Value) {
	if !rv.IsValid() {
		return
	}
	if rv.Kind() == reflect.Pointer {
		p := reflect.New(t)
		setWellKnown(t, p.Elem())
		rv.Set(p)
		return
	}
	switch {
	case strings.HasSuffix(t.String(), ".Time"):
		rv.Set(reflect.ValueOf(probeTime).Convert(rv.Type()))
	case strings.HasSuffix(t.String(), ".Duration"):
		rv.Set(reflect.ValueOf(metav1.Duration{Duration: 7 * time.Second}).Convert(rv.Type()))
	case strings.HasSuffix(t.String(), ".Quantity"):
		rv.Set(reflect.ValueOf(resource.MustParse("7")).Convert(rv.Type()))
	case strings.HasSuffix(t.String(), ".IntOrString"):
		rv.Set(reflect.ValueOf(intstr.FromInt32(7)).Convert(rv.Type()))
	case strings.HasSuffix(t.String(), ".RawExtension"):
		rv.Set(reflect.ValueOf(runtime.RawExtension{Raw: []byte(`{"probe":true}`)}).Convert(rv.Type()))
	}
}

// leafValue canonicalizes a leaf for comparison — the SAME function on both
// sides of the round-trip.
func leafValue(rv reflect.Value) string {
	if !rv.IsValid() {
		return "<none>"
	}
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return "<nil>"
		}
		return leafValue(rv.Elem())
	}
	switch v := rv.Interface().(type) {
	case metav1.Time:
		return v.UTC().Format(time.RFC3339)
	case metav1.Duration:
		return v.Duration.String()
	case resource.Quantity:
		return v.String()
	case intstr.IntOrString:
		return v.String()
	case runtime.RawExtension:
		return string(v.Raw)
	}
	return fmt.Sprintf("%v", rv.Interface())
}

// scalarValue produces a schema-legal, path-distinct value for a scalar leaf.
// Enums take their FIRST legal value — the probe must be writable; maxLength
// truncates.
func scalarValue(t reflect.Type, schema map[string]interface{}, path string) reflect.Value {
	v := reflect.New(t).Elem()
	switch t.Kind() {
	case reflect.String:
		s := "probe." + path
		if ml, ok := schema["maxLength"].(float64); ok && ml >= 1 && int(ml) < len(s) {
			s = s[:int(ml)]
		}
		if enums, ok := schema["enum"].([]interface{}); ok && len(enums) > 0 {
			if e, ok := enums[0].(string); ok {
				s = e
			}
		}
		v.SetString(s)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(clampInt(7, schema))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(clampInt(7, schema)))
	case reflect.Float32, reflect.Float64:
		f := 7.5
		if mn, ok := schema["minimum"].(float64); ok && f < mn {
			f = mn
		}
		if mx, ok := schema["maximum"].(float64); ok && f > mx {
			f = mx
		}
		v.SetFloat(f)
	}
	return v
}

// clampInt fits the generic integer probe (7) inside the schema's
// minimum/maximum bounds — the acceptance matrix must only write LEGAL
// values (#556: retry.maxAttempts carries maximum: 5, and the 7-probe
// was rejected by the real apiserver).
func clampInt(def int64, schema map[string]interface{}) int64 {
	if mn, ok := schema["minimum"].(float64); ok && def < int64(mn) {
		def = int64(mn)
	}
	if mx, ok := schema["maximum"].(float64); ok && def > int64(mx) {
		def = int64(mx)
	}
	return def
}
