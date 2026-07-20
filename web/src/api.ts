import type { Pipeline, PipelineSummary, PipelineSpec } from "./types";

const API_BASE = "/api/pipelines";

async function fetchJSON<T>(url: string, init?: RequestInit): Promise<T> {
  const resp = await fetch(url, {
    ...init,
    headers: {
      "Content-Type": "application/json",
      ...(init?.headers || {}),
    },
  });
  if (!resp.ok) {
    const body = await resp.json().catch(() => ({ error: resp.statusText }));
    throw new Error(body.error || `HTTP ${resp.status}`);
  }
  return resp.json();
}

export async function listPipelines(): Promise<PipelineSummary[]> {
  const data = await fetchJSON<{ pipelines: PipelineSummary[] }>(API_BASE);
  return data.pipelines;
}

export async function getPipeline(name: string): Promise<Pipeline> {
  return fetchJSON<Pipeline>(`${API_BASE}/${name}`);
}

export async function savePipeline(name: string, spec: PipelineSpec): Promise<Pipeline> {
  return fetchJSON<Pipeline>(`${API_BASE}/${name}`, {
    method: "PUT",
    body: JSON.stringify({ spec }),
  });
}

export async function deletePipeline(name: string): Promise<void> {
  await fetchJSON<{ status: string }>(`${API_BASE}/${name}`, {
    method: "DELETE",
  });
}
