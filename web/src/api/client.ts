const API_BASE = import.meta.env.VITE_API_BASE || '';

async function fetchJSON<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${API_BASE}${path}`, {
    headers: { 'Content-Type': 'application/json' },
    ...init,
  });
  if (!res.ok) {
    const err = await res.text();
    throw new Error(err || `HTTP ${res.status}`);
  }
  return res.json() as Promise<T>;
}

export interface WorkflowEntry {
  name: string;
  source: string;
  nodes: WorkflowNode[];
  edges: WorkflowEdge[];
  config?: Record<string, unknown>;
}

export interface WorkflowNode {
  id: string;
  type: string;
  config?: Record<string, unknown>;
}

export interface WorkflowEdge {
  from: string;
  to: string;
  condition?: string;
}

export interface ExecutorInfo {
  type: string;
  name: string;
  category: string;
  description: string;
  icon: string;
  inputSchema?: Record<string, unknown>;
  outputSchema?: Record<string, unknown>;
}

export interface RunRecord {
  runId: number;
  workflowName: string;
  status: string;
  nodeResults?: Record<string, NodeResult>;
  startedAt: string;
  completedAt?: string;
  error?: string;
}

export interface NodeResult {
  nodeId: string;
  nodeName: string;
  nodeType: string;
  status: string;
  input?: unknown;
  output?: unknown;
  error: string;
  startedAt: string;
  completedAt: string;
  duration: number;
  executionOrder: number;
}

export const api = {
  health: () => fetchJSON<{ status: string }>('/api/v1/health'),
  listWorkflows: () => fetchJSON<WorkflowEntry[]>('/api/v1/workflows'),
  getWorkflow: (id: string) => fetchJSON<WorkflowEntry>(`/api/v1/workflows/${encodeURIComponent(id)}`),
  runWorkflow: (id: string) =>
    fetchJSON<{ runId: number; status: string; workflow: string }>(`/api/v1/workflows/${encodeURIComponent(id)}/run`, {
      method: 'POST',
    }),
  listRuns: () => fetchJSON<RunRecord[]>('/api/v1/runs'),
  getRun: (id: number) => fetchJSON<RunRecord>(`/api/v1/runs/${id}`),
  listSkills: () => fetchJSON<ExecutorInfo[]>('/api/v1/skills'),
  getSkill: (id: string) => fetchJSON<ExecutorInfo>(`/api/v1/skills/${encodeURIComponent(id)}`),
};
