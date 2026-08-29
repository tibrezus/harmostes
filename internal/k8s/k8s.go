// Package k8s holds controller-runtime-backed implementations of the worker's
// collaborator interfaces: StatusPatcher (status-subresource patches on the
// Workflow CR) and a ConfigMap-backed TaskResolver.
package k8s

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// StatusPatcher reconciles a Workflow's status via the status subresource.
type StatusPatcher struct {
	Client    client.Client
	Namespace string
}

// GetStatus returns the workflow's CURRENT status — the fresh read every
// gate/pipeline decision must start from. A run-start snapshot can be
// arbitrarily stale (a long evaluate, another writer in between); deciding
// on it is the #257 lost-update class.
func (s StatusPatcher) GetStatus(ctx context.Context, name string) (*v1alpha1.WorkflowStatus, error) {
	var wf v1alpha1.Workflow
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: name}, &wf); err != nil {
		return nil, fmt.Errorf("get workflow %s: %w", name, err)
	}
	statusCopy := wf.Status
	return &statusCopy, nil
}

// PatchStatus applies mutate to the workflow status with lost-update
// discipline (#257): each attempt re-Gets, mutates only that fresh copy, and
// patches under a resourceVersion precondition (MergeFromWithOptimisticLock)
// — a concurrent writer (controller conditions, a parallel gate evaluation)
// bumps the RV, the patch conflicts, and the retry re-reads. Mutate closures
// therefore see live state and must derive persisted values from it, never
// from a snapshot taken before the call.
func (s StatusPatcher) PatchStatus(ctx context.Context, name string, mutate func(*v1alpha1.WorkflowStatus)) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var wf v1alpha1.Workflow
		if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: name}, &wf); err != nil {
			return fmt.Errorf("get workflow %s: %w", name, err)
		}
		base := wf.DeepCopy()
		mutate(&wf.Status)
		patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
		if err := s.Client.Status().Patch(ctx, &wf, patch); err != nil {
			return fmt.Errorf("patch workflow %s status: %w", name, err)
		}
		return nil
	})
}

// ConfigMapTasks resolves a TaskTemplate to its text from a ConfigMap key.
type ConfigMapTasks struct {
	Client    client.Client
	Namespace string
}

func (c ConfigMapTasks) Get(ctx context.Context, tt v1alpha1.TaskTemplate) (string, error) {
	if tt.ConfigMap == "" || tt.Key == "" {
		return "", fmt.Errorf("task template %q has no configMap/key", tt.Name)
	}
	var cm corev1.ConfigMap
	if err := c.Client.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: tt.ConfigMap}, &cm); err != nil {
		return "", fmt.Errorf("get task configmap %s: %w", tt.ConfigMap, err)
	}
	v, ok := cm.Data[tt.Key]
	if !ok {
		return "", fmt.Errorf("task configmap %s has no key %q", tt.ConfigMap, tt.Key)
	}
	return v, nil
}

// Scheme returns a runtime.Scheme with the harmostes + core + batch types
// registered (what both the controller and the worker need).
func Scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	return s
}
