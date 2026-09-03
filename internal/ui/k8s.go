// Package ui provides the K8s client wrapper for harmostes-ui.
// This wraps the controller-runtime client with UI-specific helpers
// for managing Workflows, Jobs, and Secrets.
package ui

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// K8sClient wraps the controller-runtime client with UI-specific operations.
type K8sClient interface {
	// Workflow CRUD
	GetWorkflow(ctx context.Context, name string) (*v1alpha1.Workflow, error)
	ListWorkflows(ctx context.Context, owner string) ([]v1alpha1.Workflow, error)
	CreateWorkflow(ctx context.Context, wf *v1alpha1.Workflow) error
	UpdateWorkflow(ctx context.Context, wf *v1alpha1.Workflow) error
	DeleteWorkflow(ctx context.Context, name string) error

	// Attempt queries (observability-first UI)
	GetAttempt(ctx context.Context, name string) (*v1alpha1.Attempt, error)
	ListAttempts(ctx context.Context, owner string) ([]v1alpha1.Attempt, error)

	// Job listing
	ListJobs(ctx context.Context, workflowName string) ([]batchv1.Job, error)

	// Secret CRUD
	CreateSecret(ctx context.Context, secret *corev1.Secret) error
	GetSecret(ctx context.Context, name string) (*corev1.Secret, error)
	DeleteSecret(ctx context.Context, name string) error
}

// k8sClient implements K8sClient using controller-runtime client.
type k8sClient struct {
	client    client.Client
	namespace string
}

// NewK8sClient creates a new K8s client wrapper.
func NewK8sClient(client client.Client, namespace string) K8sClient {
	return &k8sClient{
		client:    client,
		namespace: namespace,
	}
}

// Workflow CRUD

// GetWorkflow retrieves a Workflow by name.
func (k *k8sClient) GetWorkflow(ctx context.Context, name string) (*v1alpha1.Workflow, error) {
	wf := &v1alpha1.Workflow{}
	if err := k.client.Get(ctx, client.ObjectKey{Namespace: k.namespace, Name: name}, wf); err != nil {
		return nil, fmt.Errorf("get workflow %s: %w", name, err)
	}
	return wf, nil
}

// ListWorkflows lists all Workflows for a given owner; empty owner lists
// across all owners.
func (k *k8sClient) ListWorkflows(ctx context.Context, owner string) ([]v1alpha1.Workflow, error) {
	var list v1alpha1.WorkflowList
	opts := []client.ListOption{}
	if owner != "" {
		opts = append(opts, client.MatchingLabels{v1alpha1.OwnerLabel: owner})
	}
	if err := k.client.List(ctx, &list, opts...); err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	return list.Items, nil
}

// GetAttempt retrieves a single Attempt by name.
func (k *k8sClient) GetAttempt(ctx context.Context, name string) (*v1alpha1.Attempt, error) {
	att := &v1alpha1.Attempt{}
	if err := k.client.Get(ctx, client.ObjectKey{Namespace: k.namespace, Name: name}, att); err != nil {
		return nil, fmt.Errorf("get attempt %s: %w", name, err)
	}
	return att, nil
}

// ListAttempts lists all Attempts for a given owner; empty owner lists
// across all owners.
func (k *k8sClient) ListAttempts(ctx context.Context, owner string) ([]v1alpha1.Attempt, error) {
	var list v1alpha1.AttemptList
	opts := []client.ListOption{}
	if owner != "" {
		opts = append(opts, client.MatchingLabels{v1alpha1.OwnerLabel: owner})
	}
	if err := k.client.List(ctx, &list, opts...); err != nil {
		return nil, fmt.Errorf("list attempts: %w", err)
	}
	return list.Items, nil
}

// CreateWorkflow creates a new Workflow.
func (k *k8sClient) CreateWorkflow(ctx context.Context, wf *v1alpha1.Workflow) error {
	if err := k.client.Create(ctx, wf); err != nil {
		return fmt.Errorf("create workflow %s: %w", wf.Name, err)
	}
	return nil
}

// UpdateWorkflow updates an existing Workflow.
func (k *k8sClient) UpdateWorkflow(ctx context.Context, wf *v1alpha1.Workflow) error {
	if err := k.client.Update(ctx, wf); err != nil {
		return fmt.Errorf("update workflow %s: %w", wf.Name, err)
	}
	return nil
}

// DeleteWorkflow deletes a Workflow by name.
func (k *k8sClient) DeleteWorkflow(ctx context.Context, name string) error {
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: k.namespace,
		},
	}
	if err := k.client.Delete(ctx, wf); err != nil {
		if errors.IsNotFound(err) {
			return nil // idempotent delete
		}
		return fmt.Errorf("delete workflow %s: %w", name, err)
	}
	return nil
}

// Job listing

// ListJobs lists all Jobs for a given workflow.
func (k *k8sClient) ListJobs(ctx context.Context, workflowName string) ([]batchv1.Job, error) {
	var list batchv1.JobList
	if err := k.client.List(ctx, &list,
		client.InNamespace(k.namespace),
		client.MatchingLabels{v1alpha1.WorkflowLabel: workflowName},
	); err != nil {
		return nil, fmt.Errorf("list jobs for workflow %s: %w", workflowName, err)
	}
	return list.Items, nil
}

// Secret CRUD

// CreateSecret creates a new Secret.
func (k *k8sClient) CreateSecret(ctx context.Context, secret *corev1.Secret) error {
	if secret.Namespace == "" {
		secret.Namespace = k.namespace
	}
	if err := k.client.Create(ctx, secret); err != nil {
		return fmt.Errorf("create secret %s: %w", secret.Name, err)
	}
	return nil
}

// GetSecret retrieves a Secret by name.
func (k *k8sClient) GetSecret(ctx context.Context, name string) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	if err := k.client.Get(ctx, client.ObjectKey{Namespace: k.namespace, Name: name}, secret); err != nil {
		return nil, fmt.Errorf("get secret %s: %w", name, err)
	}
	return secret, nil
}

// DeleteSecret deletes a Secret by name.
func (k *k8sClient) DeleteSecret(ctx context.Context, name string) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: k.namespace,
		},
	}
	if err := k.client.Delete(ctx, secret); err != nil {
		if errors.IsNotFound(err) {
			return nil // idempotent delete
		}
		return fmt.Errorf("delete secret %s: %w", name, err)
	}
	return nil
}
