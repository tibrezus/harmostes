package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// mockDaprClient is a test double for dapr.Client that allows overriding behavior
type mockDaprClient struct {
	getStateFunc    func(context.Context, string, string) (string, error)
	saveStateFunc   func(context.Context, string, string, string) error
	deleteStateFunc func(context.Context, string, string) error
	publishFunc     func(context.Context, string, string, string) error
}

func (m *mockDaprClient) GetState(ctx context.Context, store, key string) (string, error) {
	if m.getStateFunc != nil {
		return m.getStateFunc(ctx, store, key)
	}
	return "", nil
}

func (m *mockDaprClient) SaveState(ctx context.Context, store, key, value string) error {
	if m.saveStateFunc != nil {
		return m.saveStateFunc(ctx, store, key, value)
	}
	return nil
}

func (m *mockDaprClient) DeleteState(ctx context.Context, store, key string) error {
	if m.deleteStateFunc != nil {
		return m.deleteStateFunc(ctx, store, key)
	}
	return nil
}

func (m *mockDaprClient) Publish(ctx context.Context, pubsub, topic, jsonPayload string) error {
	if m.publishFunc != nil {
		return m.publishFunc(ctx, pubsub, topic, jsonPayload)
	}
	return nil
}

func (m *mockDaprClient) GetSecret(_ context.Context, _, _ string) (map[string]string, error) {
	return nil, nil
}

func TestNewDaprClient(t *testing.T) {
	mock := &mockDaprClient{}
	client := NewDaprClient(mock)

	if client == nil {
		t.Error("NewDaprClient returned nil")
	}

	d, ok := client.(*daprClient)
	if !ok {
		t.Error("NewDaprClient did not return *daprClient")
	}
	if d.store != "ui-state" {
		t.Errorf("expected store ui-state, got %s", d.store)
	}
	if d.pubsub != "ui-pubsub" {
		t.Errorf("expected pubsub ui-pubsub, got %s", d.pubsub)
	}
}

func TestDaprClient_SaveState(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name      string
		key       string
		value     any
		saveError error
		wantErr   bool
	}{
		{
			name:  "save string",
			key:   "test:key",
			value: "test-value",
		},
		{
			name:  "save struct",
			key:   "test:struct",
			value: struct{ Name string }{Name: "test"},
		},
		{
			name:  "save map",
			key:   "test:map",
			value: map[string]any{"foo": "bar"},
		},
		{
			name:      "save error",
			key:       "test:error",
			value:     "test",
			saveError: fmt.Errorf("save failed"),
			wantErr:   true,
		},
		{
			name:    "marshal error",
			key:     "test:marshal",
			value:   make(chan int), // cannot marshal
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockDaprClient{
				saveStateFunc: func(ctx context.Context, store, key, value string) error {
					if tt.saveError != nil {
						return tt.saveError
					}
					return nil
				},
			}
			client := NewDaprClient(mock)

			err := client.SaveState(ctx, tt.key, tt.value)

			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
		})
	}
}

func TestDaprClient_GetState(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name     string
		key      string
		value    any
		data     string // Dapr response
		getError error
		wantErr  bool
		wantOK   bool
	}{
		{
			name:   "get string - found",
			key:    "test:key",
			value:  new(string),
			data:   `"test-value"`,
			wantOK: true,
		},
		{
			name:   "get struct - found",
			key:    "test:struct",
			value:  &struct{ Name string }{},
			data:   `{"Name":"test"}`,
			wantOK: true,
		},
		{
			name:   "get - not found",
			key:    "test:missing",
			value:  new(string),
			data:   "", // Dapr returns "" for missing keys
			wantOK: false,
		},
		{
			name:     "get error",
			key:      "test:error",
			value:    new(string),
			getError: fmt.Errorf("get failed"),
			wantErr:  true,
		},
		{
			name:    "unmarshal error",
			key:     "test:badjson",
			value:   new(string),
			data:    `{invalid json`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockDaprClient{
				getStateFunc: func(ctx context.Context, store, key string) (string, error) {
					if tt.getError != nil {
						return "", tt.getError
					}
					return tt.data, nil
				},
			}

			client := NewDaprClient(mock)
			ok, err := client.GetState(ctx, tt.key, tt.value)

			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if ok != tt.wantOK {
				t.Errorf("expected ok=%v, got %v", tt.wantOK, ok)
			}

			// If we got data, verify it was decoded
			if ok && tt.data != "" {
				switch v := tt.value.(type) {
				case *string:
					if *v != "test-value" {
						t.Errorf("expected value test-value, got %s", *v)
					}
				case *struct{ Name string }:
					if v.Name != "test" {
						t.Errorf("expected Name test, got %s", v.Name)
					}
				}
			}
		})
	}
}

func TestDaprClient_DeleteState(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name        string
		key         string
		deleteError error
		wantErr     bool
	}{
		{
			name: "delete success",
			key:  "test:key",
		},
		{
			name:        "delete error",
			key:         "test:error",
			deleteError: fmt.Errorf("delete failed"),
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockDaprClient{
				deleteStateFunc: func(ctx context.Context, store, key string) error {
					if tt.deleteError != nil {
						return tt.deleteError
					}
					return nil
				},
			}
			client := NewDaprClient(mock)

			err := client.DeleteState(ctx, tt.key)

			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
		})
	}
}

func TestDaprClient_PublishEvent(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name         string
		topic        string
		data         any
		publishError error
		wantErr      bool
	}{
		{
			name:  "publish string",
			topic: "test-topic",
			data:  "test-value",
		},
		{
			name:  "publish struct",
			topic: "test-topic",
			data:  struct{ Type string }{Type: "event"},
		},
		{
			name:  "publish map",
			topic: "test-topic",
			data:  map[string]any{"foo": "bar"},
		},
		{
			name:         "publish error",
			topic:        "test-topic",
			data:         "test",
			publishError: fmt.Errorf("publish failed"),
			wantErr:      true,
		},
		{
			name:    "marshal error",
			topic:   "test-topic",
			data:    make(chan int), // cannot marshal
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockDaprClient{
				publishFunc: func(ctx context.Context, pubsub, topic, jsonPayload string) error {
					if tt.publishError != nil {
						return tt.publishError
					}

					// Verify the data was JSON-encoded
					var decoded any
					if err := json.Unmarshal([]byte(jsonPayload), &decoded); err != nil {
						t.Errorf("data was not JSON-encoded: %v", err)
					}

					return nil
				},
			}
			client := NewDaprClient(mock)

			err := client.PublishEvent(ctx, tt.topic, tt.data)

			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
		})
	}
}

func TestDaprClient_GetSecret(t *testing.T) {
	ctx := context.Background()

	mock := &mockDaprClient{}
	client := NewDaprClient(mock)

	_, err := client.GetSecret(ctx, "test-secret", "test-key")
	if err == nil {
		t.Error("expected error for unimplemented GetSecret")
	}
	if err.Error() != "secret store not yet implemented via Dapr; use k8s Secrets API directly" {
		t.Errorf("unexpected error message: %v", err)
	}
}
