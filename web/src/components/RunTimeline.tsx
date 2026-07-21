import { memo } from "react";
import type { LifecycleEvent } from "../types";

// RunTimeline shows the chronological sequence of lifecycle events for the
// current pipeline run. Each entry shows the event type, node name, status,
// and duration. This gives users a timeline view of execution progress.
export const RunTimeline = memo(function RunTimeline({
  events,
}: {
  events: LifecycleEvent[];
}) {
  if (events.length === 0) {
    return (
      <div className="timeline-empty">
        <span className="muted">Waiting for events…</span>
      </div>
    );
  }

  return (
    <div className="run-timeline">
      <div className="timeline-header">
        <span>Run Timeline</span>
        <span className="muted">({events.length} events)</span>
      </div>
      <div className="timeline-list">
        {events.map((ev, i) => (
          <TimelineEntry key={`${ev.timestamp}-${i}`} event={ev} />
        ))}
      </div>
    </div>
  );
});

function TimelineEntry({ event }: { event: LifecycleEvent }) {
  const time = formatTime(event.timestamp);
  const icon = eventIcon(event.event);
  const cls = eventClass(event.event);

  return (
    <div className={`timeline-entry timeline-entry--${cls}`}>
      <span className="timeline-time">{time}</span>
      <span className="timeline-icon">{icon}</span>
      <div className="timeline-content">
        <span className="timeline-event-type">{formatEventName(event.event)}</span>
        {event.node && <span className="timeline-node">{event.node}</span>}
        {event.status && (
          <span className={`timeline-status timeline-status--${event.status}`}>
            {event.status}
          </span>
        )}
        {event.durationMs !== undefined && event.durationMs > 0 && (
          <span className="timeline-duration">{formatDuration(event.durationMs)}</span>
        )}
        {event.feedback && (
          <div className="timeline-feedback">{truncate(event.feedback, 80)}</div>
        )}
      </div>
    </div>
  );
}

function eventIcon(event: string): string {
  switch (event) {
    case "pipeline.started": return "▶";
    case "pipeline.completed": return "✓";
    case "pipeline.failed": return "✕";
    case "node.started": return "●";
    case "node.completed": return "✓";
    case "node.failed": return "✕";
    default: return "•";
  }
}

function eventClass(event: string): string {
  if (event.includes("failed")) return "failed";
  if (event.includes("completed")) return "green";
  if (event.includes("started")) return "running";
  return "pending";
}

function formatEventName(event: string): string {
  return event.replace(/[.]/g, " → ");
}

function formatTime(ts: string): string {
  try {
    const d = new Date(ts);
    return d.toLocaleTimeString("en-US", { hour12: false, hour: "2-digit", minute: "2-digit", second: "2-digit" });
  } catch {
    return ts;
  }
}

function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)}s`;
  return `${Math.floor(ms / 60000)}m${Math.floor((ms % 60000) / 1000)}s`;
}

function truncate(s: string, n: number): string {
  if (s.length <= n) return s;
  return s.slice(0, n) + "…";
}
