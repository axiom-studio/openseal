import { useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import { api, type RunRecord } from '../api/client';

function statusClass(status: string) {
  switch (status) {
    case 'completed': return 'status-completed';
    case 'failed': return 'status-failed';
    case 'running': return 'status-running';
    default: return 'status-pending';
  }
}

export default function Runs() {
  const [runs, setRuns] = useState<RunRecord[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');

  useEffect(() => {
    api.listRuns()
      .then(setRuns)
      .catch((e) => setError(e.message))
      .finally(() => setLoading(false));

    const interval = setInterval(() => {
      api.listRuns().then(setRuns).catch(() => {});
    }, 2000);
    return () => clearInterval(interval);
  }, []);

  if (loading) return <div className="page-loading">Loading runs...</div>;
  if (error) return <div className="page-error">Error: {error}</div>;

  return (
    <div className="page">
      <header className="page-header">
        <h1>Runs</h1>
        <p className="page-subtitle">Execution history refreshes every 2s</p>
      </header>
      <div className="runs-table-wrap">
        <table className="runs-table">
          <thead>
            <tr>
              <th>Run ID</th>
              <th>Workflow</th>
              <th>Status</th>
              <th>Started</th>
              <th>Duration</th>
            </tr>
          </thead>
          <tbody>
            {runs.map((run) => (
              <tr key={run.runId}>
                <td>
                  <Link to={`/runs/${run.runId}`} className="run-link">
                    #{run.runId}
                  </Link>
                </td>
                <td>{run.workflowName}</td>
                <td>
                  <span className={`status-badge ${statusClass(run.status)}`}>
                    {run.status}
                  </span>
                </td>
                <td>{new Date(run.startedAt).toLocaleString()}</td>
                <td>
                  {run.completedAt
                    ? `${((new Date(run.completedAt).getTime() - new Date(run.startedAt).getTime()) / 1000).toFixed(1)}s`
                    : '—'}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {runs.length === 0 && (
          <div className="empty-state">No runs yet. Trigger a workflow to see results.</div>
        )}
      </div>
    </div>
  );
}
