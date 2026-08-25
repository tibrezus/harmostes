package ui

import (
	"net/http"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// runDetailData is the template model for the run-detail page.
type runDetailData struct {
	Workflow  v1alpha1.Workflow
	Job       batchv1.Job
	PodName   string
	PodPhase  string
	Logs      string
	LogsError string
	Duration  string
	ExitCode  *int32
	HasLogs   bool
}

// handleRunDetail renders a single Job run with its pod logs. Security:
//  1. The Workflow CR must be owned by the authenticated user (owner label check).
//  2. The Job must belong to that workflow (harmostes.dev/workflow label match).
//
// This chain ensures a user can never view another user's run logs, even if
// they know the Job name — the workflow ownership check gates the entire page.
func (s *Server) handleRunDetail(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context()).Username
	workflowName := r.PathValue("name")
	jobName := r.PathValue("job")
	if workflowName == "" || jobName == "" {
		http.NotFound(w, r)
		return
	}

	// Gate 1: workflow must exist and be owned by the user.
	var wf v1alpha1.Workflow
	if err := s.k8sClient.Get(r.Context(), client.ObjectKey{Namespace: s.namespace, Name: workflowName}, &wf); err != nil {
		if errors.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		s.logger.Error("get workflow for run detail", "workflow", workflowName, "err", err)
		s.renderError(w, r, "Failed to load workflow: "+err.Error())
		return
	}
	if wf.Labels[v1alpha1.OwnerLabel] != owner {
		http.NotFound(w, r)
		return
	}

	// Gate 2: the Job must exist and belong to this workflow.
	var job batchv1.Job
	err := s.k8sClient.Get(r.Context(), client.ObjectKey{Namespace: s.namespace, Name: jobName}, &job)
	if errors.IsNotFound(err) {
		// Worker-pool era: runs are shared pool pods, not per-run Jobs.
		s.renderPoolRun(w, r, wf, jobName)
		return
	}
	if err != nil {
		s.logger.Error("get job for run detail", "job", jobName, "err", err)
		s.renderError(w, r, "Failed to load run: "+err.Error())
		return
	}
	if job.Labels[v1alpha1.WorkflowLabel] != workflowName {
		// The Job exists but belongs to a different workflow — treat as not found
		// to avoid information leakage.
		http.NotFound(w, r)
		return
	}

	data := runDetailData{
		Workflow: wf,
		Job:      job,
		Duration: jobDuration(job),
		HasLogs:  s.logFetch != nil,
	}

	// Find the pod(s) for this job and fetch logs from the worker container.
	pods, err := s.listPodsForJob(r, jobName)
	if err != nil {
		s.logger.Error("list pods for job", "job", jobName, "err", err)
		data.LogsError = "Failed to list pods: " + err.Error()
		s.render(w, r, "pages/run_detail.html", data)
		return
	}

	if len(pods) > 0 {
		// Use the most relevant pod: prefer running, then the latest by creation time.
		pod := selectPod(pods)
		data.PodName = pod.Name
		data.PodPhase = string(pod.Status.Phase)
		data.ExitCode = podExitCode(pod)

		if s.logFetch != nil {
			logs, err := s.logFetch(r.Context(), s.namespace, pod.Name, "worker")
			if err != nil {
				s.logger.Error("fetch pod logs", "pod", pod.Name, "err", err)
				data.LogsError = "Failed to fetch logs: " + err.Error()
			} else {
				data.Logs = formatLogs(logs)
			}
		}
	}

	s.render(w, r, "pages/run_detail.html", data)
}

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

// jobDuration returns a human-readable duration string for a Job.
func jobDuration(job batchv1.Job) string {
	if job.Status.StartTime == nil {
		return "—"
	}
	end := job.Status.CompletionTime
	if end == nil {
		// Still running — compute from now (approximate).
		return "running…"
	}
	d := end.Sub(job.Status.StartTime.Time)
	if d < 0 {
		return "—"
	}
	return d.Truncate(1_000_000_000).String()
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

// renderPoolRun serves run detail for worker-pool runs. Pool pods are shared
// across workflows (and therefore potentially across owners), so the Job-era
// workflow-label check proves nothing here. Ownership is established through
// the Attempt chain instead: the requesting user must own an Attempt that
// lists this run, which is exactly how the UI linked here in the first place.
func (s *Server) renderPoolRun(w http.ResponseWriter, r *http.Request, wf v1alpha1.Workflow, runName string) {
	owner := identityFromContext(r.Context()).Username

	var atts v1alpha1.AttemptList
	if err := s.k8sClient.List(r.Context(), &atts, client.InNamespace(s.namespace), client.MatchingLabels{v1alpha1.OwnerLabel: owner}); err != nil {
		s.logger.Error("list attempts for pool run", "err", err)
		s.renderError(w, r, "Failed to load run: "+err.Error())
		return
	}
	owned := false
	for _, att := range atts.Items {
		// Restore the Job-era Gate 2 workflow dimension: the owning attempt
		// must belong to the workflow in the URL (attempts carry the same
		// harmostes.dev/workflow label Jobs did).
		if att.Labels[v1alpha1.WorkflowLabel] != wf.Name {
			continue
		}
		for _, run := range att.Status.Runs {
			if run.Name == runName {
				owned = true
				break
			}
		}
		if owned {
			break
		}
	}
	if !owned {
		http.NotFound(w, r)
		return
	}

	var pod corev1.Pod
	if err := s.k8sClient.Get(r.Context(), client.ObjectKey{Namespace: s.namespace, Name: runName}, &pod); err != nil {
		if errors.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		s.logger.Error("get pool pod for run detail", "pod", runName, "err", err)
		s.renderError(w, r, "Failed to load run: "+err.Error())
		return
	}

	// Synthesize the Job view fields from the pod so run_detail.html renders
	// one surface for both eras. Timing and identity come from the pod itself.
	job := batchv1.Job{}
	job.Name = pod.Name
	if pod.Status.StartTime != nil {
		job.Status.StartTime = pod.Status.StartTime
	}
	data := runDetailData{
		Workflow: wf,
		Job:      job,
		PodName:  pod.Name,
		PodPhase: string(pod.Status.Phase),
		ExitCode: podExitCode(pod),
		Duration: podDuration(pod),
		HasLogs:  s.logFetch != nil,
	}
	if s.logFetch != nil {
		logs, err := s.logFetch(r.Context(), s.namespace, pod.Name, "worker")
		if err != nil {
			s.logger.Error("fetch pool pod logs", "pod", pod.Name, "err", err)
			data.LogsError = "Failed to fetch logs: " + err.Error()
		} else {
			data.Logs = formatLogs(logs)
		}
	}
	s.render(w, r, "pages/run_detail.html", data)
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
