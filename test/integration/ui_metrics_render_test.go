//go:build integration

package integration

// #525: the UI's /metrics endpoint (Prometheus format, served outside auth —
// metrics.go documents the in-cluster scraper as its intended reader) is
// ingested ONLY if the pod self-registers with the annotation-based
// kubernetes-pods receiver. Without these three annotations the UI's
// telemetry — including harmostes_ui_writes_total (#418) — is invisible:
// the UI is scrape-only (no OTel SDK push, unlike the controller). This
// pins the annotations against the committed golden render so a template
// edit cannot silently orphan the metric again.

import (
	"testing"
)

func TestGoldenUIMetricsScrapeAnnotations(t *testing.T) {
	var ui goldenResource
	for _, r := range loadGoldenResources(t) {
		if r.Kind == "Deployment" && r.Metadata["name"] == "harmostes-ui" {
			ui = r
			break
		}
	}
	if ui.Kind == "" {
		t.Fatal("golden render carries no harmostes-ui Deployment — template renamed?")
	}
	tmpl, _ := ui.Spec["template"].(map[string]any)
	meta, _ := tmpl["metadata"].(map[string]any)
	anns, _ := meta["annotations"].(map[string]any)
	want := map[string]string{
		"prometheus.io/scrape": "true",
		"prometheus.io/port":   "8083", // the UI's only server: app + /metrics
		"prometheus.io/path":   "/metrics",
	}
	for k, v := range want {
		got, ok := anns[k].(string)
		if !ok || got != v {
			t.Fatalf("UI pod annotation %s = %q (present=%v), want %q — without it the kubernetes-pods receiver never scrapes the UI and harmostes_ui_writes_total stays un ingested (#525)", k, got, ok, v)
		}
	}
}
