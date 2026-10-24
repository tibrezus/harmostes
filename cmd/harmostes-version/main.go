// harmostes-version prints the release version for HEAD (#755), replacing
// the GitVersion step in the release pipeline. Deterministic git-tag/log
// arithmetic — see internal/versioner for the scheme.
//
//	harmostes-version               → next version for this worktree
//	harmostes-version -sha          → HEAD commit sha (tag target)
//	harmostes-version -preview-base → stable base + one patch (PR previews)
package main

import (
	"fmt"
	"os"

	"github.com/tibrezus/harmostes/internal/versioner"
)

func main() {
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	switch {
	case len(os.Args) > 1 && os.Args[1] == "-sha":
		fmt.Println(mustHead(dir))
	case len(os.Args) > 1 && os.Args[1] == "-preview-base":
		fmt.Println(must(versioner.PreviewBase(dir)))
	default:
		fmt.Println(must(versioner.Next(dir)))
	}
}

func must(v string, err error) string {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return v
}

func mustHead(dir string) string {
	out, err := gitOutput(dir, "rev-parse", "HEAD")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return out
}
