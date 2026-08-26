// Package ui provides the Dapr client wrapper for harmostes-ui.
// This wraps the existing dapr.Client interface with UI-specific helpers
// for state, secrets, and pub/sub operations.
package ui

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tibrezus/harmostes/internal/dapr"
)

// DaprClient wraps the Dapr client with UI-specific operations.
type DaprClient interface {
	// State operations
	SaveState(ctx context.Context, key string, value any) error
	GetState(ctx context.Context, key string, value any) (bool, error)
	DeleteState(ctx context.Context, key string) error

	// GetStateFromStore reads from a specific Dapr state store component
	// (e.g. the worker's "statestore" for session transcripts).
	GetStateFromStore(ctx context.Context, store, key string, value any) (bool, error)

	// Secret operations (write-only via k8s Secrets API, read via Dapr)
	GetSecret(ctx context.Context, secretName, key string) (string, error)

	// Pub/sub operations
	PublishEvent(ctx context.Context, topic string, data any) error
}

// daprClient implements DaprClient using the internal dapr.Client.
type daprClient struct {
	client dapr.Client
	store  string // Dapr state store name (ui-state)
	pubsub string // Dapr pub/sub name (ui-pubsub)
}

// NewDaprClient creates a new Dapr client wrapper.
func NewDaprClient(client dapr.Client) DaprClient {
	return &daprClient{
		client: client,
		store:  "ui-state",
		pubsub: "ui-pubsub",
	}
}

// SaveState saves a value to the Dapr state store.
// The value is JSON-encoded before storage.
func (c *daprClient) SaveState(ctx context.Context, key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	if err := c.client.SaveState(ctx, c.store, key, string(data)); err != nil {
		return fmt.Errorf("save state %s: %w", key, err)
	}
	return nil
}

// GetState retrieves a value from the Dapr state store.
// The value is JSON-decoded into the provided pointer.
// Returns false if the key does not exist.
func (c *daprClient) GetState(ctx context.Context, key string, value any) (bool, error) {
	return c.GetStateFromStore(ctx, c.store, key, value)
}

// GetStateFromStore reads from a specific Dapr state store component.
func (c *daprClient) GetStateFromStore(ctx context.Context, store, key string, value any) (bool, error) {
	data, err := c.client.GetState(ctx, store, key)
	if err != nil {
		return false, fmt.Errorf("get state %s from %s: %w", key, store, err)
	}
	if data == "" {
		return false, nil
	}
	// data arrives with one JSON-string layer already removed by
	// HTTPClient.GetState (it unwraps JSON-string bodies), so values saved
	// as `string(json.Marshal(x))` land here as plain JSON text.
	if err := json.Unmarshal([]byte(data), value); err != nil {
		return false, fmt.Errorf("unmarshal state %s: %w", key, err)
	}
	return true, nil
}

// DeleteState removes a key from the Dapr state store.
func (c *daprClient) DeleteState(ctx context.Context, key string) error {
	if err := c.client.DeleteState(ctx, c.store, key); err != nil {
		return fmt.Errorf("delete state %s: %w", key, err)
	}
	return nil
}

// GetSecret retrieves a secret value from the k8s secret store via Dapr.
// Dapr's secret store component must be configured to read k8s secrets.
func (c *daprClient) GetSecret(ctx context.Context, secretName, key string) (string, error) {
	// Note: Dapr's secret store API is not yet exposed in dapr.Client
	// We'll implement this via direct k8s client access for now
	// This is a placeholder for the full Dapr secret store integration
	return "", fmt.Errorf("secret store not yet implemented via Dapr; use k8s Secrets API directly")
}

// PublishEvent publishes an event to the Dapr pub/sub.
// The data is JSON-encoded before publishing.
func (c *daprClient) PublishEvent(ctx context.Context, topic string, data any) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	if err := c.client.Publish(ctx, c.pubsub, topic, string(jsonData)); err != nil {
		return fmt.Errorf("publish event %s: %w", topic, err)
	}
	return nil
}
