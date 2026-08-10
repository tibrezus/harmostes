package ui

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNewK8sClient(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	k := NewK8sClient(fakeClient, "test-ns")
	if k == nil {
		t.Error("NewK8sClient returned nil")
	}
}

func TestK8sClient_GetWorkflow(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)

	tests := []struct {
		name       string
		existing   *v1alpha1.Workflow
		want       string
		wantErr    bool
		errMessage string
	}{
		{
			name: "get existing workflow",
			existing: &v1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-workflow",
					Namespace: "test-ns",
					Labels: map[string]string{
						v1alpha1.OwnerLabel: "alice",
					},
				},
				Spec: v1alpha1.WorkflowSpec{
					Agent: v1alpha1.AgentSpec{
						Skill: "/test",
					},
				},
			},
			want: "test-workflow",
		},
		{
			name:       "get non-existent workflow",
			want:       "missing-workflow",
			wantErr:    true,
			errMessage: "get workflow missing-workflow",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objs []client.Object
			if tt.existing != nil {
				objs = append(objs, tt.existing)
			}

			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
			k := NewK8sClient(fakeClient, "test-ns")

			got, err := k.GetWorkflow(context.Background(), tt.want)

			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
					return
				}
				if !apierrors.IsNotFound(err) {
					t.Errorf("expected NotFound error, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if got.Name != tt.want {
				t.Errorf("expected name %s, got %s", tt.want, got.Name)
			}
		})
	}
}

func TestK8sClient_ListWorkflows(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)

	wf1 := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "wf1",
			Namespace: "test-ns",
			Labels: map[string]string{
				v1alpha1.OwnerLabel: "alice",
			},
		},
	}
	wf2 := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "wf2",
			Namespace: "test-ns",
			Labels: map[string]string{
				v1alpha1.OwnerLabel: "alice",
			},
		},
	}
	wf3 := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "wf3",
			Namespace: "test-ns",
			Labels: map[string]string{
				v1alpha1.OwnerLabel: "bob", // different owner
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(wf1, wf2, wf3).Build()
	k := NewK8sClient(fakeClient, "test-ns")

	got, err := k.ListWorkflows(context.Background(), "alice")

	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	if len(got) != 2 {
		t.Errorf("expected 2 workflows for alice, got %d", len(got))
	}
}

func TestK8sClient_CreateWorkflow(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	k := NewK8sClient(fakeClient, "test-ns")

	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "new-workflow",
			Namespace: "test-ns",
			Labels: map[string]string{
				v1alpha1.OwnerLabel: "alice",
			},
		},
		Spec: v1alpha1.WorkflowSpec{
			Agent: v1alpha1.AgentSpec{
				Skill: "/test",
			},
		},
	}

	err := k.CreateWorkflow(context.Background(), wf)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}

	// Verify it was created
	got, err := k.GetWorkflow(context.Background(), "new-workflow")
	if err != nil {
		t.Errorf("workflow not created: %v", err)
		return
	}
	if got.Name != "new-workflow" {
		t.Errorf("expected name new-workflow, got %s", got.Name)
	}
}

func TestK8sClient_UpdateWorkflow(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)

	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-workflow",
			Namespace: "test-ns",
			Labels: map[string]string{
				v1alpha1.OwnerLabel: "alice",
			},
		},
		Spec: v1alpha1.WorkflowSpec{
			Agent: v1alpha1.AgentSpec{
				Skill: "/old",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(wf).Build()
	k := NewK8sClient(fakeClient, "test-ns")

	// Get, update, and save
	got, err := k.GetWorkflow(context.Background(), "test-workflow")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	got.Spec.Agent.Skill = "/new"

	err = k.UpdateWorkflow(context.Background(), got)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}

	// Verify update
	updated, err := k.GetWorkflow(context.Background(), "test-workflow")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	if updated.Spec.Agent.Skill != "/new" {
		t.Errorf("expected skill /new, got %s", updated.Spec.Agent.Skill)
	}
}

func TestK8sClient_DeleteWorkflow(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)

	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-workflow",
			Namespace: "test-ns",
			Labels: map[string]string{
				v1alpha1.OwnerLabel: "alice",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(wf).Build()
	k := NewK8sClient(fakeClient, "test-ns")

	err := k.DeleteWorkflow(context.Background(), "test-workflow")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}

	// Verify deletion
	_, err = k.GetWorkflow(context.Background(), "test-workflow")
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected NotFound error, got: %v", err)
	}
}

func TestK8sClient_DeleteWorkflow_Idempotent(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	k := NewK8sClient(fakeClient, "test-ns")

	// Delete non-existent workflow should not error
	err := k.DeleteWorkflow(context.Background(), "non-existent")
	if err != nil {
		t.Errorf("expected nil for idempotent delete, got: %v", err)
	}
}

func TestK8sClient_ListJobs(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = batchv1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)

	job1 := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "job1",
			Namespace: "test-ns",
			Labels: map[string]string{
				v1alpha1.WorkflowLabel: "test-workflow",
			},
		},
	}
	job2 := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "job2",
			Namespace: "test-ns",
			Labels: map[string]string{
				v1alpha1.WorkflowLabel: "test-workflow",
			},
		},
	}
	job3 := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "job3",
			Namespace: "test-ns",
			Labels: map[string]string{
				v1alpha1.WorkflowLabel: "other-workflow", // different workflow
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(job1, job2, job3).Build()
	k := NewK8sClient(fakeClient, "test-ns")

	got, err := k.ListJobs(context.Background(), "test-workflow")

	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	if len(got) != 2 {
		t.Errorf("expected 2 jobs for test-workflow, got %d", len(got))
	}
}

func TestK8sClient_CreateSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	k := NewK8sClient(fakeClient, "test-ns")

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-secret",
			// Namespace should be set by CreateSecret
		},
		Data: map[string][]byte{
			"token": []byte("secret-value"),
		},
	}

	err := k.CreateSecret(context.Background(), secret)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}

	// Verify namespace was set
	if secret.Namespace != "test-ns" {
		t.Errorf("expected namespace test-ns, got %s", secret.Namespace)
	}
}

func TestK8sClient_GetSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: "test-ns",
		},
		Data: map[string][]byte{
			"token": []byte("secret-value"),
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	k := NewK8sClient(fakeClient, "test-ns")

	got, err := k.GetSecret(context.Background(), "test-secret")

	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	if got.Name != "test-secret" {
		t.Errorf("expected name test-secret, got %s", got.Name)
	}
}

func TestK8sClient_DeleteSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: "test-ns",
		},
		Data: map[string][]byte{
			"token": []byte("secret-value"),
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	k := NewK8sClient(fakeClient, "test-ns")

	err := k.DeleteSecret(context.Background(), "test-secret")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}

	// Verify deletion
	_, err = k.GetSecret(context.Background(), "test-secret")
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected NotFound error, got: %v", err)
	}
}

func TestK8sClient_DeleteSecret_Idempotent(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	k := NewK8sClient(fakeClient, "test-ns")

	// Delete non-existent secret should not error
	err := k.DeleteSecret(context.Background(), "non-existent")
	if err != nil {
		t.Errorf("expected nil for idempotent delete, got: %v", err)
	}
}
