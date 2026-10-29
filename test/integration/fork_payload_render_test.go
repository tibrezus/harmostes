//go:build integration

// fork payload posture (three-tier 3a #789 → phase 4 #792): WHO renders the
// fork-maintenance engine ConfigMaps. Since phase 4 the chart NEVER renders
// them — the template repo (tibrezus/harmostes-template-fork-maintenance:
// scripts/checks/hooks/skill) and the config tier (defs) are the ONLY
// writers; the chart renders ONLY the consumers: pool mounts,
// HARMOSTES_EXTRA_CONFIGMAP_MOUNTS, the resolver mounts. The payload key is
// transition compatibility: external (or empty) passes; anything else —
// including the removed chart mode — fails the render naming it (#504).
package integration

import (
	"os/exec"
	"strings"
	"testing"
)

func helmPathFM(t *testing.T) string {
	t.Helper()
	for _, c := range []string{"/tmp/linux-amd64/helm", "/tmp/helm", "helm"} {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	t.Skip("helm not on PATH (CI installs the pinned version)")
	return ""
}

// renderFM renders the chart over golden-values with an extra --set and
// returns the raw manifest.
func renderFM(t *testing.T, set string) (string, error) {
	t.Helper()
	args := []string{"template", "harmostes", "../../chart", "-f", "../../chart/ci/golden-values.yaml", "--set", "namespace=harmostes-ci"}
	if set != "" {
		args = append(args, "--set", set)
	}
	out, err := exec.Command(helmPathFM(t), args...).CombinedOutput()
	return string(out), err
}

// configMapCount counts rendered ConfigMap documents whose metadata.name is
// exactly one of the engine payload names (the chart's render, not a mount
// reference — mounts appear as `name: fork-maintenance-scripts` inside
// volumes too, so count only lines under a ConfigMap kind header).
func configMapCount(render string) int {
	n := 0
	docs := strings.Split(render, "\n---")
	for _, doc := range docs {
		if !strings.Contains(doc, "kind: ConfigMap") {
			continue
		}
		for _, line := range strings.Split(doc, "\n") {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "name: fork-maintenance-") {
				n++
			}
		}
	}
	return n
}

// The chart never renders the engine — with the golden fixture (enabled)
// and the DEFAULT payload (external since phase 4).
func TestForkMaintenancePhase4RendersZeroEngineConfigMaps(t *testing.T) {
	out, err := renderFM(t, "")
	if err != nil {
		t.Fatalf("helm template (default): %v", err)
	}
	if got := configMapCount(out); got != 0 {
		t.Fatalf("phase 4 default renders %d engine ConfigMaps, want 0 — the template repo + config tier are the ONLY writers; a chart render here is the two-writer thrash class", got)
	}
	// The consumer side must survive: the pool's mounts and the env that
	// carries them to per-Attempt Jobs (#311 — pool-only mounts must reach
	// the jobs or pool-only plugins exec against missing files).
	if !strings.Contains(out, "HARMOSTES_EXTRA_CONFIGMAP_MOUNTS") {
		t.Fatalf("phase 4: HARMOSTES_EXTRA_CONFIGMAP_MOUNTS missing — the consumer side is unconditional")
	}
	for _, vol := range []string{"fork-scripts", "fork-checks", "fork-defs", "fork-hooks"} {
		if !strings.Contains(out, "name: "+vol+"\n") {
			t.Fatalf("phase 4: pool volume %q missing — the engine ConfigMaps are name-mounted, not gated on who renders them", vol)
		}
	}
}

// payload=chart is REMOVED (#792): the render refuses and points at the
// new home — a stale values file silently rendering the old posture is
// worse than a failed deploy (#504).
func TestForkMaintenancePayloadChartRemovedFailsLoud(t *testing.T) {
	out, err := renderFM(t, "forkMaintenance.payload=chart")
	if err == nil {
		t.Fatalf("payload=chart: helm template SUCCEEDED — the removed-mode guard did not fire; output:\n%s", out)
	}
	if !strings.Contains(out, "must be 'external' (or empty) — chart payload mode was REMOVED") {
		t.Fatalf("payload=chart: refusal does not name the removal or the new home; output:\n%s", out)
	}
	if !strings.Contains(out, "harmostes-template-fork-maintenance") {
		t.Fatalf("payload=chart: refusal does not name the engine's home repo; output:\n%s", out)
	}
}

// A typo'd posture still fails loud (#504).
func TestForkMaintenancePayloadUnknownValueFailsLoud(t *testing.T) {
	out, err := renderFM(t, "forkMaintenance.payload=chartrepo")
	if err == nil {
		t.Fatalf("payload=chartrepo: helm template SUCCEEDED — the render-time guard did not fire; output:\n%s", out)
	}
	if !strings.Contains(out, "forkMaintenance.payload must be 'external' (or empty)") {
		t.Fatalf("payload=chartrepo: refusal does not name the contract; output:\n%s", out)
	}
}

// Explicit external (what both deployments set at their flips) passes clean.
func TestForkMaintenancePayloadExternalExplicitPasses(t *testing.T) {
	out, err := renderFM(t, "forkMaintenance.payload=external")
	if err != nil {
		t.Fatalf("helm template (external): %v", err)
	}
	if got := configMapCount(out); got != 0 {
		t.Fatalf("payload=external renders %d engine ConfigMaps, want 0", got)
	}
}
