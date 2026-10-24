// Package versioner computes the next release version for main, replacing
// GitVersion (#755): GitVersion's continuous-deployment mode emitted
// <frozen-base>-<build-counter> (1.2.0-335 — 335 releases without the base
// ever moving, because its own prerelease-shaped tags can never advance the
// version source), which made the counter pure noise.
//
// The scheme is deterministic git arithmetic:
//
//   - a PR MERGED to main (a squash-merge commit — subject ends "(#N)")
//     bumps the PATCH: 1.2.0-335 → the next merge releases 1.2.1. Several
//     PRs merging between release runs collapse into one stable release
//     carrying them all — releases are content-addressed, not per-commit.
//   - a DIRECT push to main (no "(#N)" subject) increments only the -N
//     suffix on the last stable version (1.2.1-1, 1.2.1-2, …): these builds
//     publish but semver-order BELOW the stable they derive from. Safety
//     valve only — branch protection keeps direct pushes off main.
//
// Semver ordering carries the transition for free: 1.2.1 > 1.2.0-335, so
// every >= range resolving the old tags keeps resolving the new stables.
package versioner

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// squashMergeRe matches squash-merge subjects ("Title (#123)") — the repo's
// only merge path into main (linear history, squash merges).
var squashMergeRe = regexp.MustCompile(`\(#\d+\)$`)

type tag struct {
	major, minor, patch int
	pre                 string // prerelease suffix without the leading '-'; "" = stable
	ref                 string // original tag ref ("v1.2.0-335") for git ranges
	sha                 string
}

func (t tag) version() string {
	if t.pre == "" {
		return fmt.Sprintf("%d.%d.%d", t.major, t.minor, t.patch)
	}
	return fmt.Sprintf("%d.%d.%d-%s", t.major, t.minor, t.patch, t.pre)
}

// tagOrder sorts tags by version: base numerically, then stable above any
// prerelease of the same base, then prereleases among themselves (numeric
// when the suffix is a plain number — v1.2.0-100 must outrank v1.2.0-99,
// which lexical comparison would not do).
func tagOrder(a, b tag) int {
	if c := cmpInt(a.major, b.major); c != 0 {
		return c
	}
	if c := cmpInt(a.minor, b.minor); c != 0 {
		return c
	}
	if c := cmpInt(a.patch, b.patch); c != 0 {
		return c
	}
	switch {
	case a.pre == "" && b.pre == "":
		return 0
	case a.pre == "":
		return +1 // stable outranks prerelease
	case b.pre == "":
		return -1
	}
	an, aerr := strconv.Atoi(a.pre)
	bn, berr := strconv.Atoi(b.pre)
	switch {
	case aerr == nil && berr == nil:
		return cmpInt(an, bn)
	case aerr == nil:
		return +1 // numeric suffixes outrank alphanumeric
	case berr == nil:
		return -1
	default:
		return strings.Compare(a.pre, b.pre)
	}
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return +1
	default:
		return 0
	}
}

// Next returns the version the release pipeline must publish for HEAD of
// the repository at dir.
func Next(dir string) (string, error) {
	tags, err := readTags(dir)
	if err != nil {
		return "", err
	}

	var newest tag
	var haveNewest bool
	for _, tg := range tags {
		if !haveNewest || tagOrder(tg, newest) > 0 {
			newest, haveNewest = tg, true
		}
	}

	// No tags at all: 0.0.0 is the implicit base and everything since the
	// beginning of history counts.
	base := tag{}
	windowFrom := ""
	if haveNewest {
		base = tag{major: newest.major, minor: newest.minor, patch: newest.patch}
		windowFrom = newest.ref + "^{commit}"
	}

	subjects, err := logSubjects(dir, windowFrom)
	if err != nil {
		return "", err
	}

	merges, directs := 0, 0
	for _, s := range subjects {
		if squashMergeRe.MatchString(s) {
			merges++
		} else {
			directs++
		}
	}

	switch {
	case merges > 0:
		// One patch per merged PR; direct commits in the same window ride
		// along (they are already in the tree the release publishes).
		return fmt.Sprintf("%d.%d.%d", base.major, base.minor, base.patch+merges), nil
	case directs > 0:
		// Direct pushes only increment the suffix, counted against the
		// stable base so recomputation is idempotent per push.
		return fmt.Sprintf("%d.%d.%d-%d", base.major, base.minor, base.patch, directs), nil
	default:
		// HEAD is exactly at the newest tag (release rerun / force republish).
		return base.version(), nil
	}
}

