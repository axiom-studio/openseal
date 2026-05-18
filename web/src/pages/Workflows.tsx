import { useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import { api, type WorkflowEntry } from '../api/client';

export default function Workflows() {
  const [workflows, setWorkflows] = useState<WorkflowEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [running, setRunning] = useState<string | null>(null);

  useEffect(() => {
    api.listWorkflows()
      .then(setWorkflows)
      .catch((e) => setError(e.message))
      .finally(() => setLoading(false));
  }, []);

  const runWorkflow = async (id: string) => {
    setRunning(id);
    try {
      await api.runWorkflow(id);
      alert('Workflow triggered. Check the Runs page for progress.');
    } catch (e: any) {
      alert('Failed to trigger: ' + e.message);
    } finally {
      setRunning(null);
    }
  };

  if (loading) return <div className="page-loading">Loading workflows...</div>;
  if (error) return <div className="page-error">Error: {error}</div>;

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1>Workflows</h1>
          <p className="page-subtitle">{workflows.length} workflow{workflows.length !== 1 ? 's' : ''} loaded</p>
        </div>
        <Link to="/builder" className="btn-primary">
          + New Workflow
        </Link>
      </header>
      <div className="grid">
        {workflows.map((wf) => (
          <div className="card workflow-card" key={wf.name}>
            <div className="card-header">
              <div className="workflow-name">{wf.name}</div>
              <div className="workflow-source" title={wf.source}>{wf.source.split('/').pop()}</div>
            </div>
            <div className="card-body">
              <div className="workflow-stats">
                <div className="stat">
                  <div className="stat-value">{wf.nodes.length}</div>
                  <div className="stat-label">Nodes</div>
                </div>
                <div className="stat">
                  <div className="stat-value">{wf.edges.length}</div>
                  <div className="stat-label">Edges</div>
                </div>
              </div>
              <div className="workflow-nodes">
                {wf.nodes.map((n) => (
                  <span className="node-tag" key={n.id}>
                    {n.type}
                  </span>
                ))}
              </div>
            </div>
            <div className="card-footer">
              <button
                className="btn-primary"
                onClick={() => runWorkflow(wf.name)}
                disabled={running === wf.name}
              >
                {running === wf.name ? 'Running...' : '▶ Run'}
              </button>
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}
