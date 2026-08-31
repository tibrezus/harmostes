package ui

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

	// No WithStatusSubresource: these tests only seed and GET — the fake
	// tracker would strip the seeded status on create if the subresource
	// were declared (controller-runtime gotcha).
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

// makeAttempt builds an Attempt CR owned by owner, tied to workflow, with one
// recorded run named runName in the given phase.
func makeAttempt(name, owner, workflow, runName, phase string) *v1alpha1.Attempt {
	return &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "harmostes",
			Labels: map[string]string{
				v1alpha1.OwnerLabel:    owner,
				v1alpha1.WorkflowLabel: workflow,
			},
		},
		Spec: v1alpha1.AttemptSpec{
			WorkflowRef: "harmostes/" + workflow,
		},
		Status: v1alpha1.AttemptStatus{
			Runs: []v1alpha1.RunRecord{{Name: runName, Phase: phase}},
		},
	}
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

// ---------------------------------------------------------------------------
// Legacy Job-page redirect
// ---------------------------------------------------------------------------

// TestRunRedirect_ResolvesAttempt pins the dead-link fix: a legacy
// /workflows/{wf}/runs/{job} deep link resolves to the attempt that recorded
// the run — never to the ephemeral Job.
func TestRunRedirect_ResolvesAttempt(t *testing.T) {
	att := makeAttempt("attempt-pr-review-x-abc123", "alice", "pr-review-harmostes", "job-7jtbx", "succeeded")
	s := runsTestServer(att)

	req := httptest.NewRequest(http.MethodGet, "/workflows/pr-review-harmostes/runs/job-7jtbx", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/runs/attempt-pr-review-x-abc123" {
		t.Errorf("Location = %q, want /runs/attempt-pr-review-x-abc123", loc)
	}
}

func TestRunRedirect_ForeignOwner(t *testing.T) {
	att := makeAttempt("attempt-x", "alice", "wf-a", "job-1", "succeeded")
	s := runsTestServer(att)

	req := httptest.NewRequest(http.MethodGet, "/workflows/wf-a/runs/job-1", nil)
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "mallory"}))
	rec := httptest.NewRecorder()
	s.handleWorkflowRunRedirect(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (no leakage across owners)", rec.Code)
	}
}

func TestRunRedirect_UnknownRun(t *testing.T) {
	att := makeAttempt("attempt-x", "alice", "wf-a", "job-1", "succeeded")
	s := runsTestServer(att)

	req := httptest.NewRequest(http.MethodGet, "/workflows/wf-a/runs/job-unknown", nil)
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))
	rec := httptest.NewRecorder()
	s.handleWorkflowRunRedirect(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestRedirectAttempts_PreservesPathAndQuery(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/attempts?window=all", nil)
	rec := httptest.NewRecorder()
	redirectAttempts(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/runs?window=all" {
		t.Errorf("Location = %q, want /runs?window=all", loc)
	}
}

// ---------------------------------------------------------------------------
// Run logs fragment
// ---------------------------------------------------------------------------

func TestRunLogs_PodAlive(t *testing.T) {
	att := makeAttempt("attempt-x", "alice", "wf-a", "job-1", "running")
	pod := makePod("job-1-pod", "job-1", corev1.PodRunning)
	pod.Status.StartTime = &metav1.Time{Time: metav1.Now().Time}
	s := runsTestServer(att, pod)

	req := httptest.NewRequest(http.MethodGet, "/runs/attempt-x/runs/job-1/logs", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// The stub log's slog line is formatted (msg extracted), the plugin line passes through.
	if !strings.Contains(body, "workflow started") || !strings.Contains(body, "plugin stderr output here") {
		t.Errorf("fragment missing log content:\n%s", body)
	}
	if !strings.Contains(body, "job-1-pod") {
		t.Errorf("fragment missing pod metadata:\n%s", body)
	}
	if strings.Contains(body, "Pod recycled") {
		t.Errorf("fragment wrongly claims pod is gone:\n%s", body)
	}
}

func TestRunLogs_PodGone_PointsAtTranscript(t *testing.T) {
	att := makeAttempt("attempt-x", "alice", "wf-a", "job-1", "succeeded")
	s := runsTestServer(att) // no pod objects — the normal terminal state

	req := httptest.NewRequest(http.MethodGet, "/runs/attempt-x/runs/job-1/logs", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Pod recycled") {
		t.Errorf("fragment missing pod-gone note:\n%s", body)
	}
	if !strings.Contains(body, "/runs/attempt-x/runs/job-1/session") {
		t.Errorf("fragment missing durable transcript link:\n%s", body)
	}
}

// TestRunLogs_ForgedJobName is the security pin: a job name that is not in
// the attempt's run list must 404 even for the attempt's owner — the URL
// alone must never leak another run's logs.
func TestRunLogs_ForgedJobName(t *testing.T) {
	att := makeAttempt("attempt-x", "alice", "wf-a", "job-1", "succeeded")
	s := runsTestServer(att)

	req := httptest.NewRequest(http.MethodGet, "/runs/attempt-x/runs/job-other/logs", nil)
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))
	rec := httptest.NewRecorder()
	s.handleRunLogs(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestRunLogs_ForeignOwner(t *testing.T) {
	att := makeAttempt("attempt-x", "alice", "wf-a", "job-1", "succeeded")
	pod := makePod("job-1-pod", "job-1", corev1.PodRunning)
	s := runsTestServer(att, pod)

	req := httptest.NewRequest(http.MethodGet, "/runs/attempt-x/runs/job-1/logs", nil)
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "mallory"}))
	rec := httptest.NewRecorder()
	s.handleRunLogs(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (no leakage across owners)", rec.Code)
	}
}

