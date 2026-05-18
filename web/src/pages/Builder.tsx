import { useEffect, useRef, useState, useCallback } from 'react';
import { api, type WorkflowEntry, type ExecutorInfo } from '../api/client';

interface CanvasNode {
  id: string;
  type: string;
  x: number;
  y: number;
  config: Record<string, unknown>;
}

interface CanvasEdge {
  id: string;
  from: string;
  to: string;
}

type DragState =
  | { kind: 'idle' }
  | { kind: 'node'; nodeId: string; offsetX: number; offsetY: number }
  | { kind: 'connect'; fromId: string };

const NODE_W = 168;
const NODE_H = 72;
const PORT_R = 5;

export default function Builder() {
  const [workflowName, setWorkflowName] = useState('untitled');
  const [nodes, setNodes] = useState<CanvasNode[]>([]);
  const [edges, setEdges] = useState<CanvasEdge[]>([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [drag, setDrag] = useState<DragState>({ kind: 'idle' });
  const [mouse, setMouse] = useState({ x: 0, y: 0 });
  const [skills, setSkills] = useState<ExecutorInfo[]>([]);
  const [saving, setSaving] = useState(false);
  const [validation, setValidation] = useState<string | null>(null);
  const canvasRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    api.listSkills().then(setSkills).catch(() => {});
  }, []);

  // Keyboard shortcuts
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Delete' || e.key === 'Backspace') {
        if (selectedId) {
          deleteNode(selectedId);
        }
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [selectedId, nodes, edges]);

  const toWorkflow = useCallback((): WorkflowEntry => ({
    name: workflowName,
    source: `${workflowName}.hcl`,
    nodes: nodes.map((n) => ({ id: n.id, type: n.type, config: n.config })),
    edges: edges.map((e) => ({ from: e.from, to: e.to })),
  }), [workflowName, nodes, edges]);

  const addNode = useCallback((type: string, x: number, y: number) => {
    const count = nodes.filter((n) => n.type === type).length;
    const id = `${type}_${count + 1}`;
    setNodes((prev) => [...prev, { id, type, x: x - NODE_W / 2, y: y - NODE_H / 2, config: {} }]);
    setSelectedId(id);
  }, [nodes]);

  const deleteNode = useCallback((id: string) => {
    setNodes((prev) => prev.filter((n) => n.id !== id));
    setEdges((prev) => prev.filter((e) => e.from !== id && e.to !== id));
    if (selectedId === id) setSelectedId(null);
  }, [selectedId]);

  const updateNodeConfig = useCallback((id: string, config: Record<string, unknown>) => {
    setNodes((prev) => prev.map((n) => (n.id === id ? { ...n, config } : n)));
  }, []);

  const updateNodeId = useCallback((oldId: string, newId: string) => {
    if (!newId || newId === oldId) return;
    if (nodes.some((n) => n.id === newId)) return;
    setNodes((prev) => prev.map((n) => (n.id === oldId ? { ...n, id: newId } : n)));
    setEdges((prev) => prev.map((e) => ({
      ...e,
      from: e.from === oldId ? newId : e.from,
      to: e.to === oldId ? newId : e.to,
    })));
    setSelectedId(newId);
  }, [nodes]);

  const handleDrop = (e: React.DragEvent) => {
    e.preventDefault();
    const type = e.dataTransfer.getData('skillType');
    if (!type || !canvasRef.current) return;
    const rect = canvasRef.current.getBoundingClientRect();
    addNode(type, e.clientX - rect.left, e.clientY - rect.top);
  };

  const handleCanvasMouseDown = (e: React.MouseEvent) => {
    if (e.target === canvasRef.current || (e.target as HTMLElement).classList.contains('canvas-grid')) {
      setSelectedId(null);
    }
  };

  const handleNodeMouseDown = (e: React.MouseEvent, node: CanvasNode) => {
    e.stopPropagation();
    const target = e.target as HTMLElement;
    if (target.classList.contains('port-output')) {
      setDrag({ kind: 'connect', fromId: node.id });
      setSelectedId(node.id);
      return;
    }
    setSelectedId(node.id);
    setDrag({
      kind: 'node',
      nodeId: node.id,
      offsetX: e.clientX,
      offsetY: e.clientY,
    });
  };

  const handleMouseMove = (e: React.MouseEvent) => {
    if (!canvasRef.current) return;
    const rect = canvasRef.current.getBoundingClientRect();
    const x = e.clientX - rect.left;
    const y = e.clientY - rect.top;
    setMouse({ x, y });

    if (drag.kind === 'node') {
      const dx = e.clientX - drag.offsetX;
      const dy = e.clientY - drag.offsetY;
      setNodes((prev) =>
        prev.map((n) =>
          n.id === drag.nodeId ? { ...n, x: n.x + dx, y: n.y + dy } : n
        )
      );
      setDrag({ ...drag, offsetX: e.clientX, offsetY: e.clientY });
    }
  };

  const handleMouseUp = (e: React.MouseEvent) => {
    if (drag.kind === 'connect' && canvasRef.current) {
      const rect = canvasRef.current.getBoundingClientRect();
      const mx = e.clientX - rect.left;
      const my = e.clientY - rect.top;
      // Find if we dropped on a node's input port
      for (const n of nodes) {
        if (n.id === drag.fromId) continue;
        const px = n.x + NODE_W / 2;
        const py = n.y;
        const dist = Math.hypot(mx - px, my - py);
        if (dist < 20) {
          const exists = edges.some((ed) => ed.from === drag.fromId && ed.to === n.id);
          if (!exists) {
            setEdges((prev) => [
              ...prev,
              { id: `${drag.fromId}-${n.id}-${Date.now()}`, from: drag.fromId, to: n.id },
            ]);
          }
          break;
        }
      }
    }
    setDrag({ kind: 'idle' });
  };

  const handleSave = async () => {
    if (nodes.length === 0) {
      setValidation('Add at least one node before saving.');
      return;
    }
    setSaving(true);
    setValidation(null);
    try {
      await api.createWorkflow(toWorkflow());
      setValidation('Workflow saved successfully.');
    } catch (err: any) {
      setValidation(`Save failed: ${err.message}`);
    } finally {
      setSaving(false);
    }
  };

  const handleValidate = async () => {
    setValidation(null);
    try {
      const res = await api.validateWorkflow(toWorkflow());
      setValidation(res.valid ? 'Workflow is valid.' : `Invalid: ${res.issues.map((i) => i.message).join('; ')}`);
    } catch (err: any) {
      setValidation(`Validation error: ${err.message}`);
    }
  };

  const handleAutoLayout = () => {
    if (nodes.length === 0) return;
    const levels = new Map<string, number>();
    const incoming = new Map<string, number>();
    const adj = new Map<string, string[]>();
    for (const n of nodes) {
      incoming.set(n.id, 0);
      adj.set(n.id, []);
    }
    for (const e of edges) {
      incoming.set(e.to, (incoming.get(e.to) || 0) + 1);
      adj.set(e.from, [...(adj.get(e.from) || []), e.to]);
    }
    const queue: string[] = [];
    for (const [id, count] of incoming) {
      if (count === 0) queue.push(id);
    }
    let qi = 0;
    while (qi < queue.length) {
      const id = queue[qi++];
      const lvl = levels.get(id) || 0;
      for (const next of adj.get(id) || []) {
        levels.set(next, Math.max(levels.get(next) || 0, lvl + 1));
        incoming.set(next, (incoming.get(next) || 0) - 1);
        if (incoming.get(next) === 0) queue.push(next);
      }
    }
    const byLevel = new Map<number, string[]>();
    let maxLevel = 0;
    for (const [id, lvl] of levels) {
      const arr = byLevel.get(lvl) || [];
      arr.push(id);
      byLevel.set(lvl, arr);
      maxLevel = Math.max(maxLevel, lvl);
    }
    // Handle orphaned nodes (cycles or disconnected)
    const placed = new Set(levels.keys());
    for (const n of nodes) {
      if (!placed.has(n.id)) {
        const lvl = maxLevel + 1;
        const arr = byLevel.get(lvl) || [];
        arr.push(n.id);
        byLevel.set(lvl, arr);
      }
    }
    const colWidth = 240;
    const rowHeight = 120;
    const newNodes = [...nodes];
    for (const [lvl, ids] of byLevel) {
      ids.forEach((id, idx) => {
        const i = newNodes.findIndex((n) => n.id === id);
        if (i >= 0) {
          newNodes[i] = {
            ...newNodes[i],
            x: 40 + lvl * colWidth,
            y: 40 + idx * rowHeight,
          };
        }
      });
    }
    setNodes(newNodes);
  };

  const selectedNode = nodes.find((n) => n.id === selectedId) || null;
  const selectedSkill = skills.find((s) => s.type === selectedNode?.type);

  // Build skill categories
  const skillCategories = new Map<string, ExecutorInfo[]>();
  for (const s of skills) {
    const cat = s.category || 'other';
    const arr = skillCategories.get(cat) || [];
    arr.push(s);
    skillCategories.set(cat, arr);
  }

  return (
    <div className="builder-page">
      {/* Toolbar */}
      <div className="builder-toolbar">
        <div className="toolbar-left">
          <div className="brand-mini">
            <span className="brand-mark">◈</span>
            <span className="brand-text">OpenSeal</span>
          </div>
          <input
            className="workflow-name-input"
            value={workflowName}
            onChange={(e) => setWorkflowName(e.target.value)}
            placeholder="Workflow name..."
          />
        </div>
        <div className="toolbar-actions">
          <button className="btn-toolbar" onClick={handleAutoLayout} disabled={nodes.length === 0}>
            ◫ Auto Layout
          </button>
          <button className="btn-toolbar" onClick={handleValidate} disabled={nodes.length === 0}>
            ◊ Validate
          </button>
          <button className="btn-toolbar btn-primary" onClick={handleSave} disabled={saving}>
            {saving ? '...' : '↳ Save'}
          </button>
        </div>
      </div>

      {/* Validation toast */}
      {validation && (
        <div className={`validation-toast ${validation.includes('failed') || validation.includes('Invalid') ? 'error' : 'success'}`}>
          {validation}
          <button className="toast-close" onClick={() => setValidation(null)}>×</button>
        </div>
      )}

      <div className="builder-body">
        {/* Skills Palette */}
        <div className="builder-palette">
          <div className="palette-header">▦ Skills</div>
          <div className="palette-body">
            {Array.from(skillCategories.entries()).map(([cat, list]) => (
              <div key={cat} className="palette-group">
                <div className="palette-group-label">{cat}</div>
                {list.map((s) => (
                  <div
                    key={s.type}
                    className="palette-item"
                    draggable
                    onDragStart={(e) => e.dataTransfer.setData('skillType', s.type)}
                    title={s.description}
                  >
                    <span className="palette-item-icon">{s.icon || '◈'}</span>
                    <span className="palette-item-name">{s.type}</span>
                  </div>
                ))}
              </div>
            ))}
          </div>
        </div>

        {/* Canvas */}
        <div
          className="builder-canvas"
          ref={canvasRef}
          onDrop={handleDrop}
          onDragOver={(e) => e.preventDefault()}
          onMouseDown={handleCanvasMouseDown}
          onMouseMove={handleMouseMove}
          onMouseUp={handleMouseUp}
        >
          <div className="canvas-grid" />

          {/* Connections SVG */}
          <svg className="canvas-svg">
            <defs>
              <marker id="edge-arrow" markerWidth="8" markerHeight="6" refX="7" refY="3" orient="auto">
                <polygon points="0 0, 8 3, 0 6" fill="#3a3a3a" />
              </marker>
            </defs>
            {edges.map((edge) => {
              const from = nodes.find((n) => n.id === edge.from);
              const to = nodes.find((n) => n.id === edge.to);
              if (!from || !to) return null;
              const fx = from.x + NODE_W / 2;
              const fy = from.y + NODE_H;
              const tx = to.x + NODE_W / 2;
              const ty = to.y;
              const mx = (fx + tx) / 2;
              return (
                <g key={edge.id}>
                  <path
                    d={`M ${fx} ${fy} C ${fx} ${fy + 40}, ${tx} ${ty - 40}, ${tx} ${ty}`}
                    fill="none"
                    stroke="#3a3a3a"
                    strokeWidth={2}
                    markerEnd="url(#edge-arrow)"
                  />
                  <rect
                    x={mx - 6}
                    y={(fy + ty) / 2 - 6}
                    width={12}
                    height={12}
                    fill="#1c1c1c"
                    stroke="#3a3a3a"
                    rx={2}
                    className="edge-delete"
                    onClick={() => setEdges((prev) => prev.filter((e) => e.id !== edge.id))}
                    style={{ cursor: 'pointer' }}
                  />
                  <text
                    x={mx}
                    y={(fy + ty) / 2 + 3}
                    textAnchor="middle"
                    fill="#8a8a8a"
                    fontSize={8}
                    pointerEvents="none"
                  >
                    ×
                  </text>
                </g>
              );
            })}
            {/* Temp connection line */}
            {drag.kind === 'connect' && (() => {
              const from = nodes.find((n) => n.id === drag.fromId);
              if (!from) return null;
              const fx = from.x + NODE_W / 2;
              const fy = from.y + NODE_H;
              return (
                <path
                  d={`M ${fx} ${fy} C ${fx} ${fy + 40}, ${mouse.x} ${mouse.y - 40}, ${mouse.x} ${mouse.y}`}
                  fill="none"
                  stroke="var(--accent)"
                  strokeWidth={2}
                  strokeDasharray="6 4"
                />
              );
            })()}
          </svg>

          {/* Nodes */}
          {nodes.map((node) => (
            <div
              key={node.id}
              className={`canvas-node ${selectedId === node.id ? 'selected' : ''}`}
              style={{ left: node.x, top: node.y, width: NODE_W, height: NODE_H }}
              onMouseDown={(e) => handleNodeMouseDown(e, node)}
            >
              {/* Input port */}
              <div className="port port-input" style={{ left: NODE_W / 2 - PORT_R, top: -PORT_R }} />
              {/* Output port */}
              <div
                className="port port-output"
                style={{ left: NODE_W / 2 - PORT_R, top: NODE_H - PORT_R }}
              />
              <div className="node-content">
                <div className="node-id">{node.id}</div>
                <div className="node-type">{node.type}</div>
              </div>
              <button
                className="node-delete-btn"
                onClick={(e) => {
                  e.stopPropagation();
                  deleteNode(node.id);
                }}
              >
                ×
              </button>
            </div>
          ))}
        </div>

        {/* Properties Panel */}
        <div className="builder-props">
          <div className="props-header">◫ Properties</div>
          <div className="props-body">
            {!selectedNode && (
              <div className="props-empty">
                Select a node to edit its properties.
                <br />
                <br />
                <strong>Shortcuts:</strong>
                <ul>
                  <li>Drag skill from left to canvas</li>
                  <li>Drag node to move</li>
                  <li>Drag output port → input port to connect</li>
                  <li>Click node to select</li>
                  <li>Delete key to remove selected</li>
                </ul>
              </div>
            )}
            {selectedNode && (
              <div className="props-form">
                <div className="prop-row">
                  <label>ID</label>
                  <input
                    value={selectedNode.id}
                    onChange={(e) => updateNodeId(selectedNode.id, e.target.value)}
                    className="prop-input"
                  />
                </div>
                <div className="prop-row">
                  <label>Type</label>
                  <div className="prop-readonly">{selectedNode.type}</div>
                </div>
                {selectedSkill?.inputSchema && (
                  <>
                    <div className="prop-divider" />
                    <div className="prop-section">Configuration</div>
                    {Object.entries((selectedSkill.inputSchema as any).properties || {}).map(
                      ([key, schema]: [string, any]) => {
                        const val = selectedNode.config[key];
                        return (
                          <div className="prop-row" key={key}>
                            <label title={schema.description}>
                              {key}
                              {(selectedSkill.inputSchema as any).required?.includes(key) && (
                                <span className="prop-required">*</span>
                              )}
                            </label>
                            {schema.type === 'boolean' ? (
                              <select
                                className="prop-input"
                                value={val === true ? 'true' : val === false ? 'false' : ''}
                                onChange={(e) => {
                                  const v = e.target.value;
                                  updateNodeConfig(selectedNode.id, {
                                    ...selectedNode.config,
                                    [key]: v === 'true' ? true : v === 'false' ? false : undefined,
                                  });
                                }}
                              >
                                <option value="">—</option>
                                <option value="true">true</option>
                                <option value="false">false</option>
                              </select>
                            ) : (
                              <input
                                className="prop-input"
                                value={val !== undefined && val !== null ? String(val) : ''}
                                placeholder={schema.description || ''}
                                onChange={(e) => {
                                  updateNodeConfig(selectedNode.id, {
                                    ...selectedNode.config,
                                    [key]: e.target.value,
                                  });
                                }}
                              />
                            )}
                          </div>
                        );
                      }
                    )}
                  </>
                )}
              </div>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}
