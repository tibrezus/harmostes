package k8s

import (
	"context"
	"errors"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Self-pod deletion (#613): the missing-injection recovery path. A pod the
// injector skipped cannot gain its sidecar by restarting the container —
// admission never re-runs. Deleting the pod lets the workload's owning
// controller (Deployment ReplicaSet, or the dispatcher's re-arm for attempt
// Jobs) mint a replacement whose admission hits a healthy injector webhook.

// ErrNoPodIdentity means neither POD_NAME nor HOSTNAME is set — the process
// is not running as a Kubernetes pod (local dev), so there is nothing to
// delete and self-heal must not be attempted.
var ErrNoPodIdentity = errors.New("no pod identity: set POD_NAME (or HOSTNAME) to enable self-delete")

// DeleteOwnPod deletes the pod this process runs in. Best-effort by design at
// the call sites: RBAC without pods/delete, a vanished pod (already replaced),
// or an unreachable API server all degrade to "exit non-zero and let the
// workload controller's own machinery retry".
func DeleteOwnPod(ctx context.Context, cl client.Client, namespace string) error {
	name := os.Getenv("POD_NAME")
	if name == "" {
		name = os.Getenv("HOSTNAME") // k8s sets HOSTNAME to the pod name
	}
	if name == "" {
		return ErrNoPodIdentity
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if err := cl.Delete(ctx, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // already gone — the replacement won the race
		}
		return fmt.Errorf("delete own pod %s/%s: %w", namespace, name, err)
	}
	return nil
}

// DeleteOwnPodInCluster is the mains' one-liner: builds an in-cluster client
// and deletes this process's pod in namespace. The controller-runtime config
// read fails outside a cluster (dev) — surfaced, not swallowed: callers log
// it before their fatal exit.
func DeleteOwnPodInCluster(ctx context.Context, namespace string) error {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("k8s config: %w", err)
	}
	cl, err := client.New(cfg, client.Options{Scheme: Scheme()})
	if err != nil {
		return fmt.Errorf("k8s client: %w", err)
	}
	return DeleteOwnPod(ctx, cl, namespace)
}
