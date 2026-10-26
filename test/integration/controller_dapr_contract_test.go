package integration

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The controller's dapr contract is ONE contract: the sidecar annotations
// (observability.controllerSidecar) and the #613 startup-guard requirement
// (HARMOSTES_DAPR_REQUIRED) must render together or not at all. Arming the
// requirement without the sidecar boot-loops the controller forever — the
// #613 guard treats a missing DAPR_HTTP_PORT as a misconfiguration and
// self-heal-deletes the pod (hit live on harmostes-dev with 1.5.0: pod
// deleted every ~30s, zero dispatches, because dev's values never set
// controllerSidecar while the requirement rendered unconditionally).
//
// This probe renders the chart under helm for BOTH gate states and asserts
// the pairing. Helm must be on PATH (CI installs the pinned version; locally
// /tmp/helm or any v4.2.x).
func TestControllerDaprRequiredMatchesSidecar(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		if _, serr := os.Stat("/tmp/helm"); serr == nil {
			helm = "/tmp/helm"
		} else {
			t.Skip("helm not on PATH (CI installs the pinned version)")
		}
	}

	type envEntry struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	type deployment struct {
		Spec struct {
			Template struct {
				Metadata struct {
					Annotations map[string]string `json:"annotations"`
				} `json:"metadata"`
				Spec struct {
					Containers []struct {
						Env []envEntry `json:"env"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}

	render := func(t *testing.T, set string) *deployment {
		t.Helper()
		// golden-values.yaml supplies the deployment-decision fields the
		// 10-25 contract marks `required` (pr-review model) — the probe
		// only varies the dapr gate.
		args := []string{"template", "harmostes", "../../chart", "-f", "../../chart/ci/golden-values.yaml", "--set", "namespace=harmostes"}
		if set != "" {
			args = append(args, "--set", set)
		}
		out, err := exec.Command(helm, args...).Output()
		if err != nil {
			t.Fatalf("helm template (set %q): %v", set, err)
		}
		for _, doc := range strings.Split(string(out), "\n---") {
			var d deployment
			if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
				continue
			}
			if strings.Contains(strings.ToLower(doc), "kind: deployment") && strings.Contains(doc, "controller") && d.Spec.Template.Spec.Containers != nil {
				for _, c := range d.Spec.Template.Spec.Containers {
					for _, e := range c.Env {
						if e.Name == "HARMOSTES_ATTEMPT_RETENTION" {
							return &d // the controller deployment (its env signature)
						}
					}
				}
			}
		}
		t.Fatalf("helm template (set %q): no controller deployment in render", set)
		return nil
	}

	assertPairing := func(t *testing.T, d *deployment, wantSidecar bool) {
		t.Helper()
		_, hasEnabled := d.Spec.Template.Metadata.Annotations["dapr.io/enabled"]
		required := false
		for _, c := range d.Spec.Template.Spec.Containers {
			for _, e := range c.Env {
				if e.Name == "HARMOSTES_DAPR_REQUIRED" {
					required = true
				}
			}
		}
		if hasEnabled != wantSidecar {
			t.Fatalf("dapr.io/enabled = %v, want %v (gate: observability.controllerSidecar)", hasEnabled, wantSidecar)
		}
		if required != wantSidecar {
			t.Fatalf("HARMOSTES_DAPR_REQUIRED = %v, want %v — the #613 requirement and the sidecar annotations are ONE contract; arming the requirement without the injection boot-loops the controller (dev 1.5.0 incident)", required, wantSidecar)
		}
	}

	t.Run("default (sidecar off): no annotations, no requirement", func(t *testing.T) {
		assertPairing(t, render(t, ""), false)
	})
	t.Run("controllerSidecar=true: annotations AND requirement", func(t *testing.T) {
		assertPairing(t, render(t, "observability.controllerSidecar=true"), true)
	})
}