// PreviewBase returns the mainline version an open PR is a candidate for:
// the governing stable base, one patch ahead. Preview artifacts self-report
// <base>-pr<N>.<sha8> (e.g. 1.2.1-pr719.a1b2c3d4) so a preview's version
// carries the mainline it rides instead of a frozen 0.0.0 (#755).
func PreviewBase(dir string) (string, error) {
	tags, err := readTags(dir)
	if err != nil {
		return "", err
	}
	var base tag
	var haveBase bool
	for _, tg := range tags {
		if !haveBase || tagOrder(tg, base) > 0 {
			base, haveBase = tg, true
		}
	}
	if !haveBase {
		return "0.0.1", nil
	}
	return fmt.Sprintf("%d.%d.%d", base.major, base.minor, base.patch+1), nil
}

func readTags(dir string) ([]tag, error) {
	out, err := git(dir, "tag", "--list", "--format=%(refname:short) %(objectname)")
	if err != nil {
		return nil, err
	}
	var tags []tag
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		tg, err := parseTag(parts[0], parts[1])
		if err != nil {
			continue // non-release tags are ignored
		}
		tags = append(tags, tg)
	}
	return tags, nil
}

func parseTag(name, sha string) (tag, error) {
	s := strings.TrimPrefix(name, "v")
	major, s, ok := cutNum(s, ".")
	if !ok {
		return tag{}, fmt.Errorf("not a release tag: %s", name)
	}
	minor, s, ok := cutNum(s, ".")
	if !ok {
		return tag{}, fmt.Errorf("not a release tag: %s", name)
	}
	patch, s, ok := cutNum(s, "")
	if !ok {
		return tag{}, fmt.Errorf("not a release tag: %s", name)
	}
	if s != "" && s[0] != '-' {
		return tag{}, fmt.Errorf("not a release tag: %s", name)
	}
	return tag{major: major, minor: minor, patch: patch, pre: strings.TrimPrefix(s, "-"), ref: name, sha: sha}, nil
}

func cutNum(s, sep string) (int, string, bool) {
	var i int
	if sep == "" {
		i = len(s)
		for j, r := range s {
			if r < '0' || r > '9' {
				i = j
				break
			}
		}
	} else {
		i = strings.Index(s, sep)
		if i < 0 {
			return 0, "", false
		}
	}
	n, err := strconv.Atoi(s[:i])
	if err != nil || i == 0 {
		return 0, "", false
	}
	if sep == "" {
		return n, s[i:], true
	}
	return n, s[i+1:], true
}

func logSubjects(dir, from string) ([]string, error) {
	args := []string{"log", "--format=%s"}
	if from != "" {
		args = append(args, from+"..HEAD")
	} else {
		args = append(args, "HEAD")
	}
	out, err := git(dir, args...)
	if err != nil {
		return nil, err
	}
	var subs []string
	for _, s := range strings.Split(strings.TrimSpace(out), "\n") {
		if s != "" {
			subs = append(subs, s)
		}
	}
	return subs, nil
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	// Bot-token boundary (#480 r10 t20): the child env is enumerated, never
	// inherited — a git read needs nothing but PATH, and an inherited env
	// would carry whatever credentials the parent holds.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %v: %v: %s", args, err, ee.Stderr)
		}
		return "", fmt.Errorf("git %v: %v", args, err)
	}
	return string(out), nil
}
