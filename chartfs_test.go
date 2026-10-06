package harmostes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The embedded chart must be complete enough for the fixture loader: a
// review environment (ui.fixture) extracts it in a distroless container
// where no chart directory exists — a missing CRD or values key surfaces
// there first, hundreds of miles from the tree that forgot it.
func TestExtractChartSeedsAFixtureWorld(t *testing.T) {
	dst := t.TempDir()
	if err := ExtractChart(dst); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "values.yaml")); err != nil {
		t.Fatalf("values.yaml missing: %v", err)
	}
	crds, err := os.ReadDir(filepath.Join(dst, "crds"))
	if err != nil || len(crds) == 0 {
		t.Fatalf("crds missing or empty (%v)", err)
	}
	// The values must carry the template the fixture world tracks — the
	// loadChartTemplate contract (fixture follows the chart, #436).
	b, err := os.ReadFile(filepath.Join(dst, "values.yaml"))
	if err != nil || !strings.Contains(string(b), "workflowTemplates:") {
		t.Fatalf("embedded values.yaml lacks workflowTemplates (%v)", err)
	}
}
