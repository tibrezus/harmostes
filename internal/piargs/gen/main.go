// Command gen renders the extension single-source into its two consumed
// forms (#339): the checked-in JSON artifact (extensions.json — the machine
// form tests and drift checks compare against) and the generated block in
// harmostes.py (the standalone primitive is fetched as ONE file, so the
// artifact is materialized into it at generation time, not fetched at
// runtime). Run: go generate ./internal/piargs
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/tibrezus/harmostes/internal/piargs"
)

func main() {
	json, pyBlock, err := piargs.RenderExtensions()
	if err != nil {
		fmt.Fprintln(os.Stderr, "render:", err)
		os.Exit(1)
	}
	if err := os.WriteFile("extensions.json", json, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write extensions.json:", err)
		os.Exit(1)
	}
	const (
		begin = "# BEGIN GENERATED EXTENSIONS (go generate ./internal/piargs — do not edit)"
		end   = "# END GENERATED EXTENSIONS"
	)
	raw, err := os.ReadFile("../../harmostes.py")
	if err != nil {
		fmt.Fprintln(os.Stderr, "read harmostes.py:", err)
		os.Exit(1)
	}
	src := string(raw)
	i, j := strings.Index(src, begin), strings.Index(src, end)
	if i < 0 || j < 0 || j < i {
		fmt.Fprintln(os.Stderr, "harmostes.py: generated-extension markers missing")
		os.Exit(1)
	}
	spliced := src[:i] + begin + "\n" + string(pyBlock) + end + src[j+len(end):]
	if err := os.WriteFile("../../harmostes.py", []byte(spliced), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write harmostes.py:", err)
		os.Exit(1)
	}
}
