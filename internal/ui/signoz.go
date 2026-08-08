package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// SignozClient queries the SigNoz API for OTel metrics (token usage, durations,
// cost). It is nil-safe: when not configured (URL or API key empty), metric
// endpoints return graceful "not configured" responses.
type SignozClient struct {
	baseURL string // e.g. http://signoz.signoz.svc.cluster.local:8080
	apiKey  string // SIGNOZ-API-KEY value
	http    *http.Client
}

// NewSignozClient creates a SigNoz API client. Returns nil if either URL or
// API key is empty (metrics disabled).
func NewSignozClient(baseURL, apiKey string) *SignozClient {
	if baseURL == "" || apiKey == "" {
		return nil
	}
	return &SignozClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

// signozQueryResult is the response from /api/v4/query_range.
type signozQueryResult struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Values [][]any           `json:"values"` // [timestamp, "value_string"]
		} `json:"result"`
	} `json:"data"`
	Error string `json:"error"`
}

// MetricSeries is one metric time series: a label set + (timestamp, value) pairs.
type MetricSeries struct {
	Labels map[string]string `json:"labels"`
	Points []MetricPoint     `json:"points"`
}

// MetricPoint is a single timestamp + value pair.
type MetricPoint struct {
	Timestamp time.Time `json:"timestamp"`
	Value     float64   `json:"value"`
}

// QueryRange runs a PromQL query over a time range with the given
// step interval. Uses the Prometheus-compatible /api/v1/query_range endpoint.
func (c *SignozClient) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]MetricSeries, error) {
	if c == nil {
		return nil, fmt.Errorf("signoz client not configured")
	}

	params := url.Values{}
	params.Set("start", strconv.FormatInt(start.Unix(), 10))
	params.Set("end", strconv.FormatInt(end.Unix(), 10))
	params.Set("step", fmt.Sprintf("%.0fs", step.Seconds()))
	params.Set("query", query)

	reqURL := c.baseURL + "/api/v1/query_range?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("SIGNOZ-API-KEY", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query signoz: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("signoz returned %d: %s", resp.StatusCode, string(body))
	}

	var result signozQueryResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode signoz response: %w", err)
	}

	if result.Status == "error" {
		return nil, fmt.Errorf("signoz query error: %s", result.Error)
	}

	// Convert to MetricSeries
	var series []MetricSeries
	for _, r := range result.Data.Result {
		s := MetricSeries{Labels: r.Metric}
		for _, v := range r.Values {
			if len(v) != 2 {
				continue
			}
			tsNanos, ok := v[0].(float64)
			if !ok {
				continue
			}
			valStr, ok := v[1].(string)
			if !ok {
				continue
			}
			val, err := strconv.ParseFloat(valStr, 64)
			if err != nil {
				continue
			}
			s.Points = append(s.Points, MetricPoint{
				Timestamp: time.Unix(0, int64(tsNanos)),
				Value:     val,
			})
		}
		series = append(series, s)
	}

	return series, nil
}
