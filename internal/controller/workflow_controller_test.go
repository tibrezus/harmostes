package controller

import (
	"maps"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// TestBuildDaprAnnotations covers the Dapr sidecar-annotation contract for
// worker Jobs: stock daprd (events/state only) vs the rezuscloud/dapr fork
// (OTLP push). It locks the annotation set the Dapr injector reads, so a
// regression that drops dapr.io/sidecar-image or the insecure env is caught.
func TestBuildDaprAnnotations(t *testing.T) {
	wf := &v1alpha1.Workflow{}
	wf.Name = "llm-wiki"

	const forkAMD64 = "ghcr.io/rezuscloud/daprd:otel-metrics-latest-amd64"

	tests := []struct {
		name string
		r    WorkflowReconciler
		want map[string]string
	}{
		{
			name: "dapr disabled yields no annotations",
			r:    WorkflowReconciler{DaprEnabled: false},
			want: map[string]string{},
		},
		{
			name: "stock daprd injects events/state only",
			r:    WorkflowReconciler{DaprEnabled: true},
			want: map[string]string{
				"dapr.io/enabled": "true",
				"dapr.io/app-id":  "harmostes-worker-llm-wiki",
				"dapr.io/config":  "harmostes-config",
			},
		},
		{
			name: "fork daprd insecure adds sidecar-image and OTLP_INSECURE env",
			r: WorkflowReconciler{
				DaprEnabled:  true,
				DaprdImage:   forkAMD64,
				OTLPInsecure: true,
			},
			want: map[string]string{
				"dapr.io/enabled":       "true",
				"dapr.io/app-id":        "harmostes-worker-llm-wiki",
				"dapr.io/config":        "harmostes-config",
				"dapr.io/sidecar-image": forkAMD64,
				"dapr.io/env":           "OTEL_EXPORTER_OTLP_INSECURE=true",
			},
		},
		{
			name: "fork daprd secure omits the insecure env",
			r: WorkflowReconciler{
				DaprEnabled: true,
				DaprdImage:  forkAMD64,
			},
			want: map[string]string{
				"dapr.io/enabled":       "true",
				"dapr.io/app-id":        "harmostes-worker-llm-wiki",
				"dapr.io/config":        "harmostes-config",
				"dapr.io/sidecar-image": forkAMD64,
			},
		},
		{
			name: "daprd-image flag ignored when dapr disabled",
			r: WorkflowReconciler{
				DaprEnabled: false,
				DaprdImage:  forkAMD64,
			},
			want: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.r.buildDaprAnnotations(wf)
			if !maps.Equal(got, tt.want) {
				t.Errorf("buildDaprAnnotations() =\n  %v\nwant\n  %v", got, tt.want)
			}
		})
	}
}

// envMap folds an EnvVar slice into a name→value map (for assertions; ValueFrom
// vars show an empty value, which the presence check tolerates).
func envMap(envs []corev1.EnvVar) map[string]string {
	m := make(map[string]string, len(envs))
	for _, e := range envs {
		m[e.Name] = e.Value
	}
	return m
}

// TestWorkerEnv locks the Phase 2 contract: the OTel exporter config is stamped on
// every worker Job so the worker's own pipeline spans + traceparent join are
// enabled (endpoint set) or cleanly disabled (empty), alongside the unchanged
// identity + token env.
func TestWorkerEnv(t *testing.T) {
	wf := &v1alpha1.Workflow{}
	wf.Name = "llm-wiki"
	wf.Namespace = "harmostes"
	const ep = "signoz-otel-collector.signoz.svc.cluster.local:4317"

	t.Run("otel enabled stamps endpoint + insecure", func(t *testing.T) {
		env := envMap(WorkflowReconciler{OTLPEndpoint: ep, OTLPInsecure: true}.workerEnv(wf))
		if env["OTEL_EXPORTER_OTLP_ENDPOINT"] != ep {
			t.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT = %q, want %q", env["OTEL_EXPORTER_OTLP_ENDPOINT"], ep)
		}
		if env["OTEL_EXPORTER_OTLP_INSECURE"] != "true" {
			t.Errorf("OTEL_EXPORTER_OTLP_INSECURE = %q, want true", env["OTEL_EXPORTER_OTLP_INSECURE"])
		}
	})

	t.Run("otel disabled when endpoint empty", func(t *testing.T) {
		env := envMap(WorkflowReconciler{}.workerEnv(wf))
		if env["OTEL_EXPORTER_OTLP_ENDPOINT"] != "" {
			t.Errorf("expected empty endpoint (disabled), got %q", env["OTEL_EXPORTER_OTLP_ENDPOINT"])
		}
		if env["OTEL_EXPORTER_OTLP_INSECURE"] != "false" {
			t.Errorf("OTEL_EXPORTER_OTLP_INSECURE = %q, want false", env["OTEL_EXPORTER_OTLP_INSECURE"])
		}
	})

	// identity + tokens are always present regardless of observability config.
	env := envMap(WorkflowReconciler{}.workerEnv(wf))
	for _, k := range []string{"HARMOSTES_WORKFLOW", "HARMOSTES_NAMESPACE", "HARMOSTES_WORKDIR", "DAPR_HTTP_ENDPOINT", "HARMOSTES_GIT_TOKEN", "LITELLM_API_KEY"} {
		if _, ok := env[k]; !ok {
			t.Errorf("missing required env var %q", k)
		}
	}
	if env["HARMOSTES_WORKFLOW"] != "llm-wiki" || env["HARMOSTES_NAMESPACE"] != "harmostes" {
		t.Errorf("identity env = workflow=%q ns=%q", env["HARMOSTES_WORKFLOW"], env["HARMOSTES_NAMESPACE"])
	}
}
