package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/dapr"
)

// ---------------------------------------------------------------------------
// Trigger Publishing — the controller signals "this workflow is due" by
// publishing a CloudEvent to the Dapr pub/sub topic harmostes-triggers.
//
// The worker pool (a long-running Deployment with daprd) subscribes to this
// topic and consumes trigger events one at a time. This replaces the
// batchv1.Job-per-run model:
//
//   OLD: controller → createWorkerJob() → batchv1.Job → dead pod accumulates
//   NEW: controller → publishTrigger()  → Dapr pub/sub  → worker pool consumes
//
// Phase 1 runs both in parallel (feature flag). Phase 3 cuts over to pub/sub
// only and removes Job creation.
// ---------------------------------------------------------------------------

// TriggerEvent is the payload carried inside the CloudEvent data field. It
// carries everything the worker needs to execute the run without re-querying
// the controller: the workflow name, namespace, why it was triggered, and the
// W3C trace context for distributed tracing continuity.
type TriggerEvent struct {
	Workflow    string `json:"workflow"`
	Namespace   string `json:"namespace"`
	Revision    string `json:"revision,omitempty"`
	TriggerType string `json:"triggerType"` // webhook | schedule | revision | manual | spec-change
	Traceparent string `json:"traceparent,omitempty"`
	AttemptName string `json:"attemptName,omitempty"`
}

// publishTrigger publishes a trigger CloudEvent to the Dapr pub/sub topic.
// The topic defaults to "harmostes-triggers" but is configurable via
// r.TriggerTopic.
//
// This is best-effort: if the Dapr client is nil (Dapr disabled) or the
// publish fails, the error is logged but does not block reconciliation.
// The existing Job-creation path (Phase 1 parallel mode) is the fallback.
func (r *WorkflowReconciler) publishTrigger(ctx context.Context, wf *v1alpha1.Workflow, triggerType, revision, traceparent, attemptName string) error {
	if r.DaprClient == nil {
		return nil // Dapr not configured — skip (Job path handles it)
	}

	topic := r.TriggerTopic
	if topic == "" {
		topic = "harmostes-triggers"
	}

	event := cloudevents.NewEvent()
	event.SetType("harmostes.trigger")
	event.SetSource("harmostes-controller")
	event.SetSubject(wf.Name)
	event.SetTime(time.Now())
	event.SetID(fmt.Sprintf("%s-%d", wf.Name, time.Now().UnixNano()))

	payload := TriggerEvent{
		Workflow:    wf.Name,
		Namespace:   wf.Namespace,
		Revision:    revision,
		TriggerType: triggerType,
		Traceparent: traceparent,
		AttemptName: attemptName,
	}
	if err := event.SetData(cloudevents.ApplicationJSON, payload); err != nil {
		return fmt.Errorf("set cloud event data: %w", err)
	}

	b, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal cloud event: %w", err)
	}

	if err := r.DaprClient.Publish(ctx, "pubsub", topic, string(b)); err != nil {
		return fmt.Errorf("publish trigger to %s: %w", topic, err)
	}

	return nil
}

// triggerReason maps the isDue logic to a human-readable trigger type for
// the CloudEvent. This helps the worker and observability layer understand
// WHY a run was triggered.
func triggerReason(wf *v1alpha1.Workflow) string {
	if triggerRev := wf.Annotations["harmostes.dev/trigger-revision"]; triggerRev != "" {
		if triggerRev != wf.Status.LastProcessedRevision {
			return "webhook"
		}
	}
	if wf.Status.ObservedGeneration != wf.Generation {
		return "spec-change"
	}
	return "schedule"
}

// Ensure dapr.Client is referenced (the reconciler struct uses it).
var _ dapr.Client = (*dapr.HTTPClient)(nil)
