package ui

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// metricsRegistry collects the UI's own instrumentation, served at
// GET /metrics (outside auth — the scraper is in-cluster and holds no
// identity, same as the health probe). A dedicated registry rather than
// prometheus.DefaultRegisterer keeps the export explicit: what is registered
// here is exactly what this binary serves.
var metricsRegistry = prometheus.NewRegistry()

// writesTotal counts every mutating-route outcome by action and result —
// the observability floor #436 required BEFORE the lifecycle verbs re-armed
// (#418): rejections were log-only until now, and a write path that can
// fail silently in aggregate will. result is one of:
//
//	created   — the mutation succeeded (the verb's happy path)
//	forbidden — the identity/provenance/ownership gate said no (403s and
//	            foreign-owner 404s alike: the decision, not the wire code)
//	error     — everything else (validation, conflict, apiserver failure)
var writesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "harmostes_ui_writes_total",
		Help: "Workflow write actions by action and result (created|forbidden|error).",
	},
	[]string{"action", "result"},
)

func init() {
	metricsRegistry.MustRegister(writesTotal)
	// Pre-instantiate the full grid at zero. A CounterVec renders nothing
	// until a labeled child exists, so a fresh deployment's /metrics is
	// EMPTY — and "no series" is ambiguous between "no writes happened"
	// and "the metric is not scraped at all" (#525 live-learned: the
	// repaired scraper delivered, the metric stayed invisible until the
	// first write). A zeroed grid makes the scrape contract visible from
	// the first scrape and self-documents the vocabulary. The action set
	// mirrors the mutating routes (create + enable/disable/trigger/delete);
	// "created" is the verb's happy-path result whatever the verb.
	for _, action := range []string{"create", "enable", "disable", "trigger", "delete"} {
		for _, result := range []string{"created", "forbidden", "error"} {
			writesTotal.WithLabelValues(action, result)
		}
	}
}

// recordWrite increments the writes counter — the single funnel every
// mutating route reports through.
func recordWrite(action, result string) {
	writesTotal.WithLabelValues(action, result).Inc()
}

// handleMetrics serves the registry in Prometheus text format.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	promhttp.HandlerFor(metricsRegistry, promhttp.HandlerOpts{}).ServeHTTP(w, r)
}
