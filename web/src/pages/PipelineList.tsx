import { useEffect, useState } from "react";
import type { PipelineSummary } from "../types";
import { listPipelines, deletePipeline } from "../api";

export function PipelineList() {
  const [pipelines, setPipelines] = useState<PipelineSummary[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  async function load() {
    setLoading(true);
    setError("");
    try {
      setPipelines(await listPipelines());
    } catch (e) {
      setError(String(e));
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => {
    load();
  }, []);

  async function handleDelete(name: string) {
    if (!confirm(`Delete pipeline "${name}"?`)) return;
    try {
      await deletePipeline(name);
      load();
    } catch (e) {
      alert(`Delete failed: ${e}`);
    }
  }

  return (
    <div className="pipeline-list-page">
      <div className="pipeline-list-header">
        <h1>Pipelines</h1>
        <a href="/pipelines/new" className="btn btn-primary">
          + New Pipeline
        </a>
      </div>

      {loading && <p className="muted">Loading…</p>}
      {error && <p className="error">{error}</p>}

      {!loading && !error && pipelines.length === 0 && (
        <div className="empty-state">
          <p>No pipelines yet.</p>
          <a href="/pipelines/new" className="btn btn-primary">
            Create your first pipeline
          </a>
        </div>
      )}

      {!loading && pipelines.length > 0 && (
        <table className="pipeline-table">
          <thead>
            <tr>
              <th>Name</th>
              <th>Nodes</th>
              <th>Trigger</th>
              <th>Status</th>
              <th>Last Run</th>
              <th>Actions</th>
            </tr>
          </thead>
          <tbody>
            {pipelines.map((p) => (
              <tr key={p.name}>
                <td>
                  <a href={`/pipelines/${encodeURIComponent(p.name)}`} className="pipeline-name-link">
                    {p.name}
                  </a>
                </td>
                <td>{p.nodes}</td>
                <td>
                  <span className="badge badge-blue">{p.trigger}</span>
                </td>
                <td>
                  <span className={`badge badge-${statusBadgeClass(p.phase)}`}>
                    {p.phase || "idle"}
                  </span>
                </td>
                <td className="muted">{p.updatedAt || "—"}</td>
                <td>
                  <button className="btn btn-danger btn-sm" onClick={() => handleDelete(p.name)}>
                    Delete
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <div className="pipeline-list-footer">
        <a href="/workflows" className="link-muted">← Workflows</a>
      </div>
    </div>
  );
}

function statusBadgeClass(phase: string): string {
  switch (phase) {
    case "succeeded":
    case "green":
      return "green";
    case "failed":
      return "red";
    case "running":
      return "yellow";
    default:
      return "muted";
  }
}
