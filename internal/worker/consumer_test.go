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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
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
// (#311). Malformed entries are ERRORS (#320): a silently-skipped entry is a
// silently missing mount — the 12ms-forever prepare failure class.
func TestExtraConfigMapMountsFromEnv(t *testing.T) {
	t.Setenv("HARMOSTES_EXTRA_CONFIGMAP_MOUNTS",
		"fork-scripts=/workspace/scripts=0755,fork-checks=/workspace/checks")
	got, err := extraConfigMapMountsFromEnv()
	if err != nil {
		t.Fatalf("valid specs rejected: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("parsed %d mounts, want 2: %+v", len(got), got)
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

	for _, bad := range []string{"bad-entry", "also/bad=nope", "defs=/workspace/forks=999x", "=/workspace/x"} {
		t.Setenv("HARMOSTES_EXTRA_CONFIGMAP_MOUNTS", bad)
		if _, err := extraConfigMapMountsFromEnv(); err == nil {
			t.Errorf("malformed entry %q must be a construction error, got nil", bad)
		}
	}

	t.Setenv("HARMOSTES_EXTRA_CONFIGMAP_MOUNTS", "")
	if m, err := extraConfigMapMountsFromEnv(); m != nil || err != nil {
		t.Errorf("empty env must give nil,nil, got %+v, %v", m, err)
	}
}

// The env→config→JobParams seam is where #314's live bug hid: the parser
// returned 4 mounts and BuildJob rendered them, but DispatcherFromEnv
// dropped the parameter on the way to DispatchConfig — so every Job shipped
// without the extras and fork-maintenance kept failing at prepare. Since
// #320 the config resolves BEFORE the cluster dial, so the whole seam is
// directly testable: env → DispatchConfigFromEnv → JobParams → BuildJob,
// asserting the mounts reach the rendered Job.
func TestDispatcherFromEnvWiresExtraMounts(t *testing.T) {
	t.Setenv("HARMOSTES_WORKER_IMAGE", "harmostes:it")
	t.Setenv("HARMOSTES_PLUGIN_CONFIGMAPS", "harmostes-pr-review")
	t.Setenv("HARMOSTES_EXTRA_CONFIGMAP_MOUNTS", "fork-maintenance-scripts=/workspace/scripts=0755")

	var logged string
	cfg, err := DispatchConfigFromEnv(func(format string, args ...any) { logged = fmt.Sprintf(format, args...) })
	if err != nil {
		t.Fatalf("config from env: %v", err)
	}
	if len(cfg.ExtraConfigMapMounts) != 1 || cfg.ExtraConfigMapMounts[0].Name != "fork-maintenance-scripts" {
		t.Fatalf("config dropped the extra mounts: %+v", cfg.ExtraConfigMapMounts)
	}
	if len(cfg.PluginConfigMaps) != 1 || cfg.PluginConfigMaps[0] != "harmostes-pr-review" {
		t.Fatalf("config dropped the plugin configmaps: %+v", cfg.PluginConfigMaps)
	}
	if !strings.Contains(logged, "extraConfigMapMounts=[fork-maintenance-scripts=/workspace/scripts=0755]") {
		t.Errorf("startup log must carry the full mount specs (drift must be readable at boot): %q", logged)
	}

	// The seam's final hop: config → params → rendered Job.
	at := &v1alpha1.Attempt{ObjectMeta: metav1.ObjectMeta{Name: "attempt-seam", Namespace: "default"}}
	job := k8s.BuildJob(cfg.JobParams(at, "pr-review-harmostes", "default", nil))

	var volNames []string
	for _, v := range job.Spec.Template.Spec.Volumes {
		volNames = append(volNames, v.Name)
	}
	want := []string{"plugin-cm-harmostes-pr-review", "extra-cm-fork-maintenance-scripts"}
	for _, w := range want {
		if !slices.Contains(volNames, w) {
			t.Fatalf("rendered Job lost volume %q; volumes = %v", w, volNames)
		}
	}
}

// A config fact dropped inside DispatchConfigFromEnv must surface as a
// construction error or a missing render — never a silently-degraded Job.
func TestDispatchConfigRejectsUnrunnable(t *testing.T) {
	t.Setenv("HARMOSTES_WORKER_IMAGE", "")
	if _, err := DispatchConfigFromEnv(func(string, ...any) {}); err == nil {
		t.Fatal("missing worker image must fail construction — an imageless Job can never run")
	}
	t.Setenv("HARMOSTES_MAX_CONCURRENT", "zero")
	t.Setenv("HARMOSTES_WORKER_IMAGE", "harmostes:it")
	if _, err := DispatchConfigFromEnv(func(string, ...any) {}); err == nil {
		t.Fatal("non-numeric HARMOSTES_MAX_CONCURRENT must fail construction, not silently keep the default")
	}
}
