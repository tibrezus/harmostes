package controller

import (
	"maps"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
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