func TestRunLogs_NilLogFetch(t *testing.T) {
	att := makeAttempt("attempt-x", "alice", "wf-a", "job-1", "running")
	pod := makePod("job-1-pod", "job-1", corev1.PodRunning)
	s := runsTestServer(att, pod)
	s.logFetch = nil

	req := httptest.NewRequest(http.MethodGet, "/runs/attempt-x/runs/job-1/logs", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "log streaming not configured") {
		t.Errorf("fragment missing honest no-config note:\n%s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

func TestFormatLogs_PassThroughNonJSON(t *testing.T) {
	raw := "plain line\n{\"time\":\"2026-07-18T20:01:23.537Z\",\"level\":\"INFO\",\"msg\":\"hi\"}\n"
	got := formatLogs(raw)
	if !strings.Contains(got, "plain line") {
		t.Errorf("non-JSON line dropped:\n%s", got)
	}
	if !strings.Contains(got, "INFO") || !strings.Contains(got, "hi") {
		t.Errorf("JSON line not formatted:\n%s", got)
	}
}

func TestFormatLogs_Empty(t *testing.T) {
	if got := formatLogs(""); got != "" {
		t.Errorf("formatLogs(\"\") = %q, want \"\"", got)
	}
}

func TestSelectPod_PrefersRunning(t *testing.T) {
	older := makePod("older", "j", corev1.PodSucceeded)
	newer := makePod("newer", "j", corev1.PodSucceeded)
	newer.CreationTimestamp = metav1.Now()
	running := makePod("running", "j", corev1.PodRunning)
	got := selectPod([]corev1.Pod{*older, *newer, *running})
	if got.Name != "running" {
		t.Errorf("selectPod picked %q, want running", got.Name)
	}
}

func TestSelectPod_LatestWhenNoRunning(t *testing.T) {
	older := makePod("older", "j", corev1.PodSucceeded)
	newer := makePod("newer", "j", corev1.PodSucceeded)
	newer.CreationTimestamp = metav1.Now()
	got := selectPod([]corev1.Pod{*older, *newer})
	if got.Name != "newer" {
		t.Errorf("selectPod picked %q, want newer", got.Name)
	}
}

func TestPodExitCode_NotTerminated(t *testing.T) {
	pod := makePod("p", "j", corev1.PodRunning)
	if code := podExitCode(*pod); code != nil {
		t.Errorf("podExitCode = %v, want nil for running pod", *code)
	}
}

func TestListPodsForJob(t *testing.T) {
	att := makeAttempt("attempt-x", "alice", "wf-a", "job-1", "running")
	pod := makePod("job-1-pod", "job-1", corev1.PodRunning)
	s := runsTestServer(att, pod)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	pods, err := s.listPodsForJob(req, "job-1")
	if err != nil {
		t.Fatalf("listPodsForJob: %v", err)
	}
	if len(pods) != 1 || pods[0].Name != "job-1-pod" {
		t.Errorf("pods = %v, want [job-1-pod]", pods)
	}
}
