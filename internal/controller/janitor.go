package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tibrezus/harmostes/internal/attempt"
)

// The janitor (#651): namespace-wide attempt-fleet hygiene, carried by the
// controller as a manager Runnable. Three best-effort passes per tick:
//
//  1. ReapStuckAttempts — reconciling attempts past the bound (worker loss
//     debris; releases the claim first when one rides along).
//  2. GCAttempts — retention GC for terminal/statusless attempts (#385).
//  3. Orphaned runner pods — Completed attempt-runner pods whose Job is
//     gone. The Job-side TTL (worker.job.ttlSecondsAfterFinished) and the
//     TTL-after-finished controller delete the JOB, but pod GC is a
//     separate controller and demonstrably misses (two Completed pods
//     outlived their deleted Job by 4 days on the live fleet, 2026-10-06);
//     this leg is the deterministic bound that does not depend on another
//     controller's propagation ordering.
//
// Why here and not in the gate sweep (where r30/#376 first put it): the
// sweep's population is review-gate workflows, so its janitor was scoped
// to one workflow per sweep — fork-maintenance templates never entered it,
// and 27 of their claims sat reconciling for 13–15 days. Cleanup is
// workflow-AGNOSTIC kernel machinery: it must not depend on whether some
// pr-review sweep happens to fire (#651 AC 1/2).
type Janitor struct {
	client.Client
	Namespace string
	// Every is the pass cadence; <=0 means the 30m default (the old
	// janitorEvery — #629's cadence reasoning carries over: the passes'
	// full Lists are the largest API cost, bounded by the retention
	// horizon).
	Every time.Duration
	// ReapAfter bounds reconciling age; <=0 means 7d (the r30 bound).
	ReapAfter time.Duration
	// Retention is the #385 GC horizon; <=0 means the 720h default —
	// GC cannot be disabled, the knob tunes the horizon.
	Retention time.Duration
}

// DefaultJanitorEvery is the pass cadence for a zero/negative Every.
const DefaultJanitorEvery = 30 * time.Minute

// Start runs the janitor until ctx is cancelled: one pass immediately (a
// restarted controller must not skip the first window — the worker's
// janitorDue carried the same rule), then one per tick. Ticker semantics
// make the passes single-flight: a pass overrunning its cadence delays the
// next one instead of stacking.
func (j *Janitor) Start(ctx context.Context) error {
	every := j.Every
	if every <= 0 {
		every = DefaultJanitorEvery
	}
	log := ctrl.Log.WithName("janitor")
	j.pass(ctx, log)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			j.pass(ctx, log)
		}
	}
}

func (j *Janitor) pass(ctx context.Context, log logr.Logger) {
	reapAfter := j.ReapAfter
	if reapAfter <= 0 {
		reapAfter = 7 * 24 * time.Hour
	}
	retention := j.Retention
	if retention <= 0 {
		retention = 720 * time.Hour
	}
	if n, err := attempt.ReapStuckAttempts(ctx, j.Client, j.Namespace, reapAfter); err != nil {
		log.Error(err, "reap stuck attempts failed")
	} else if n > 0 {
		log.Info("reaped stuck attempts", "count", n, "olderThan", reapAfter.String())
	}
	if n, err := attempt.GCAttempts(ctx, j.Client, j.Namespace, retention); err != nil {
		log.Error(err, "attempt retention GC failed")
	} else if n > 0 {
		log.Info("GC'd attempts past retention", "count", n, "olderThan", retention.String())
	}
	if n, err := j.deleteOrphanedRunnerPods(ctx); err != nil {
		log.Error(err, "orphaned runner pod sweep failed")
	} else if n > 0 {
		log.Info("deleted orphaned runner pods", "count", n)
	}
}

// deleteOrphanedRunnerPods removes terminal-phase attempt-runner pods whose
// Job no longer exists. Live pods are never touched, and a pod whose Job
// cannot be RESOLVED (missing job-name label) is skipped — the leg deletes
// only what is provably orphaned, never what is merely unusual.
func (j *Janitor) deleteOrphanedRunnerPods(ctx context.Context) (int, error) {
	var pods corev1.PodList
	if err := j.List(ctx, &pods, client.InNamespace(j.Namespace),
		client.MatchingLabels{"app.kubernetes.io/component": "attempt-runner"}); err != nil {
		return 0, fmt.Errorf("list runner pods: %w", err)
	}
	deleted := 0
	var firstErr error
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			continue
		}
		jobName := pod.Labels["batch.kubernetes.io/job-name"]
		if jobName == "" {
			jobName = pod.Labels["job-name"] // pre-1.27 label
		}
		if jobName == "" {
			continue
		}
		var job batchv1.Job
		if err := j.Get(ctx, client.ObjectKey{Namespace: j.Namespace, Name: jobName}, &job); err == nil {
			continue // Job alive — not an orphan
		} else if !apierrors.IsNotFound(err) {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := j.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		deleted++
	}
	return deleted, firstErr
}
