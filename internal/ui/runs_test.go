package ui

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// runsTestServer builds a Server with a fake k8s client and a stub log fetcher.
func runsTestServer(existing ...client.Object) *Server {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(existing...).
		Build()

	tmpl, _ := parseTemplates()

	s := &Server{
		k8sClient: cl,
		namespace: "harmostes",
		logger:    slog.Default(),
		templates: tmpl,
		logFetch: func(ctx context.Context, ns, pod, container string) (string, error) {
			return `{"time":"2026-07-18T20:01:23.537Z","level":"INFO","msg":"workflow started","component":"worker"}
plugin stderr output here`, nil
		},
	}
	return s
}

func makeJob(name, workflow, owner string, succeeded bool) *batchv1.Job {
	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "harmostes",
			Labels: map[string]string{
				v1alpha1.WorkflowLabel: workflow,
			},
		},
		Status: batchv1.JobStatus{},
	}
	if owner != "" {
		j.Labels[v1alpha1.OwnerLabel] = owner
	}
	if succeeded {
		j.Status.Succeeded = 1
		now := metav1.Now()
		j.Status.StartTime = &now
		j.Status.CompletionTime = &now
	} else {
		j.Status.Failed = 1
	}
	return j
}

func makePod(name, jobName string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "harmostes",
			Labels:    map[string]string{"job-name": jobName},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func TestHandleRunDetail_OwnerIsolation(t *testing.T) {
	// Alice owns the workflow; Bob tries to view a run.
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alice-wf",
			Namespace: "harmostes",
			Labels:    map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
	}
	job := makeJob("alice-job", "alice-wf", "alice", true)
	pod := makePod("alice-pod", "alice-job", corev1.PodSucceeded)

	s := runsTestServer(wf, job, pod)

	// Bob tries to view Alice's run
	req := httptest.NewRequest(http.MethodGet, "/workflows/alice-wf/runs/alice-job", nil)
	req.SetPathValue("name", "alice-wf")
	req.SetPathValue("job", "alice-job")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "bob"}))

	rec := httptest.NewRecorder()
	s.handleRunDetail(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (cross-tenant access must fail)", rec.Code, http.StatusNotFound)
	}
}

func TestHandleRunDetail_Success(t *testing.T) {
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alice-wf",
			Namespace: "harmostes",
			Labels:    map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
	}
	job := makeJob("alice-job", "alice-wf", "alice", true)
	pod := makePod("alice-pod", "alice-job", corev1.PodSucceeded)

	s := runsTestServer(wf, job, pod)

	req := httptest.NewRequest(http.MethodGet, "/workflows/alice-wf/runs/alice-job", nil)
	req.SetPathValue("name", "alice-wf")
	req.SetPathValue("job", "alice-job")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleRunDetail(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if !contains(body, "alice-job") {
		t.Error("expected job name in output")
	}
	if !contains(body, "Succeeded") {
		t.Error("expected Succeeded status")
	}
	if !contains(body, "workflow started") {
		t.Error("expected log content from stub")
	}
}

func TestHandleRunDetail_JobBelongsToDifferentWorkflow(t *testing.T) {
	// Alice owns workflow A. A job exists labeled for workflow B (but same name).
	// The handler must reject: the job doesn't belong to workflow A.
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alice-wf",
			Namespace: "harmostes",
			Labels:    map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
	}
	// Job has workflow label pointing to "other-wf", not "alice-wf"
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "some-job",
			Namespace: "harmostes",
			Labels:    map[string]string{v1alpha1.WorkflowLabel: "other-wf"},
		},
	}

	s := runsTestServer(wf, job)

	req := httptest.NewRequest(http.MethodGet, "/workflows/alice-wf/runs/some-job", nil)
	req.SetPathValue("name", "alice-wf")
	req.SetPathValue("job", "some-job")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleRunDetail(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (job belongs to different workflow)", rec.Code, http.StatusNotFound)
	}
}

