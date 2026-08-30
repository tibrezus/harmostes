package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
		RunFunc:    func(_ context.Context, _ RunRequest) error { return nil },
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
		RunFunc: func(_ context.Context, req RunRequest) error {
			atomic.AddInt32(&callCount, 1)
			if req.Workflow != "wiki-lint-harmostes" {
				t.Errorf("workflow = %q", req.Workflow)
			}
			if req.Namespace != "harmostes" {
				t.Errorf("namespace = %q", req.Namespace)
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
		RunFunc: func(_ context.Context, _ RunRequest) error {
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
		RunFunc: func(_ context.Context, _ RunRequest) error { return nil },
	})

	req := httptest.NewRequest(http.MethodPost, "/triggers", strings.NewReader("not json"))
	req.Body = http.MaxBytesReader(nil, req.Body, 1<<20)
	rr := httptest.NewRecorder()
	consumer.handleTrigger(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestConsumerTriggerEvent_EmptyWorkflow(t *testing.T) {
	// Dapr delivers a CloudEvent whose data is the raw TriggerEvent JSON.
	// If the workflow field is empty, the consumer should return 500 (not
	// crash) so Dapr can retry.
	var capturedWorkflow string
	consumer := NewConsumer(ConsumerConfig{
		RunFunc: func(_ context.Context, req RunRequest) error {
			capturedWorkflow = req.Workflow
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
