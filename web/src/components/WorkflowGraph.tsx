import { useMemo } from 'react';
import type { WorkflowNode, WorkflowEdge } from '../api/client';

interface GraphProps {
  nodes: WorkflowNode[];
  edges: WorkflowEdge[];
}

interface NodePos {
  id: string;
  type: string;
  x: number;
  y: number;
  config?: Record<string, unknown>;
}

function computeLayout(nodes: WorkflowNode[], edges: WorkflowEdge[]): NodePos[] {
  if (nodes.length === 0) return [];

  const incoming = new Map<string, number>();
  const outgoing = new Map<string, string[]>();
  for (const n of nodes) {
    incoming.set(n.id, 0);
    outgoing.set(n.id, []);
  }
  for (const e of edges) {
    incoming.set(e.to, (incoming.get(e.to) || 0) + 1);
    outgoing.set(e.from, [...(outgoing.get(e.from) || []), e.to]);
  }

  // Topological levels via Kahn's algorithm
  const levels = new Map<string, number>();
  const queue: string[] = [];
  for (const [id, count] of incoming) {
    if (count === 0) {
      queue.push(id);
      levels.set(id, 0);
    }
  }

  let qi = 0;
  while (qi < queue.length) {
    const id = queue[qi++];
    const level = levels.get(id) || 0;
    for (const next of outgoing.get(id) || []) {
      const newLevel = Math.max(levels.get(next) || 0, level + 1);
      levels.set(next, newLevel);
      const rem = (incoming.get(next) || 0) - 1;
      incoming.set(next, rem);
      if (rem === 0) queue.push(next);
    }
  }

  // Any remaining nodes (cycles) get max level + 1
  let maxLevel = 0;
  for (const v of levels.values()) maxLevel = Math.max(maxLevel, v);
  for (const n of nodes) {
    if (!levels.has(n.id)) levels.set(n.id, maxLevel + 1);
  }

  // Group by level
  const byLevel = new Map<number, string[]>();
  for (const [id, lvl] of levels) {
    const arr = byLevel.get(lvl) || [];
    arr.push(id);
    byLevel.set(lvl, arr);
  }

  const nodeMap = new Map(nodes.map((n) => [n.id, n]));
  const positions: NodePos[] = [];
  const colWidth = 180;
  const rowHeight = 90;
  const nodeW = 140;
  const nodeH = 56;

  for (const [lvl, ids] of byLevel) {
    ids.forEach((id, idx) => {
      const n = nodeMap.get(id);
      if (!n) return;
      const x = lvl * colWidth + (colWidth - nodeW) / 2;
      const y = idx * rowHeight + (rowHeight - nodeH) / 2;
      positions.push({ id, type: n.type, x, y, config: n.config });
    });
  }

  return positions;
}

export default function WorkflowGraph({ nodes, edges }: GraphProps) {
  const positions = useMemo(() => computeLayout(nodes, edges), [nodes, edges]);
  const posMap = useMemo(() => new Map(positions.map((p) => [p.id, p])), [positions]);

  const width = useMemo(() => {
    if (positions.length === 0) return 300;
    return Math.max(300, Math.max(...positions.map((p) => p.x)) + 200);
  }, [positions]);

  const height = useMemo(() => {
    if (positions.length === 0) return 200;
    return Math.max(200, Math.max(...positions.map((p) => p.y)) + 100);
  }, [positions]);

  const nodeW = 140;
  const nodeH = 56;
  const rx = 8;

  const typeColor: Record<string, string> = {
    webhook: '#f59e0b',
    cron: '#8b5cf6',
    manual: '#06b6d4',
    http: '#10b981',
    ai: '#ef4444',
    code: '#3b82f6',
    transform: '#ec4899',
    if: '#f97316',
    switch: '#f97316',
    slack: '#6366f1',
    discord: '#8b5cf6',
    pgvector: '#14b8a6',
  };

  return (
    <svg
      viewBox={`0 0 ${width} ${height}`}
      className="workflow-graph"
      style={{ width: '100%', height: '100%', minHeight: '200px' }}
    >
      <defs>
        <marker id="arrowhead" markerWidth="8" markerHeight="6" refX="7" refY="3" orient="auto">
          <polygon points="0 0, 8 3, 0 6" fill="#525252" />
        </marker>
      </defs>

      {edges.map((e, i) => {
        const from = posMap.get(e.from);
        const to = posMap.get(e.to);
        if (!from || !to) return null;
        const fx = from.x + nodeW;
        const fy = from.y + nodeH / 2;
        const tx = to.x;
        const ty = to.y + nodeH / 2;
        const mx = (fx + tx) / 2;
        return (
          <g key={`edge-${i}`}>
            <path
              d={`M ${fx} ${fy} C ${mx} ${fy}, ${mx} ${ty}, ${tx} ${ty}`}
              fill="none"
              stroke="#333"
              strokeWidth={1.5}
              markerEnd="url(#arrowhead)"
            />
            {e.condition && (
              <text x={mx} y={(fy + ty) / 2 - 6} textAnchor="middle" fill="#8a8a8a" fontSize={10}>
                {e.condition}
              </text>
            )}
          </g>
        );
      })}

      {positions.map((p) => {
        const color = typeColor[p.type] || '#525252';
        return (
          <g key={p.id} transform={`translate(${p.x}, ${p.y})`}>
            <rect width={nodeW} height={nodeH} rx={rx} fill="#0e0e0e" stroke={color} strokeWidth={1.5} />
            <rect x={0} y={0} width={4} height={nodeH} rx={rx} fill={color} />
            <text x={nodeW / 2} y={22} textAnchor="middle" fill="#e8e8e8" fontSize={12} fontWeight={600}>
              {p.id}
            </text>
            <text x={nodeW / 2} y={40} textAnchor="middle" fill="#8a8a8a" fontSize={10}>
              {p.type}
            </text>
          </g>
        );
      })}
    </svg>
  );
}
