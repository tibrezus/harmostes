// Package harmostes carries the chart the binary was BUILT FROM. The UI
// image is distroless (binary only), but fixture mode needs the chart's
// CRDs + values.yaml to seed its deterministic world — a review
// environment (ui.fixture) runs the fixture UI in a container where no
// chart directory exists. The embed is single-sourced: this is the same
// chart/ directory Flux publishes; the binary carries the exact tree its
// build compiled.
package harmostes

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed chart/crds chart/values.yaml
var embedded embed.FS

// ChartFS returns the embedded chart subtree rooted at "chart/".
func ChartFS() fs.FS {
	sub, err := fs.Sub(embedded, "chart")
	if err != nil {
		panic("chartfs: the embedded chart/ subtree is a build-time constant: " + err.Error())
	}
	return sub
}

// ExtractChart writes the embedded chart under dst (creating it), so the
// fixture's disk-based loader reads a real directory. The layout mirrors
// the repo: dst/crds/*.yaml + dst/values.yaml.
func ExtractChart(dst string) error {
	root := ChartFS()
	return fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, p), 0o755)
		}
		src, err := root.Open(p)
		if err != nil {
			return fmt.Errorf("open embedded %s: %w", p, err)
		}
		defer func() { _ = src.Close() }()
		out, err := os.Create(filepath.Join(dst, filepath.FromSlash(p)))
		if err != nil {
			return fmt.Errorf("create %s: %w", filepath.Join(dst, p), err)
		}
		if _, err := io.Copy(out, src); err != nil {
			_ = out.Close()
			return fmt.Errorf("write %s: %w", p, err)
		}
		return out.Close()
	})
}
