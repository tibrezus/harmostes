package ui

// progress.go — the shared read rule for the Attempt's live Progress
// window (#604/#608). Every surface that shows the executing run — the
// wall row, the runs page's in-flight row, the run-detail waterfall —
// reads it through here, so ONE staleness decision governs all of them:
// a crashed run's last write shows a dash, never a lie.

import (
	"time"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// liveProgressOf returns the attempt's Progress sample when it is fresh
// (wallProgressFreshness) and carries usage; nil otherwise (callers render
// their honest fallback). The wall's freshness constant is the console-wide
// bound — it moved here with the shared reader.
func liveProgressOf(att *v1alpha1.Attempt) *v1alpha1.RunProgress {
	if att == nil {
		return nil
	}
	p := att.Status.Progress
	if p == nil || p.UpdatedAt.IsZero() || time.Since(p.UpdatedAt.Time) > wallProgressFreshness {
		return nil
	}
	if p.TokensIn+p.TokensOut == 0 {
		return nil
	}
	return p
}
