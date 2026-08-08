package ui

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// tokenMetric holds the query configuration for one token category.
type tokenMetric struct {
	name  string // metric name
	label string // display label
	color string // CSS color variable name (resolved client-side)
}

// handleMetricsView renders the metrics dashboard for the selected workflow.
func (s *Server) handleMetricsView(w http.ResponseWriter, r *http.Request) {
	workflowName := r.URL.Query().Get("workflow")
	timeRange := r.URL.Query().Get("range")
	if timeRange == "" {
		timeRange = "1h"
	}

	configured := s.signoz != nil

	s.render(w, r, "pages/metrics.html", map[string]any{
		"WorkflowName": workflowName,
		"TimeRange":    timeRange,
		"Configured":   configured,
	})
}

// handleMetricsAPI returns token usage metrics as JSON for client-side
// SVG histogram rendering.
func (s *Server) handleMetricsAPI(w http.ResponseWriter, r *http.Request) {
	if s.signoz == nil {
		s.writeAPIError(w, http.StatusServiceUnavailable, "metrics not configured (set SIGNOZ_URL + SIGNOZ_API_KEY)")
		return
	}

	workflow := r.URL.Query().Get("workflow")
	timeRange := r.URL.Query().Get("range")
	if timeRange == "" {
		timeRange = "1h"
	}

	dur, err := parseTimeRange(timeRange)
	if err != nil {
		s.writeAPIError(w, http.StatusBadRequest, "invalid time range: %s", timeRange)
		return
	}

	now := time.Now().UTC()
	start := now.Add(-dur)

	step := time.Minute * 5
	if dur >= 6*time.Hour {
		step = time.Minute * 15
	} else if dur >= 24*time.Hour {
		step = time.Minute * 60
	}

	ctx := r.Context()

	metrics := []tokenMetric{
		{name: "harmostes_agent_input_tokens_total", label: "Input", color: "--color-accent-gold"},
		{name: "harmostes_agent_output_tokens_total", label: "Output", color: "--color-next-teal"},
		{name: "harmostes_agent_cache_read_tokens_total", label: "Cache Read", color: "--fg-muted"},
	}

	type metricResponse struct {
		Label  string        `json:"label"`
		Color  string        `json:"color"`
		Points []MetricPoint `json:"points"`
	}

	var response struct {
		Workflow string           `json:"workflow"`
		Range    string           `json:"range"`
		Series   []metricResponse `json:"series"`
	}
	response.Workflow = workflow
	response.Range = timeRange

	for _, m := range metrics {
		query := fmt.Sprintf("sum(value:metric:%s{%s})", m.name, workflowAttr(workflow))
		series, err := s.signoz.QueryRange(ctx, query, start, now, step)
		if err != nil {
			s.logger.Warn("signoz query failed", "metric", m.name, "err", err)
			response.Series = append(response.Series, metricResponse{
				Label: m.label,
				Color: m.color,
			})
			continue
		}
		var points []MetricPoint
		if len(series) > 0 {
			points = series[0].Points
		}
		response.Series = append(response.Series, metricResponse{
			Label:  m.label,
			Color:  m.color,
			Points: points,
		})
	}

	s.writeJSON(w, http.StatusOK, response)
}

func workflowAttr(workflow string) string {
	if workflow == "" {
		return ""
	}
	return fmt.Sprintf("workflow=\"%s\"", workflow)
}

func parseTimeRange(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("too short")
	}
	val, err := strconv.Atoi(s[:len(s)-1])
	if err != nil {
		return 0, err
	}
	switch s[len(s)-1] {
	case 'm':
		return time.Duration(val) * time.Minute, nil
	case 'h':
		return time.Duration(val) * time.Hour, nil
	case 'd':
		return time.Duration(val) * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("unknown unit: %c", s[len(s)-1])
	}
}
