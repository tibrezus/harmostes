package ui

import (
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
	ExitCode    *int32
	Duration    string
	Logs        string
	LogsError   string
	PodGone     bool // pod GC'd — transcript is the durable log
}

// handleRunLogs renders the log fragment for one run of an attempt.
//
// Security: the Attempt lookup IS the gate — only the attempt owner reaches
// this handler (same chain as the run detail page). A user who knows a Job
// name but owns no attempt containing it gets 404.
func (s *Server) handleRunLogs(w http.ResponseWriter, r *http.Request) {
	attemptName := r.PathValue("name")
	jobName := r.PathValue("job")
	if attemptName == "" || jobName == "" {
		http.NotFound(w, r)
		return
	}

	att, err := s.getAttempt(r, attemptName)
	if err != nil {
		s.renderError(w, r, "Failed to get attempt: "+err.Error())
		return
	}
	if att.Labels[v1alpha1.OwnerLabel] != identityFromContext(r.Context()).Username {
		http.NotFound(w, r)
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

	pods, err := s.listPodsForJob(r, jobName)
	if err != nil {
		s.logger.Error("list pods for run logs", "job", jobName, "err", err)
		data.LogsError = "Failed to list pods: " + err.Error()
		s.renderFragment(w, "pages/frag_run_logs.html", data)
		return
	}

	if len(pods) == 0 {
		// Pod GC'd (the normal terminal state under Job-per-attempt).
		// The session transcript persists in the state store — point there.
		data.PodGone = true
		s.renderFragment(w, "pages/frag_run_logs.html", data)
		return
	}

	pod := selectPod(pods)
	data.PodName = pod.Name
	data.PodPhase = string(pod.Status.Phase)
	data.ExitCode = podExitCode(pod)
	data.Duration = podDuration(pod)

	if s.logFetch == nil {
		data.LogsError = "log streaming not configured"
		s.renderFragment(w, "pages/frag_run_logs.html", data)
		return
	}
	logs, err := s.logFetch(r.Context(), s.namespace, pod.Name, "worker")
	if err != nil {
		s.logger.Error("fetch run logs", "pod", pod.Name, "err", err)
		data.LogsError = "Failed to fetch logs: " + err.Error()
	} else {
		data.Logs = formatLogs(logs)
	}
	s.renderFragment(w, "pages/frag_run_logs.html", data)
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

	owner := identityFromContext(r.Context()).Username
	var atts v1alpha1.AttemptList
	if err := s.k8sClient.List(r.Context(), &atts,
		client.InNamespace(s.namespace),
		client.MatchingLabels{v1alpha1.OwnerLabel: owner, v1alpha1.WorkflowLabel: workflowName},
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

// selectPod picks the most relevant pod from a list: prefers a running pod,
// otherwise the one with the latest creation timestamp.
func selectPod(pods []corev1.Pod) corev1.Pod {
	var best corev1.Pod
	for _, p := range pods {
		if p.Status.Phase == corev1.PodRunning {
			return p
		}
		if best.Name == "" || p.CreationTimestamp.After(best.CreationTimestamp.Time) {
			best = p
		}
	}
	return best
}

// podExitCode extracts the worker container's exit code from the pod's
// container statuses. Returns nil if the container hasn't terminated yet.
func podExitCode(pod corev1.Pod) *int32 {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != "worker" {
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
