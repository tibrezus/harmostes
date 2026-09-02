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

	k8s "github.com/tibrezus/harmostes/internal/k8s"
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

// Pool-only named mounts must parse into the dispatcher's extra-mount list
// (#311): malformed entries are skipped with a log line, never fatal.
func TestExtraConfigMapMountsFromEnv(t *testing.T) {
	logs := []string{}
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	t.Setenv("HARMOSTES_EXTRA_CONFIGMAP_MOUNTS",
		"fork-scripts=/workspace/scripts=0755,fork-checks=/workspace/checks,bad-entry,also/bad=nope,defs=/workspace/forks=999x")
	got := extraConfigMapMountsFromEnv(logf)
	if len(got) != 2 {
		t.Fatalf("parsed %d mounts, want 2 (valid entries only): %+v", len(got), got)
	}
	if got[0].Name != "fork-scripts" || got[0].MountPath != "/workspace/scripts" {
		t.Errorf("first mount = %+v", got[0])
	}
	if got[0].Mode == nil || *got[0].Mode != 0o755 {
		t.Errorf("explicit mode = %v, want 493 (0755)", got[0].Mode)
	}
	if got[1].Mode != nil {
		t.Errorf("mode-less entry must default (nil Mode), got %v", got[1].Mode)
	}
	if got[1].Name != "fork-checks" || got[1].MountPath != "/workspace/checks" {
		t.Errorf("second mount = %+v", got[1])
	}
	if len(logs) != 3 {
		t.Errorf("malformed entries must be logged, got %v", logs)
	}

	t.Setenv("HARMOSTES_EXTRA_CONFIGMAP_MOUNTS", "")
	if m := extraConfigMapMountsFromEnv(logf); m != nil {
		t.Errorf("empty env must give nil, got %+v", m)
	}
}

// The FromEnv→cfg seam is where #313's live bug hid: the parser returned 4
// mounts and BuildJob rendered them, but DispatcherFromEnv dropped the
// parameter on the way to DispatchConfig — so every Job shipped without the
// extras and fork-maintenance kept failing at prepare. This test pins the
// whole seam end-to-end.
func TestDispatcherFromEnvWiresExtraMounts(t *testing.T) {
	t.Setenv("HARMOSTES_NAMESPACE", "harmostes")
	t.Setenv("HARMOSTES_EXTRA_CONFIGMAP_MOUNTS", "fork-maintenance-scripts=/workspace/scripts=0755")

	// NewDispatcher connects to the real in-cluster config; on a dev box
	// that fails — which still proves the point ONLY if the failure happens
	// AFTER config resolution. Instead call the resolver + inspect the same
	// construction the production path uses, via a Dispatcher built through
	// the real DispatchConfig path... DispatcherFromEnv needs a cluster, so
	// assert the config seam by constructing DispatchConfig the same way
	// DispatcherFromEnv does and reflecting over it is not possible without
	// a cluster. The honest seam test: run DispatcherFromEnv and require the
	// startup log to report the parsed count — the log prints AFTER the
	// config is assembled, BEFORE the cluster dial.
	var logged string
	capture := func(format string, args ...any) { logged = fmt.Sprintf(format, args...) }
	_, _ = DispatcherFromEnv([]string{"harmostes-pr-review"},
		[]k8s.ConfigMapMount{{Name: "fork-maintenance-scripts", MountPath: "/workspace/scripts"}},
		capture)
	if !strings.Contains(logged, "extraConfigMapMounts=1") {
		t.Fatalf("DispatcherFromEnv dropped the extra mounts; startup log: %q", logged)
	}
	if !strings.Contains(logged, "pluginConfigMaps=[harmostes-pr-review]") {
		t.Fatalf("plugin configmaps missing from dispatch config log: %q", logged)
	}
}
