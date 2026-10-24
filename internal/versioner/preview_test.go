package versioner

import "testing"

// PreviewBase is the mainline version an open PR is a candidate for: the
// stable base one patch ahead. Preview binaries self-report
// <base>-pr<N>.<sha8> instead of the old frozen 0.0.0-pr<N> (#755).
func TestPreviewBase(t *testing.T) {
	t.Run("frozen prerelease tree", func(t *testing.T) {
		f := newFixture(t)
		f.commit("chore: seed")
		f.tag("v1.2.0-335")
		f.commit("feat: branch work (#755)")
		f.assertPreviewBase("1.2.1", "stable base 1.2.0 + one patch")
	})

	t.Run("stable tree", func(t *testing.T) {
		f := newFixture(t)
		f.commit("chore: seed")
		f.tag("v1.2.1")
		f.commit("feat: branch work (#756)")
		f.assertPreviewBase("1.2.2", "stable base 1.2.1 + one patch")
	})

	t.Run("newest prerelease under a stable", func(t *testing.T) {
		f := newFixture(t)
		f.commit("chore: seed")
		f.tag("v1.2.0-400")
		f.commit("fix: a (#1)")
		f.tag("v1.2.1")
		f.assertPreviewBase("1.2.2", "v1.2.1 stable governs; branch commits never count")
	})
}