func TestHandleRunDetail_WorkflowNotFound(t *testing.T) {
	s := runsTestServer()

	req := httptest.NewRequest(http.MethodGet, "/workflows/nonexistent/runs/job1", nil)
	req.SetPathValue("name", "nonexistent")
	req.SetPathValue("job", "job1")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleRunDetail(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHandleRunDetail_JobNotFound(t *testing.T) {
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alice-wf",
			Namespace: "harmostes",
			Labels:    map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
	}
	s := runsTestServer(wf)

	req := httptest.NewRequest(http.MethodGet, "/workflows/alice-wf/runs/nonexistent-job", nil)
	req.SetPathValue("name", "alice-wf")
	req.SetPathValue("job", "nonexistent-job")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleRunDetail(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHandleRunDetail_UnmanagedWorkflowRejected(t *testing.T) {
	// System workflow (no owner label) — must not be accessible from UI.
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "system-wf",
			Namespace: "harmostes",
			Labels:    map[string]string{},
		},
	}
	job := makeJob("system-job", "system-wf", "", true)
	s := runsTestServer(wf, job)

	req := httptest.NewRequest(http.MethodGet, "/workflows/system-wf/runs/system-job", nil)
	req.SetPathValue("name", "system-wf")
	req.SetPathValue("job", "system-job")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleRunDetail(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (unmanaged workflow)", rec.Code, http.StatusNotFound)
	}
}

func TestHandleRunDetail_LogFetchError(t *testing.T) {
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alice-wf",
			Namespace: "harmostes",
			Labels:    map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
	}
	job := makeJob("alice-job", "alice-wf", "alice", true)
	pod := makePod("alice-pod", "alice-job", corev1.PodSucceeded)

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(wf, job, pod).Build()
	tmpl, _ := parseTemplates()

	s := &Server{
		k8sClient: cl,
		namespace: "harmostes",
		logger:    slog.Default(),
		templates: tmpl,
		logFetch: func(ctx context.Context, ns, pod, container string) (string, error) {
			return "", errFakeLogFetch
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/workflows/alice-wf/runs/alice-job", nil)
	req.SetPathValue("name", "alice-wf")
	req.SetPathValue("job", "alice-job")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleRunDetail(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "Failed to fetch logs") {
		t.Error("expected error message about failed log fetch")
	}
}

func TestHandleRunDetail_NilLogFetch(t *testing.T) {
	// When logFetch is nil (log viewer not configured), the page should still
	// render without crashing — showing "Log viewer not configured."
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alice-wf",
			Namespace: "harmostes",
			Labels:    map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
	}
	job := makeJob("alice-job", "alice-wf", "alice", true)
	pod := makePod("alice-pod", "alice-job", corev1.PodSucceeded)

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(wf, job, pod).Build()
	tmpl, _ := parseTemplates()

	s := &Server{
		k8sClient: cl,
		namespace: "harmostes",
		logger:    slog.Default(),
		templates: tmpl,
		logFetch:  nil, // log viewer disabled
	}

	req := httptest.NewRequest(http.MethodGet, "/workflows/alice-wf/runs/alice-job", nil)
	req.SetPathValue("name", "alice-wf")
	req.SetPathValue("job", "alice-job")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleRunDetail(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "Log viewer not configured") {
		t.Error("expected 'Log viewer not configured' message when logFetch is nil")
	}
}

func TestFormatLogs_PassThroughNonJSON(t *testing.T) {
	input := "plain text line\n{\"level\":\"INFO\",\"msg\":\"json line\"}\nanother plain\n"
	out := formatLogs(input)
	if !contains(out, "plain text line") {
		t.Error("non-JSON line should pass through")
	}
	if !contains(out, "another plain") {
		t.Error("non-JSON line should pass through")
	}
	if !contains(out, "INFO") {
		t.Error("JSON line should be formatted")
	}
}

func TestFormatJSONLogLine(t *testing.T) {
	input := `{"time":"2026-07-18T20:01:23.537Z","level":"INFO","msg":"workflow started","component":"worker"}`
	out, ok := formatJSONLogLine(input)
	if !ok {
		t.Fatal("should parse as JSON")
	}
	if !contains(out, "INFO") {
		t.Error("should contain level")
	}
	if !contains(out, "workflow started") {
		t.Error("should contain message")
	}
}

func TestFormatJSONLogLine_InvalidJSON(t *testing.T) {
	_, ok := formatJSONLogLine("not json at all")
	if ok {
		t.Error("should return false for invalid JSON")
	}
}

func TestSelectPod_PrefersRunning(t *testing.T) {
	pods := []corev1.Pod{
		*makePod("pod1", "job1", corev1.PodSucceeded),
		*makePod("pod2", "job1", corev1.PodRunning),
		*makePod("pod3", "job1", corev1.PodFailed),
	}
	selected := selectPod(pods)
	if selected.Name != "pod2" {
		t.Errorf("expected pod2 (running), got %s", selected.Name)
	}
}

func TestSelectPod_LatestWhenNoRunning(t *testing.T) {
	t1 := metav1.NewTime(metav1.Now().Add(-10 * 1e9)) // 10s ago
	t2 := metav1.Now()
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "older", CreationTimestamp: t1}, Status: corev1.PodStatus{Phase: corev1.PodSucceeded}},
		{ObjectMeta: metav1.ObjectMeta{Name: "newer", CreationTimestamp: t2}, Status: corev1.PodStatus{Phase: corev1.PodSucceeded}},
	}
	selected := selectPod(pods)
	if selected.Name != "newer" {
		t.Errorf("expected newer pod, got %s", selected.Name)
	}
}

