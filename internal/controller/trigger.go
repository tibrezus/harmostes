package controller

import (
	"context"
	"encoding/json"
	"fmt"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
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
	Source      string `json:"source,omitempty"`
	TriggerType string `json:"triggerType"` // webhook | schedule | revision | manual | spec-change
	Traceparent string `json:"traceparent,omitempty"`
	AttemptName string `json:"attemptName,omitempty"`
	// PR + Action carry the pull_request wake (ADR-0006) to the worker: the
	// controller clears the trigger annotations at schedule time, so the
	// Review-Ready Gate receives its target through this payload.
	Pr      string `json:"pr,omitempty"`
	PrTitle string `json:"prTitle,omitempty"`
	Action  string `json:"action,omitempty"`
}

// publishTrigger publishes a trigger event to the Dapr pub/sub topic. The
// raw TriggerEvent JSON is published; Dapr wraps it in a CloudEvent and
// delivers it to the consumer. The topic defaults to "harmostes-triggers".
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

	payload := TriggerEvent{
		Workflow:    wf.Name,
		Namespace:   wf.Namespace,
		Revision:    revision,
		Source:      wf.Spec.Source.Revision,
		TriggerType: triggerType,
		Traceparent: traceparent,
		AttemptName: attemptName,
		Pr:          wf.Annotations["harmostes.dev/trigger-pr"],
		PrTitle:     wf.Annotations["harmostes.dev/trigger-title"],
		Action:      wf.Annotations["harmostes.dev/trigger-action"],
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal trigger event: %w", err)
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
