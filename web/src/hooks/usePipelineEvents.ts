import { useEffect, useRef, useState } from "react";
import type { LifecycleEvent } from "../types";

/**
 * usePipelineEvents connects to the SSE endpoint for a pipeline and returns
 * a stream of lifecycle events in real-time.
 *
 * The SSE endpoint is GET /api/pipelines/{name}/events. It streams events
 * published by the graph executor via Dapr pub/sub → harmostes-ui daprd
 * subscription.
 *
 * EventSource automatically includes cookies (Authentik session), so auth
 * works transparently.
 */
export function usePipelineEvents(pipelineName: string | undefined) {
  const [events, setEvents] = useState<LifecycleEvent[]>([]);
  const [connected, setConnected] = useState(false);
  const esRef = useRef<EventSource | null>(null);

  useEffect(() => {
    if (!pipelineName) return;

    const es = new EventSource(`/api/pipelines/${encodeURIComponent(pipelineName)}/events`);
    esRef.current = es;

    es.onopen = () => setConnected(true);
    es.onerror = () => setConnected(false);
    es.onmessage = (e: MessageEvent) => {
      try {
        const ev = JSON.parse(e.data) as LifecycleEvent;
        setEvents((prev) => [...prev, ev]);
      } catch {
        // Ignore malformed events (heartbeats are comments, not messages).
      }
    };

    return () => {
      es.close();
      esRef.current = null;
      setConnected(false);
    };
  }, [pipelineName]);

  /** Clear accumulated events (e.g. when starting a fresh watch). */
  const clear = () => setEvents([]);

  return { events, connected, clear };
}
