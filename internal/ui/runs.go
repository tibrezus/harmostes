package ui

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// Run logs fragment (the durable logs path)
//
// The Attempt is the run spine (ADR-0005); Jobs and pods are ephemeral
// (ADR-0007). Logs are therefore served as a per-run fragment inside the run
// detail page: live pod tail while the pod exists, an honest "pod gone" note
// (pointing at the persistent session transcript) once it doesn't.
// ---------------------------------------------------------------------------

// runLogsData is the template model for the run-logs fragment.
type runLogsData struct {
	AttemptName string
	RunName     string
	RunPhase    string // running | succeeded | failed | "" (unknown)
	PodName     string
	PodPhase    string
	ExitNote    string // "exit 0" style; empty while the container hasn't terminated
	Duration    string
	Logs        string
	LogsError   string
	PodGone     bool // pod GC'd — transcript is the durable log
}

// handleRunLogs renders the log fragment for one run of an attempt.
//
// Security: attemptOr404 gates ownership, and the run name must exist in the
// attempt's recorded run list. A forged name in the URL never reaches pod
// discovery.
func (s *Server) handleRunLogs(w http.ResponseWriter, r *http.Request) {
	attemptName := r.PathValue("name")
	jobName := r.PathValue("job")
	if attemptName == "" || jobName == "" {
		http.NotFound(w, r)
		return
	}

	att, ok := s.attemptOr404(w, r)
	if !ok {
		return
	}

	// The run must belong to this attempt — a forged job name in the URL
	// must not leak another run's logs.
	runPhase := ""
	for _, run := range att.Status.Runs {
		if run.Name == jobName {
			runPhase = run.Phase
			break
		}
	}
	if runPhase == "" {
		http.NotFound(w, r)
		return
	}

	data := runLogsData{
		AttemptName: attemptName,
		RunName:     jobName,
		RunPhase:    runPhase,
	}

	pods, err := s.listAttemptPods(r, attemptName)
	if err != nil {
		s.logger.Error("list pods for run logs", "attempt", attemptName, "err", err)
		data.LogsError = "Failed to list pods: " + err.Error()
		s.renderFragment(w, "pages/frag_run_logs.html", data)
		return
	}

	// RunRecord.Name is the pod name on the Job execution model (the worker
	// reads its own pod name via the downward API). Match exactly — a run
	// without a live matching pod is a recycled pod.
	var pod *corev1.Pod
	for i := range pods {
		if pods[i].Name == jobName {
			pod = &pods[i]
			break
		}
	}
	if pod == nil {
		// Pod GC'd (the normal terminal state under Job-per-attempt).
		// The session transcript persists in the state store — point there.
		data.PodGone = true
		s.renderFragment(w, "pages/frag_run_logs.html", data)
		return
	}

	data.PodName = pod.Name
	data.PodPhase = string(pod.Status.Phase)
	if code := podExitCode(*pod); code != nil {
		data.ExitNote = fmt.Sprintf("exit %d", *code)
	}
	data.Duration = podDuration(*pod)

	if s.logFetch == nil {
		data.LogsError = "log streaming not configured"
		s.renderFragment(w, "pages/frag_run_logs.html", data)
		return
	}
	logs, err := s.logFetch(r.Context(), s.namespace, pod.Name, resolveContainer(pod))
	if err != nil {
		s.logger.Error("fetch run logs", "pod", pod.Name, "err", err)
		data.LogsError = "Failed to fetch logs: " + err.Error()
	} else {
		data.Logs = formatLogs(logs)
	}
	s.renderFragment(w, "pages/frag_run_logs.html", data)
}

// renderFragmentString renders a template fragment to a string (for SSE
// delivery). Mirror of renderFragment for non-HTTP writers.
func (s *Server) renderFragmentString(name string, data any) (string, error) {
	tmpl := s.templates.Lookup(name)
	if tmpl == nil {
		return "", fmt.Errorf("template not found: %s", name)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render %s: %w", name, err)
	}
	return buf.String(), nil
}

// listAttemptPods lists the live pods of an attempt — BuildJob stamps the
// harmostes.dev/attempt label on the Job's pod template (there is no
// job-name label: k8s >= 1.31 no longer stamps it, and RunRecord.Name is
// the pod name, not the Job name). Absence is the normal terminal state.
func (s *Server) listAttemptPods(r *http.Request, attemptName string) ([]corev1.Pod, error) {
	var podList corev1.PodList
	if err := s.k8sClient.List(r.Context(), &podList,
		client.InNamespace(s.namespace),
		client.MatchingLabels{v1alpha1.AttemptLabel: attemptName},
	); err != nil {
		return nil, fmt.Errorf("list pods for attempt %s: %w", attemptName, err)
	}
	return podList.Items, nil
}