func TestPodExitCode(t *testing.T) {
	exitCode := int32(1)
	pod := corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "worker",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode},
					},
				},
				{
					Name: "daprd",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 0},
					},
				},
			},
		},
	}
	code := podExitCode(pod)
	if code == nil || *code != 1 {
		t.Errorf("expected exit code 1, got %v", code)
	}
}

func TestPodExitCode_NotTerminated(t *testing.T) {
	pod := corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "worker", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	}
	code := podExitCode(pod)
	if code != nil {
		t.Errorf("expected nil for running container, got %d", *code)
	}
}

func TestJobDuration(t *testing.T) {
	start := metav1.Time{}
	t1 := metav1.Now()
	start.Time = t1.Add(-5_000_000_000) // 5s ago

	job := batchv1.Job{
		Status: batchv1.JobStatus{
			StartTime:      &start,
			CompletionTime: &t1,
		},
	}
	d := jobDuration(job)
	if d == "—" || d == "running…" {
		t.Errorf("expected a duration, got %q", d)
	}

	// Running job (no completion time)
	job2 := batchv1.Job{Status: batchv1.JobStatus{StartTime: &start}}
	d2 := jobDuration(job2)
	if d2 != "running…" {
		t.Errorf("expected 'running…', got %q", d2)
	}
}

func TestListPodsForJob(t *testing.T) {
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alice-wf",
			Namespace: "harmostes",
			Labels:    map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
	}
	job := makeJob("alice-job", "alice-wf", "alice", true)
	pod1 := makePod("pod1", "alice-job", corev1.PodSucceeded)
	pod2 := makePod("pod2", "other-job", corev1.PodSucceeded)

	s := runsTestServer(wf, job, pod1, pod2)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.Background())

	pods, err := s.listPodsForJob(req, "alice-job")
	if err != nil {
		t.Fatalf("listPodsForJob: %v", err)
	}
	if len(pods) != 1 {
		t.Errorf("expected 1 pod for alice-job, got %d", len(pods))
	}
	if pods[0].Name != "pod1" {
		t.Errorf("expected pod1, got %s", pods[0].Name)
	}
}

func TestRunDetail_VerifyWorkflowGet(t *testing.T) {
	// Ensure the handler verifies the workflow by getting it (not just trusting
	// the URL parameter).
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alice-wf",
			Namespace: "harmostes",
			Labels:    map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
	}
	_ = wf

	s := runsTestServer()

	// Workflow doesn't exist in the fake client → should be NotFound
	req := httptest.NewRequest(http.MethodGet, "/workflows/alice-wf/runs/job1", nil)
	req.SetPathValue("name", "alice-wf")
	req.SetPathValue("job", "job1")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleRunDetail(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// contains is a minimal strings.Contains wrapper for test readability.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// sentinel error for the log fetch error test
var errFakeLogFetch = &fakeErr{"simulated log fetch failure"}

type fakeErr struct{ msg string }

func (e *fakeErr) Error() string { return e.msg }

// Ensure types are imported (avoid unused import errors if a test references them)
var _ = types.NamespacedName{}
