package versioner

import "testing"

// fixture builds an isolated git repository and returns a helper driving it.
type fixture struct {
	t   *testing.T
	dir string
}

// Red-first suite for #755: deterministic release versioning.
//
//   - a PR merged to main (subject ends "(#N)") bumps the patch
//   - direct pushes to main only increment the -N suffix
//   - stable tags advance the base; prerelease tags never do
//   - transition: a tree with only v1.2.0-<build> tags releases 1.2.1 next
func TestVersioner(t *testing.T) {
	t.Run("transition from frozen prerelease tags", func(t *testing.T) {
		f := newFixture(t)
		f.commit("chore: seed")
		f.tag("v1.2.0-335")
		f.commit("feat: the scheme lands (#755)")
		f.assertNext("1.2.1", "every PR merge bumps the patch off the frozen base")
	})

	t.Run("stable tags advance the base", func(t *testing.T) {
		f := newFixture(t)
		f.commit("chore: seed")
		f.tag("v1.2.1")
		f.commit("fix: something (#756)")
		f.assertNext("1.2.2", "one merge since the stable tag")
	})

	t.Run("multiple merges between release runs collapse", func(t *testing.T) {
		f := newFixture(t)
		f.commit("chore: seed")
		f.tag("v1.2.1")
		f.commit("fix: a (#756)")
		f.commit("feat: b (#757)")
		f.assertNext("1.2.3", "two merged PRs since v1.2.1 → +2 patches (content-addressed)")
	})

	t.Run("direct pushes increment only the suffix", func(t *testing.T) {
		f := newFixture(t)
		f.commit("chore: seed")
		f.tag("v1.2.1")
		f.commit("hotfix pushed straight to main")
		f.assertNext("1.2.1-1", "no merge since v1.2.1 → -1 suffix on the stable base")

		f.commit("another direct push")
		f.assertNext("1.2.1-2", "second direct push since the stable → -2")
	})

	t.Run("merge after direct push releases the next stable", func(t *testing.T) {
		f := newFixture(t)
		f.commit("chore: seed")
		f.tag("v1.2.1")
		f.commit("hotfix pushed straight to main")
		f.commit("feat: reviewed work (#758)")
		f.assertNext("1.2.2", "a merged PR since v1.2.1 → next stable; the direct commit rides along")
	})

	t.Run("newest prerelease tag anchors the frozen base", func(t *testing.T) {
		f := newFixture(t)
		f.commit("chore: seed")
		f.tag("v1.2.0-99")
		f.commit("fix: a (#100)")
		f.tag("v1.2.0-100")
		f.commit("fix: b (#101)")
		f.assertNext("1.2.1", "counting starts at the NEWEST tag (numeric build order, not lexical)")
	})

	t.Run("stable beats newer-numbered prerelease base", func(t *testing.T) {
		f := newFixture(t)
		f.commit("chore: seed")
		f.tag("v1.2.0-400")
		f.commit("fix: a (#100)")
		f.tag("v1.2.1")
		f.commit("fix: b (#101)")
		f.assertNext("1.2.2", "v1.2.1 stable is the source; the older -400 tag is ignored")
	})

	t.Run("rerun on the same head is idempotent", func(t *testing.T) {
		f := newFixture(t)
		f.commit("chore: seed")
		f.tag("v1.2.1")
		f.commit("feat: a (#756)")
		f.tag("v1.2.2")
		f.assertNext("1.2.2", "the release run re-executing on the tagged head recomputes the same version")
	})
}