// resolveContainer picks the worker container to stream: the attempt Job
// names it "run"; legacy pool pods named it "worker". Falls back to the
// first container so a future rename degrades to best-effort, not failure.
func resolveContainer(pod *corev1.Pod) string {
	names := make(map[string]bool, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		names[c.Name] = true
	}
	for _, want := range []string{"run", "worker"} {
		if names[want] {
			return want
		}
	}
	if len(pod.Spec.Containers) > 0 {
		return pod.Spec.Containers[0].Name
	}
	return "run"
}

// renderFragment executes a page template standalone (no layout wrapper).
// Used by HTMX fragment endpoints: the response replaces a node in place.
func (s *Server) renderFragment(w http.ResponseWriter, name string, data any) {
	tmpl := s.templates.Lookup(name)
	if tmpl == nil {
		s.logger.Error("template not found", "page", name)
		http.Error(w, "template not found: "+name, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		s.logger.Error("render fragment", "page", name, "err", err)
	}
}

// ---------------------------------------------------------------------------
// Legacy URL compatibility
// ---------------------------------------------------------------------------

// handleWorkflowRunRedirect resolves the legacy Job-page URL
// (/workflows/{wf}/runs/{job}) to its Attempt-scoped home (/runs/{attempt}).
// Jobs are ephemeral under Job-per-attempt; the Attempt is the durable spine,
// so the old deep link is answered by the attempt that recorded the run —
// not by a dead Job object (the source of the UI's dead links).
//
// Security: the resolving attempt must belong to the requesting user; a
// known Job name without an owned attempt renders 404 (no leakage).
func (s *Server) handleWorkflowRunRedirect(w http.ResponseWriter, r *http.Request) {
	workflowName := r.PathValue("name")
	jobName := r.PathValue("job")
	if workflowName == "" || jobName == "" {
		http.NotFound(w, r)
		return
	}

	owner := s.visibleOwner(identityFromContext(r.Context()))
	var atts v1alpha1.AttemptList
	listOpts := []client.ListOption{
		client.InNamespace(s.namespace),
		client.MatchingLabels{v1alpha1.WorkflowLabel: workflowName},
	}
	if owner != "" {
		listOpts = append(listOpts, client.MatchingLabels{v1alpha1.OwnerLabel: owner})
	}
	if err := s.k8sClient.List(r.Context(), &atts,
		listOpts...,
	); err != nil {
		s.logger.Error("list attempts for run redirect", "workflow", workflowName, "err", err)
		s.renderError(w, r, "Failed to resolve run: "+err.Error())
		return
	}
	for _, att := range atts.Items {
		for _, run := range att.Status.Runs {
			if run.Name == jobName {
				http.Redirect(w, r, "/runs/"+att.Name, http.StatusSeeOther)
				return
			}
		}
	}
	http.NotFound(w, r)
}

// redirectAttempts answers the pre-rename /attempts... URLs. The spine is
// presented as "Runs"; every old deep link keeps working — the path prefix
// is rewritten, the rest of the path and the query string ride along.
func redirectAttempts(w http.ResponseWriter, r *http.Request) {
	target := strings.Replace(r.URL.Path, "/attempts", "/runs", 1)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Pod helpers (shared by the logs fragment)
// ---------------------------------------------------------------------------

// podExitCode extracts the worker container's exit code from the pod's
// container statuses. Returns nil if the container hasn't terminated yet.
func podExitCode(pod corev1.Pod) *int32 {
	want := resolveContainer(&pod)
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != want {
			continue
		}
		if cs.LastTerminationState.Terminated != nil {
			code := cs.LastTerminationState.Terminated.ExitCode
			return &code
		}
		if cs.State.Terminated != nil {
			code := cs.State.Terminated.ExitCode
			return &code
		}
	}
	return nil
}

// formatLogs transforms raw slog JSON log lines into a readable format:
// "LEVEL  msg  key=val  key=val"
// Non-JSON lines (e.g., plugin stderr) are passed through as-is.
func formatLogs(raw string) string {
	if raw == "" {
		return ""
	}
	lines := strings.Split(raw, "\n")
	var b strings.Builder
	for _, line := range lines {
		if line == "" {
			continue
		}
		// Quick check: JSON lines start with '{'
		if strings.HasPrefix(line, "{") {
			if formatted, ok := formatJSONLogLine(line); ok {
				b.WriteString(formatted)
				b.WriteByte('\n')
				continue
			}
		}
		// Pass through non-JSON lines (plugin output, stderr, etc.)
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// podDuration returns the pod's active span: start → now or container finish.
func podDuration(pod corev1.Pod) string {
	if pod.Status.StartTime == nil {
		return "—"
	}
	end := time.Now()
	if f := podFinishTime(pod); !f.IsZero() {
		end = f
	}
	return formatDuration(end.Sub(pod.Status.StartTime.Time))
}

// podFinishTime returns the latest container finish time of a pod, or zero.
func podFinishTime(pod corev1.Pod) time.Time {
	var latest time.Time
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil && cs.State.Terminated.FinishedAt.After(latest) {
			latest = cs.State.Terminated.FinishedAt.Time
		}
	}
	return latest
}
