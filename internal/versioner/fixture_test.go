package versioner

import (
	"os/exec"
	"testing"
)

// newFixture creates a throwaway git repository with a deterministic identity.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{t: t, dir: dir}
	f.git("init", "-q", "-b", "main")
	f.git("config", "user.email", "test@rezus.cloud")
	f.git("config", "user.name", "versioner test")
	return f
}

func (f *fixture) git(args ...string) string {
	f.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", f.dir}, args...)...).CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// commit makes a direct commit on main (subjects ending "(#N)" model squash
// merges — the repo's only merge path; everything else models a direct push).
func (f *fixture) commit(subject string) {
	f.t.Helper()
	f.git("commit", "-q", "--allow-empty", "-m", subject)
}

func (f *fixture) tag(name string) {
	f.t.Helper()
	f.git("tag", name)
}

func (f *fixture) assertNext(want, why string) {
	f.t.Helper()
	got, err := Next(f.dir)
	if err != nil {
		f.t.Fatalf("Next(%s): %v", f.dir, err)
	}
	if got != want {
		f.t.Fatalf("Next = %q, want %q (%s)", got, want, why)
	}
}

func (f *fixture) assertPreviewBase(want, why string) {
	f.t.Helper()
	got, err := PreviewBase(f.dir)
	if err != nil {
		f.t.Fatalf("PreviewBase(%s): %v", f.dir, err)
	}
	if got != want {
		f.t.Fatalf("PreviewBase = %q, want %q (%s)", got, want, why)
	}
}
