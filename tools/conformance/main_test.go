package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The parse gate rides inside the tool (!288 lesson): a file whose YAML
// cannot round-trip a real decoder fails BEFORE the apiserver sees it.
// docsOf is that gate's seam — pin its behavior: multi-doc split, empty
// docs skipped, glued partial-line edits surface as errors.
func TestDocsOfSplitsAndSurvivesMultiDoc(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "multi.yaml")
	body := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n---\n# comment doc\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: b\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	docs, err := docsOf(path)
	if err != nil {
		t.Fatalf("docsOf: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("docs = %d, want 2 (comment-only doc skipped)", len(docs))
	}
}

func TestDocsOfRejectsGluedYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "glued.yaml")
	// the partial-line-edit class: a deleted newline glues a value line
	// to the NEXT key ("name: a" + "namespace: b" on one line)
	body := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a  namespace: harmostes\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := docsOf(path); err == nil {
		t.Fatal("glued YAML parsed clean — the !288 gate is dead")
	}
}
