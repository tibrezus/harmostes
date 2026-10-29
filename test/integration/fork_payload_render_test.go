//go:build integration

// fork payload posture (three-tier 3a, #789): WHO renders the fork-maintenance
// engine ConfigMaps. payload=chart (default) → the chart, from its embedded
// copies — today's posture, byte-identical goldens. payload=external → the
// deployment brings them (template repo: scripts/checks/hooks/skill; config
// tier: defs) and the chart renders ONLY the consumers: pool mounts,
// HARMOSTES_EXTRA_CONFIGMAP_MOUNTS, the resolver mounts. The mounts must
// SURVIVE external mode — the knob moves the payload, never the consumer side.
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
	args := []string{"template", "harmostes", "../../chart", "-f", "../../chart/ci/golden-values.yaml", "--set", "namespace=harmostes"}
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

func TestForkMaintenancePayloadChartRendersTheEngine(t *testing.T) {
	out, err := renderFM(t, "")
	if err != nil {
		t.Fatalf("helm template (default): %v", err)
	}
	// scripts, checks, hooks, defs, skill — five payload ConfigMaps.
	if got := configMapCount(out); got != 5 {
		t.Fatalf("payload=chart (default) renders %d engine ConfigMaps, want 5 — the default posture must not change (inert knob, byte-identical goldens)", got)
	}
	if !strings.Contains(out, "HARMOSTES_EXTRA_CONFIGMAP_MOUNTS") {
		t.Fatalf("payload=chart: HARMOSTES_EXTRA_CONFIGMAP_MOUNTS missing — the attempt-Job projection is part of the consumer side")
	}
}

func TestForkMaintenancePayloadExternalSkipsRendersKeepsConsumers(t *testing.T) {
	out, err := renderFM(t, "forkMaintenance.payload=external")
	if err != nil {
		t.Fatalf("helm template (external): %v", err)
	}
	if got := configMapCount(out); got != 0 {
		t.Fatalf("payload=external renders %d engine ConfigMaps, want 0 — the deployment (template repo + config tier) is the ONLY writer in external mode; a chart render here is the two-writer thrash class", got)
	}
	// The consumer side must survive: the pool's mounts and the env that
	// carries them to per-Attempt Jobs (#311 — pool-only mounts must reach
	// the jobs or pool-only plugins exec against missing files).
	if !strings.Contains(out, "HARMOSTES_EXTRA_CONFIGMAP_MOUNTS") {
		t.Fatalf("payload=external: HARMOSTES_EXTRA_CONFIGMAP_MOUNTS missing — the knob moved the payload, not the consumers")
	}
	for _, vol := range []string{"fork-scripts", "fork-checks", "fork-defs", "fork-hooks"} {
		if !strings.Contains(out, "name: "+vol+"\n") {
			t.Fatalf("payload=external: pool volume %q missing — the engine ConfigMaps are name-mounted, not gated on who renders them", vol)
		}
	}
}

func TestForkMaintenancePayloadUnknownValueFailsLoud(t *testing.T) {
	// #504: a typo'd posture silently rendering the wrong thing is worse
	// than a failed deploy — the render refuses and names the value.
	out, err := renderFM(t, "forkMaintenance.payload=chartrepo")
	if err == nil {
		t.Fatalf("payload=chartrepo: helm template SUCCEEDED — the render-time guard did not fire; output:\n%s", out)
	}
	if !strings.Contains(out, "forkMaintenance.payload must be 'chart' or 'external'") {
		t.Fatalf("payload=chartrepo: refusal does not name the contract; output:\n%s", out)
	}
}
