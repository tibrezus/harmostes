package ui

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// #525: the writes CounterVec must be pre-instantiated as a zeroed grid —
// a CounterVec with no children renders NOTHING, and an empty /metrics
// made "no writes" indistinguishable from "not scraped" until the first
// write landed (live: the repaired kubernetes-pods scrape delivered, the
// metric stayed invisible). The full action×result grid at 0 is the
// scrape contract.
func TestMetricsServesZeroedWritesGrid(t *testing.T) {
	s := newTestServerWithHub(t)
	rec := httptest.NewRecorder()
	s.handleMetrics(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, action := range []string{"create", "enable", "disable", "trigger", "delete"} {
		for _, result := range []string{"created", "forbidden", "error"} {
			// Prometheus text format: name{action="...",result="..."} 0
			// (quoting of label values is stable in the exposition format).
			series := `harmostes_ui_writes_total{action="` + action + `",result="` + result + `"}`
			if !strings.Contains(body, series) {
				t.Fatalf("missing zeroed series %s — CounterVec children must be pre-instantiated or /metrics renders empty (ambiguous with not-scraped)", series)
			}
		}
	}
}
