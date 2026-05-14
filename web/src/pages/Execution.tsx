import { useEffect, useState } from 'react';
import { useParams, Link } from 'react-router-dom';
import { api, type RunRecord, type NodeResult } from '../api/client';

function statusClass(status: string) {
  switch (status) {
    case 'completed': return 'status-completed';
    case 'failed': return 'status-failed';
    case 'running': return 'status-running';
    default: return 'status-pending';
  }
}

function formatJSON(v: unknown) {
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}

export default function Execution() {
  const { id } = useParams<{ id: string }>();
  const [run, setRun] = useState<RunRecord | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');

  useEffect(() => {
    if (!id) return;
    const load = () => {
      api.getRun(Number(id))
        .then(setRun)
        .catch((e) => setError(e.message))
        .finally(() => setLoading(false));
    };
    load();
    const interval = setInterval(load, 1500);
    return () => clearInterval(interval);
  }, [id]);

  if (loading) return <div className="page-loading">Loading run...</div>;
  if (error) return <div className="page-error">Error: {error}</div>;
  if (!run) return <div className="page-error">Run not found</div>;

  const nodeResults: NodeResult[] = Object.values(run.nodeResults || {})
    .sort((a, b) => a.executionOrder - b.executionOrder);

  return (
    <div className="page">
      <header className="page-header">
        <div className="breadcrumb">
          <Link to="/runs">Runs</Link>
          <span> / </span>
          <span>Run #{run.runId}</span>
        </div>
        <h1>{run.workflowName}</h1>
        <div className="run-meta">
          <span className={`status-badge ${statusClass(run.status)}`}>{run.status}</span>
          <span className="meta-item">Started: {new Date(run.startedAt).toLocaleString()}</span>
          {run.completedAt && (
            <span className="meta-item">
              Duration: {((new Date(run.completedAt).getTime() - new Date(run.startedAt).getTime()) / 1000).toFixed(1)}s
            </span>
          )}
        </div>
        {run.error && <div className="run-error">{run.error}</div>}
      </header>

      <div className="node-results">
        {nodeResults.map((nr) => (
          <div className="node-result-card" key={nr.nodeId}>
            <div className="node-result-header">
              <div className="node-result-title">
                <span className="node-type-tag">{nr.nodeType}</span>
                <span className="node-name">{nr.nodeName}</span>
              </div>
              <span className={`status-badge ${statusClass(nr.status)}`}>{nr.status}</span>
            </div>
            <div className="node-result-body">
              {nr.error && <div className="node-error">{nr.error}</div>}
              {nr.output !== undefined && nr.output !== null && (
                <div className="code-block-wrap">
                  <div className="code-block-label">Output</div>
                  <pre className="code-block"><code>{formatJSON(nr.output)}</code></pre>
                </div>
              )}
              {nr.input !== undefined && nr.input !== null && (
                <div className="code-block-wrap">
                  <div className="code-block-label">Input</div>
                  <pre className="code-block"><code>{formatJSON(nr.input)}</code></pre>
                </div>
              )}
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}