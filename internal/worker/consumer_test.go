package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestConsumerSubscribeEndpoint(t *testing.T) {
	consumer := NewConsumer(ConsumerConfig{
		HTTPPort:   "0", // unused — we test the handler directly
		PubsubName: "pubsub",
		Topic:      "harmostes-triggers",
		RunFunc:    func(_ context.Context, _, _, _, _, _, _, _, _ string) error { return nil },
	})

	req := httptest.NewRequest(http.MethodGet, "/dapr/subscribe", nil)
	rr := httptest.NewRecorder()
	consumer.handleSubscribe(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var subs []daprSubscription
	if err := json.NewDecoder(rr.Body).Decode(&subs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 subscription, got %d", len(subs))
	}
	if subs[0].PubsubName != "pubsub" {
		t.Errorf("pubsub = %q", subs[0].PubsubName)
	}
	if subs[0].Topic != "harmostes-triggers" {
		t.Errorf("topic = %q", subs[0].Topic)
	}
	if subs[0].Route != "/triggers" {
		t.Errorf("route = %q", subs[0].Route)
	}
}

func TestConsumerHealthz(t *testing.T) {
	// Healthz is a trivial 200 handler — test the pattern.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestConsumerTriggerEvent_ParsesCloudEvent(t *testing.T) {
	var callCount int32
	consumer := NewConsumer(ConsumerConfig{
		RunFunc: func(_ context.Context, workflow, namespace, _, _, _, _, _, _ string) error {
			atomic.AddInt32(&callCount, 1)
			if workflow != "wiki-lint-harmostes" {
				t.Errorf("workflow = %q", workflow)
			}
			if namespace != "harmostes" {
				t.Errorf("namespace = %q", namespace)
			}
			return nil
		},
	})

	// Build a CloudEvent matching the controller's publishTrigger format.
	cloudEvent := `{
		"specversion": "1.0",
		"type": "harmostes.trigger",
		"source": "harmostes-controller",
		"subject": "wiki-lint-harmostes",
		"id": "test-1",
		"time": "2026-08-09T12:00:00Z",
		"datacontenttype": "application/json",
		"data": {
			"workflow": "wiki-lint-harmostes",
			"namespace": "harmostes",
			"triggerType": "webhook",
			"revision": "abc123",
			"traceparent": "00-trace",
			"attemptName": "attempt-1"
		}
	}`

	req := httptest.NewRequest(http.MethodPost, "/triggers", strings.NewReader(cloudEvent))
	req.Body = http.MaxBytesReader(nil, req.Body, 1<<20)
	rr := httptest.NewRecorder()
	consumer.handleTrigger(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if atomic.LoadInt32(&callCount) != 1 {
		t.Errorf("RunFunc called %d times, want 1", callCount)
	}
}

func TestConsumerTriggerEvent_SingleFlight(t *testing.T) {
	// Simulate a slow RunFunc; a second concurrent trigger should get 503.
	block := make(chan struct{})
	consumer := NewConsumer(ConsumerConfig{
		RunFunc: func(_ context.Context, _, _, _, _, _, _, _, _ string) error {
			<-block // block until test releases
			return nil
		},
	})

	cloudEvent := `{
		"specversion": "1.0",
		"type": "harmostes.trigger",
		"source": "harmostes-controller",
		"subject": "test",
		"id": "test-1",
		"datacontenttype": "application/json",
		"data": {"workflow": "test", "namespace": "harmostes", "triggerType": "schedule"}
	}`

	// Start the first request in a goroutine.
	done := make(chan int, 2)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/triggers", strings.NewReader(cloudEvent))
		req.Body = http.MaxBytesReader(nil, req.Body, 1<<20)
		rr := httptest.NewRecorder()
		consumer.handleTrigger(rr, req)
		done <- rr.Code
	}()

	// Give the first request time to acquire the lock.
	time.Sleep(50 * time.Millisecond)

	// Second concurrent request should get 503.
	req2 := httptest.NewRequest(http.MethodPost, "/triggers", strings.NewReader(cloudEvent))
	req2.Body = http.MaxBytesReader(nil, req2.Body, 1<<20)
	rr2 := httptest.NewRecorder()
	consumer.handleTrigger(rr2, req2)

	if rr2.Code != http.StatusServiceUnavailable {
		t.Errorf("second request status = %d, want 503", rr2.Code)
	}

	// Release the first request.
	close(block)
	code := <-done
	if code != http.StatusOK {
		t.Errorf("first request status = %d, want 200", code)
	}
}

func TestConsumerTriggerEvent_InvalidJSON(t *testing.T) {
	consumer := NewConsumer(ConsumerConfig{
		RunFunc: func(_ context.Context, _, _, _, _, _, _, _, _ string) error { return nil },
	})

	req := httptest.NewRequest(http.MethodPost, "/triggers", strings.NewReader("not json"))
	req.Body = http.MaxBytesReader(nil, req.Body, 1<<20)
	rr := httptest.NewRecorder()
	consumer.handleTrigger(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestBuildChildEnv_ScrubsConsumerMode(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"HARMOSTES_CONSUMER_MODE=true",
		"HOME=/root",
	}
	env := buildChildEnv(parent, "wiki-lint-harmostes", "harmostes", "main", "attempt-1", "00-trace", "git.rezus.cloud/tibrez/rhesadox#42", "labeled", "wake123")
	if !slices.Contains(env, "HARMOSTES_TRIGGER_PR=git.rezus.cloud/tibrez/rhesadox#42") ||
		!slices.Contains(env, "HARMOSTES_TRIGGER_ACTION=labeled") ||
		!slices.Contains(env, "HARMOSTES_TRIGGER_REVISION=wake123") {
		t.Fatalf("wake env missing: %v", env)
	}

	// HARMOSTES_CONSUMER_MODE must NOT be present
	for _, e := range env {
		if strings.HasPrefix(e, "HARMOSTES_CONSUMER_MODE=") {
			t.Errorf("HARMOSTES_CONSUMER_MODE leaked into child env: %s", e)
		}
	}

	// Workflow vars must be present
	mustContain := map[string]bool{
		"HARMOSTES_WORKFLOW=wiki-lint-harmostes": false,
		"HARMOSTES_NAMESPACE=harmostes":          false,
		"HARMOSTES_SOURCE=main":                  false,
		"HARMOSTES_NO_DAPR_SHUTDOWN=true":        false,
		"HARMOSTES_ATTEMPT=attempt-1":            false,
		"HARMOSTES_TRACEPARENT=00-trace":         false,
		"PATH=/usr/bin":                          false,
		"HOME=/root":                             false,
	}
	for _, e := range env {
		if _, ok := mustContain[e]; ok {
			mustContain[e] = true
		}
	}
	for k, found := range mustContain {
		if !found {
			t.Errorf("missing expected env var: %s", k)
		}
	}
}

func TestBuildChildEnv_OmitsEmptyOptional(t *testing.T) {
	env := buildChildEnv([]string{"PATH=/usr/bin"}, "wf", "ns", "", "", "", "", "", "")

	for _, e := range env {
		if strings.HasPrefix(e, "HARMOSTES_ATTEMPT=") {
			t.Errorf("HARMOSTES_ATTEMPT should be omitted when empty, got: %s", e)
		}
		if strings.HasPrefix(e, "TRACEPARENT=") {
			t.Errorf("TRACEPARENT should be omitted when empty, got: %s", e)
		}
	}
}

func TestConsumerTriggerEvent_EmptyWorkflow(t *testing.T) {
	// Dapr delivers a CloudEvent whose data is the raw TriggerEvent JSON.
	// If the workflow field is empty, the consumer should return 500 (not
	// crash) so Dapr can retry.
	var capturedWorkflow string
	consumer := NewConsumer(ConsumerConfig{
		RunFunc: func(_ context.Context, workflow, _, _, _, _, _, _, _ string) error {
			capturedWorkflow = workflow
			return fmt.Errorf("simulated failure")
		},
	})

	// Dapr-generated CloudEvent with empty workflow. Source must be a valid URI.
	cloudEvent := `{"specversion":"1.0","id":"test","type":"com.dapr.event.sent","source":"harmostes-controller","data":{"workflow":"","namespace":"harmostes","triggerType":"schedule"}}`

	req := httptest.NewRequest(http.MethodPost, "/triggers", strings.NewReader(cloudEvent))
	req.Body = http.MaxBytesReader(nil, req.Body, 1<<20)
	rr := httptest.NewRecorder()
	consumer.handleTrigger(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
	if capturedWorkflow != "" {
		t.Errorf("workflow = %q, want empty", capturedWorkflow)
	}
}
