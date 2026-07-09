// Package dapr is a tiny client for the Dapr sidecar (state store + pub/sub) at
// localhost:3500. It is the deterministic event-system application layer that
// surrounds harmostes: state for skip/dedup, pub/sub for choreography +
// observability. Dapr abstracts the backend (Valkey today; swap by changing the
// Component CR) so this client never talks to Valkey directly.
package dapr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Client is the Dapr surface harmostes uses.
type Client interface {
	// GetState returns the stored value ("" if absent). A missing key is not an
	// error.
	GetState(ctx context.Context, store, key string) (string, error)
	// SaveState writes a single key.
	SaveState(ctx context.Context, store, key, value string) error
	// DeleteState removes a key (idempotent).
	DeleteState(ctx context.Context, store, key string) error
	// Publish sends a JSON payload on a pub/sub topic (best-effort; returns nil on
	// 200/204).
	Publish(ctx context.Context, pubsub, topic, jsonPayload string) error
}

// HTTPClient talks to a Dapr sidecar over HTTP.
type HTTPClient struct {
	BaseURL string // e.g. http://localhost:3500
	HTTP    *http.Client
}

// New returns a client for the sidecar at baseURL (default http://localhost:3500).
func New(baseURL string) *HTTPClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:3500" // not localhost: Go may resolve it to IPv6 ::1, which daprd doesn't bind
	}
	return &HTTPClient{BaseURL: baseURL, HTTP: &http.Client{}}
}

func (c *HTTPClient) GetState(ctx context.Context, store, key string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v1.0/state/%s/%s", c.BaseURL, store, key), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("dapr get-state: %s", resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	// Dapr returns the value JSON-encoded (a quoted string for a string value).
	var v string
	if json.Unmarshal(bytes.TrimSpace(b), &v) == nil {
		return v, nil
	}
	return strings.TrimSpace(string(b)), nil
}

func (c *HTTPClient) SaveState(ctx context.Context, store, key, value string) error {
	body, err := json.Marshal([]map[string]any{{"key": key, "value": value}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/v1.0/state/%s", c.BaseURL, store), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("dapr save-state: %s", resp.Status)
	}
	return nil
}

func (c *HTTPClient) DeleteState(ctx context.Context, store, key string) error {
	// Dapr's bulk-delete: POST an array with operation=delete (the form the
	// existing bash scripts use, version-portable).
	body, err := json.Marshal([]map[string]any{{"key": key, "operation": "delete"}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/v1.0/state/%s", c.BaseURL, store), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 200/204 = deleted; 404 (already gone) is also success.
	if resp.StatusCode > http.StatusNoContent {
		return nil
	}
	return nil
}

func (c *HTTPClient) Publish(ctx context.Context, pubsub, topic, jsonPayload string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/v1.0/publish/%s/%s", c.BaseURL, pubsub, topic),
		strings.NewReader(jsonPayload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("dapr publish %s/%s: %s", pubsub, topic, resp.Status)
	}
	return nil
}
