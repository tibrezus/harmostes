package ui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The stylesheet contract (#537): every CSS custom property the component
// layer references must be defined somewhere in the shipped stylesheets.
// Live-learned: 33 var(--ds-*) usages pointed at six tokens that were never
// defined — the execution graph silently fell back to hardcoded hex (white
// node cards in dark mode). A missing token is invisible to every other
// tier: the page renders, the fallback "works", the theme lies.
func TestCSSVarReferencesAreDefined(t *testing.T) {
	dir := filepath.Join("static", "css")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read css dir: %v", err)
	}
	defined := map[string]bool{}
	references := map[string][]string{}
	refRe := regexp.MustCompile(`var\(\s*(--[A-Za-z0-9-]+)`)
	defRe := regexp.MustCompile(`^\s*(--[A-Za-z0-9-]+)\s*:`)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".css") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if m := defRe.FindStringSubmatch(line); m != nil {
				defined[m[1]] = true
			}
		}
		for _, m := range refRe.FindAllStringSubmatch(string(b), -1) {
			references[m[1]] = append(references[m[1]], e.Name())
		}
	}
	// A reference is satisfied by a definition OR by another reference's
	// fallback chain… no — a bare var(--x) with no --x defined anywhere is
	// the defect; var(--x, #fff) hides it but still renders a lie. Both
	// count as unresolved unless defined.
	// Brace balance per file: an unclosed block silently swallows every
	// rule after it as "nested" — live-learned when .metrics-toolbar
	// (app.css:1818) ate the entire run-graph + canvas tail for weeks
	// (#537: the browser parsed 307 of 700+ rules and nobody's rule fired).
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".css") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if d := strings.Count(string(b), "{") - strings.Count(string(b), "}"); d != 0 {
			t.Errorf("%s: unbalanced braces (%+d) — every rule after the unclosed block is dead", e.Name(), d)
		}
	}

	var unresolved []string
	for name, files := range references {
		if !defined[name] {
			unresolved = append(unresolved, name+" (used in "+strings.Join(files, ", ")+")")
		}
	}
	if len(unresolved) > 0 {
		t.Errorf("CSS custom properties referenced but never defined (theme lies, fallbacks render):\n\t%s",
			strings.Join(unresolved, "\n\t"))
	}
}
