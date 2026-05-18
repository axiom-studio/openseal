import { useEffect, useRef, useState, useCallback, useMemo } from 'react';
import { api, type WorkflowEntry, type WorkflowEdge, type ExecutorInfo } from '../api/client';
import WorkflowGraph from '../components/WorkflowGraph';

type BuilderStep =
  | 'start'
  | 'name'
  | 'trigger_type'
  | 'trigger_config'
  | 'add_action'
  | 'action_config'
  | 'connect'
  | 'review';

interface ChatMessage {
  role: 'system' | 'user';
  text: string;
  quickActions?: string[];
}

interface DraftNode {
  id: string;
  type: string;
  config: Record<string, unknown>;
}

const TRIGGER_TYPES = ['webhook', 'cron', 'manual', 'k8s-event', 'k8s-watch'];

const TRIGGER_PROMPTS: Record<string, Record<string, string>> = {
  webhook: { path: 'Webhook path (e.g. /webhook)', method: 'HTTP method (GET, POST, etc.)' },
  cron: { expression: 'Cron expression (with seconds field, e.g. "0 */5 * * * *")' },
  manual: {},
  'k8s-event': { namespace: 'Namespace to watch', resource: 'Resource type' },
  'k8s-watch': { namespace: 'Namespace to watch', resource: 'Resource type' },
};

export default function Builder() {
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [input, setInput] = useState('');
  const [step, setStep] = useState<BuilderStep>('start');
  const [draftName, setDraftName] = useState('');
  const [draftNodes, setDraftNodes] = useState<DraftNode[]>([]);
  const [draftEdges, setDraftEdges] = useState<WorkflowEdge[]>([]);
  const [skills, setSkills] = useState<ExecutorInfo[]>([]);
  const [configuringNodeIdx, setConfiguringNodeIdx] = useState<number | null>(null);
  const [configuringKeys, setConfiguringKeys] = useState<string[]>([]);
  const [configuringKeyIdx, setConfiguringKeyIdx] = useState(0);
  const [pendingConfig, setPendingConfig] = useState<Record<string, unknown>>({});
  const [hclPreview, setHclPreview] = useState('');
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState('');
  const [sidebarOpen, setSidebarOpen] = useState(true);
  const messagesEndRef = useRef<HTMLDivElement>(null);

  const scrollToBottom = () => messagesEndRef.current?.scrollIntoView({ behavior: 'smooth' });
  useEffect(scrollToBottom, [messages]);

  useEffect(() => {
    api.listSkills().then(setSkills).catch(() => {});
  }, []);

  const sendSystem = useCallback((text: string, quickActions?: string[]) => {
    setMessages((prev) => [...prev, { role: 'system', text, quickActions }]);
  }, []);

  const sendUser = useCallback((text: string) => {
    setMessages((prev) => [...prev, { role: 'user', text }]);
  }, []);

  const draftWorkflow = useCallback((): WorkflowEntry => ({
    name: draftName,
    source: `${draftName}.hcl`,
    nodes: draftNodes.map((n) => ({ id: n.id, type: n.type, config: n.config })),
    edges: draftEdges,
  }), [draftName, draftNodes, draftEdges]);

  const updateHCLPreview = useCallback(async () => {
    if (draftNodes.length === 0) {
      setHclPreview('');
      return;
    }
    const wf = draftWorkflow();
    try {
      const res = await api.validateWorkflow(wf);
      if (res.valid) {
        // Build a pseudo-HCL for preview since we don't have a client-side renderer
        let hcl = `workflow "${wf.name}" {\n`;
        for (const n of wf.nodes) {
          hcl += `  node ${n.type} "${n.id}" {\n`;
          for (const [k, v] of Object.entries(n.config || {})) {
            const val = typeof v === 'string' ? `"${v}"` : String(v);
            hcl += `    ${k} = ${val}\n`;
          }
          hcl += `  }\n`;
        }
        for (const e of wf.edges) {
          hcl += `  edge "${e.from}" "${e.to}"`;
          if (e.condition) hcl += ` { condition = "${e.condition}" }`;
          hcl += '\n';
        }
        hcl += '}\n';
        setHclPreview(hcl);
      } else {
        setHclPreview(`// Validation issues:\n${res.issues.map((i) => `// [${i.level}] ${i.message}`).join('\n')}`);
      }
    } catch {
      setHclPreview('// Unable to generate preview');
    }
  }, [draftWorkflow, draftNodes.length]);

  useEffect(() => {
    updateHCLPreview();
  }, [updateHCLPreview]);

  const startBuilder = () => {
    setStep('name');
    sendSystem('What would you like to name your workflow?', []);
  };

  const handleName = (text: string) => {
    const name = text.trim();
    if (!name) {
      sendSystem('Please provide a valid workflow name.', []);
      return;
    }
    setDraftName(name);
    setStep('trigger_type');
    sendSystem(
      `Great. Let's add a trigger node. Pick a trigger type:`,
      TRIGGER_TYPES,
    );
  };

  const handleTriggerType = (text: string) => {
    const type_ = text.trim().toLowerCase();
    if (!TRIGGER_TYPES.includes(type_)) {
      sendSystem(`Unknown trigger type "${type_}". Pick from: ${TRIGGER_TYPES.join(', ')}`, TRIGGER_TYPES);
      return;
    }
    const nodeId = type_ === 'manual' ? 'trigger' : `${type_.replace(/-/g, '_')}_trigger`;
    const newNode: DraftNode = { id: nodeId, type: type_, config: {} };
    setDraftNodes([newNode]);
    const prompts = TRIGGER_PROMPTS[type_] || {};
    const keys = Object.keys(prompts);
    if (keys.length === 0) {
      setStep('add_action');
      sendSystem(
        `Trigger node "${nodeId}" (${type_}) added. What action nodes should run next?`,
        skills.filter((s) => !TRIGGER_TYPES.includes(s.type)).map((s) => s.type),
      );
    } else {
      setStep('trigger_config');
      setConfiguringNodeIdx(0);
      setConfiguringKeys(keys);
      setConfiguringKeyIdx(0);
      setPendingConfig({});
      sendSystem(`${keys[0]}: ${prompts[keys[0]]}`, []);
    }
  };

  const handleConfig = (text: string) => {
    const key = configuringKeys[configuringKeyIdx];
    const nextIdx = configuringKeyIdx + 1;
    const updatedConfig = { ...pendingConfig, [key]: text.trim() };
    setPendingConfig(updatedConfig);

    if (nextIdx < configuringKeys.length) {
      setConfiguringKeyIdx(nextIdx);
      const prompts = TRIGGER_PROMPTS[draftNodes[configuringNodeIdx!].type] || {};
      const nextKey = configuringKeys[nextIdx];
      sendSystem(`${nextKey}: ${prompts[nextKey]}`, []);
    } else {
      // Done configuring this node
      setDraftNodes((prev) => {
        const copy = [...prev];
        copy[configuringNodeIdx!] = { ...copy[configuringNodeIdx!], config: updatedConfig };
        return copy;
      });
      setConfiguringNodeIdx(null);
      setConfiguringKeys([]);
      setConfiguringKeyIdx(0);
      setPendingConfig({});
      setStep('add_action');
      sendSystem(
        `Node configured. What action nodes should run next?`,
        skills.filter((s) => !TRIGGER_TYPES.includes(s.type)).map((s) => s.type),
      );
    }
  };

  const handleAddAction = (text: string) => {
    const type_ = text.trim().toLowerCase();
    if (type_ === 'done' || type_ === 'no more' || type_ === 'finish') {
      if (draftNodes.length <= 1) {
        sendSystem('Add at least one action node before finishing.', []);
        return;
      }
      setStep('connect');
      sendSystem(
        `How should nodes connect? You can type pairs like "a -> b", or click "Use linear" to connect them in order.`,
        ['Use linear'],
      );
      return;
    }
    const skill = skills.find((s) => s.type === type_);
    if (!skill) {
      sendSystem(
        `Unknown node type "${type_}". Pick from the list or type "done" to finish.`,
        [...skills.filter((s) => !TRIGGER_TYPES.includes(s.type)).map((s) => s.type), 'done'],
      );
      return;
    }
    const id = `${type_.replace(/-/g, '_')}_${draftNodes.filter((n) => n.type === type_).length + 1}`;
    const newNode: DraftNode = { id, type: type_, config: {} };
    setDraftNodes((prev) => [...prev, newNode]);
    setConfiguringNodeIdx(draftNodes.length); // index of the new node

    // Build config keys from inputSchema if available
    const schema = skill.inputSchema || {};
    const props = (schema.properties || {}) as Record<string, { type?: string; description?: string }>;
    const required = (schema.required || []) as string[];
    const keys = Object.keys(props).filter((k) => required.includes(k));
    if (keys.length > 0) {
      setStep('action_config');
      setConfiguringKeys(keys);
      setConfiguringKeyIdx(0);
      setPendingConfig({});
      sendSystem(`${keys[0]} (${props[keys[0]]?.type || 'string'}): ${props[keys[0]]?.description || 'Required config'}`, []);
    } else {
      sendSystem(
        `Added "${id}" (${type_}). Add another node or type "done" to finish.`,
        [...skills.filter((s) => !TRIGGER_TYPES.includes(s.type)).map((s) => s.type), 'done'],
      );
    }
  };

  const handleActionConfig = (text: string) => {
    const key = configuringKeys[configuringKeyIdx];
    const nextIdx = configuringKeyIdx + 1;
    const updatedConfig = { ...pendingConfig, [key]: text.trim() };
    setPendingConfig(updatedConfig);

    if (nextIdx < configuringKeys.length) {
      setConfiguringKeyIdx(nextIdx);
      const skill = skills.find((s) => s.type === draftNodes[configuringNodeIdx!].type);
      const schema = skill?.inputSchema || {};
      const props = (schema.properties || {}) as Record<string, { type?: string; description?: string }>;
      const nextKey = configuringKeys[nextIdx];
      sendSystem(`${nextKey} (${props[nextKey]?.type || 'string'}): ${props[nextKey]?.description || 'Required config'}`, []);
    } else {
      setDraftNodes((prev) => {
        const copy = [...prev];
        copy[configuringNodeIdx!] = { ...copy[configuringNodeIdx!], config: updatedConfig };
        return copy;
      });
      setConfiguringNodeIdx(null);
      setConfiguringKeys([]);
      setConfiguringKeyIdx(0);
      setPendingConfig({});
      setStep('add_action');
      sendSystem(
        `Node configured. Add another node or type "done" to finish.`,
        [...skills.filter((s) => !TRIGGER_TYPES.includes(s.type)).map((s) => s.type), 'done'],
      );
    }
  };

  const handleConnect = (text: string) => {
    if (text.trim().toLowerCase() === 'use linear') {
      const edges: WorkflowEdge[] = [];
      for (let i = 0; i < draftNodes.length - 1; i++) {
        edges.push({ from: draftNodes[i].id, to: draftNodes[i + 1].id });
      }
      setDraftEdges(edges);
      setStep('review');
      sendSystem(
        `Workflow "${draftName}" is ready. Review the graph and HCL on the right, then click Save.`,
        ['Save workflow', 'Add more nodes', 'Reset edges'],
      );
      return;
    }
    // Parse "a -> b" format
    const match = text.trim().match(/(.+?)\s*->\s*(.+)/);
    if (!match) {
      sendSystem('Type connections as "from -> to" or click "Use linear".', ['Use linear']);
      return;
    }
    const from = match[1].trim();
    const to = match[2].trim();
    const fromExists = draftNodes.some((n) => n.id === from);
    const toExists = draftNodes.some((n) => n.id === to);
    if (!fromExists || !toExists) {
      sendSystem(`Unknown node(s). Available: ${draftNodes.map((n) => n.id).join(', ')}`, ['Use linear']);
      return;
    }
    setDraftEdges((prev) => [...prev, { from, to }]);
    sendSystem(
      `Added edge ${from} -> ${to}. Type another connection or click "Done connecting".`,
      ['Done connecting', 'Use linear'],
    );
  };

  const handleReviewAction = (text: string) => {
    const t = text.trim().toLowerCase();
    if (t === 'save workflow') {
      saveWorkflow();
    } else if (t === 'add more nodes') {
      setStep('add_action');
      sendSystem(
        'What action node should we add?',
        skills.filter((s) => !TRIGGER_TYPES.includes(s.type)).map((s) => s.type),
      );
    } else if (t === 'reset edges') {
      setDraftEdges([]);
      setStep('connect');
      sendSystem(
        'Edges cleared. How should nodes connect?',
        ['Use linear'],
      );
    } else if (t === 'done connecting') {
      setStep('review');
      sendSystem(
        `Workflow "${draftName}" is ready. Review and click Save when satisfied.`,
        ['Save workflow', 'Add more nodes', 'Reset edges'],
      );
    } else {
      sendSystem('Click an action above or type your choice.', ['Save workflow', 'Add more nodes', 'Reset edges']);
    }
  };

  const saveWorkflow = async () => {
    setSaving(true);
    setSaveError('');
    try {
      const wf = draftWorkflow();
      await api.createWorkflow(wf);
      sendSystem(`Workflow "${draftName}" saved successfully! It will appear on the Workflows page.`, ['Build another']);
      setStep('start');
      setDraftName('');
      setDraftNodes([]);
      setDraftEdges([]);
      setHclPreview('');
    } catch (e: any) {
      setSaveError(e.message || 'Failed to save');
      sendSystem(`Error saving: ${e.message || 'unknown error'}`, ['Retry save', 'Edit more']);
    } finally {
      setSaving(false);
    }
  };

  const handleSend = () => {
    if (!input.trim()) return;
    const text = input.trim();
    setInput('');
    sendUser(text);

    switch (step) {
      case 'start':
        startBuilder();
        break;
      case 'name':
        handleName(text);
        break;
      case 'trigger_type':
        handleTriggerType(text);
        break;
      case 'trigger_config':
        handleConfig(text);
        break;
      case 'action_config':
        handleActionConfig(text);
        break;
      case 'add_action':
        handleAddAction(text);
        break;
      case 'connect':
        handleConnect(text);
        break;
      case 'review':
        handleReviewAction(text);
        break;
    }
  };

  const handleQuickAction = (action: string) => {
    setInput(action);
    setTimeout(() => {
      setInput('');
      sendUser(action);
      switch (step) {
        case 'start':
          startBuilder();
          break;
        case 'name':
          handleName(action);
          break;
        case 'trigger_type':
          handleTriggerType(action);
          break;
        case 'trigger_config':
          handleConfig(action);
          break;
        case 'action_config':
          handleActionConfig(action);
          break;
        case 'add_action':
          handleAddAction(action);
          break;
        case 'connect':
          handleConnect(action);
          break;
        case 'review':
          handleReviewAction(action);
          break;
      }
    }, 0);
  };

  const handleSkillClick = (skillType: string) => {
    if (step === 'start') {
      startBuilder();
      return;
    }
    if (step === 'name') {
      sendSystem('Please name your workflow first.', []);
      return;
    }
    if (step === 'trigger_type') {
      handleTriggerType(skillType);
      return;
    }
    if (step === 'trigger_config' || step === 'action_config') {
      sendSystem('Finish configuring the current node first.', []);
      return;
    }
    if (step === 'connect' || step === 'review') {
      sendSystem('You can add more nodes by clicking "Add more nodes" first.', []);
      return;
    }
    // add_action
    handleAddAction(skillType);
  };

  const skillCategories = useMemo(() => {
    const map = new Map<string, ExecutorInfo[]>();
    for (const s of skills) {
      const cat = s.category || 'other';
      const arr = map.get(cat) || [];
      arr.push(s);
      map.set(cat, arr);
    }
    return map;
  }, [skills]);

  const wfEntry = draftWorkflow();

  return (
    <div className="page builder-page">
      {/* Skills Sidebar */}
      <div className={`builder-sidebar ${sidebarOpen ? 'open' : 'collapsed'}`}>
        <div className="sidebar-header">
          <span className="sidebar-title">▦ Skills</span>
          <button className="sidebar-toggle" onClick={() => setSidebarOpen(!sidebarOpen)}>
            {sidebarOpen ? '◀' : '▶'}
          </button>
        </div>
        {sidebarOpen && (
          <div className="sidebar-body">
            {Array.from(skillCategories.entries()).map(([cat, list]) => (
              <div key={cat} className="skill-group">
                <div className="skill-group-label">{cat}</div>
                {list.map((s) => (
                  <button
                    key={s.type}
                    className="skill-item"
                    onClick={() => handleSkillClick(s.type)}
                    title={s.description}
                  >
                    <span className="skill-item-icon">{s.icon || '▸'}</span>
                    <span className="skill-item-name">{s.type}</span>
                  </button>
                ))}
              </div>
            ))}
          </div>
        )}
      </div>

      <div className="builder-chat">
        <header className="builder-header">
          <h1>Workflow Builder</h1>
          <p className="page-subtitle">Chat your way to a workflow</p>
        </header>
        <div className="chat-messages">
          {messages.length === 0 && (
            <div className="chat-empty">
              <div className="chat-welcome">
                <div className="welcome-icon">◈</div>
                <h2>Build a workflow</h2>
                <p>Describe what you want, and I'll assemble the nodes and connections.</p>
                <button className="btn-primary btn-large" onClick={startBuilder}>
                  Start Building
                </button>
              </div>
            </div>
          )}
          {messages.map((msg, i) => (
            <div key={i} className={`chat-bubble ${msg.role}`}>
              <div className="chat-avatar">{msg.role === 'system' ? '◈' : '◉'}</div>
              <div className="chat-content">
                <div className="chat-text">{msg.text}</div>
                {msg.quickActions && msg.quickActions.length > 0 && (
                  <div className="quick-actions">
                    {msg.quickActions.map((a) => (
                      <button key={a} className="quick-action" onClick={() => handleQuickAction(a)}>
                        {a}
                      </button>
                    ))}
                  </div>
                )}
              </div>
            </div>
          ))}
          <div ref={messagesEndRef} />
        </div>
        <div className="chat-input-bar">
          <input
            className="chat-input"
            placeholder={step === 'start' ? 'Click Start Building...' : 'Type your response...'}
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && handleSend()}
            disabled={step === 'start' || saving}
          />
          <button className="btn-primary" onClick={handleSend} disabled={!input.trim() || saving}>
            {saving ? '...' : '↳'}
          </button>
        </div>
        {saveError && <div className="chat-error">{saveError}</div>}
      </div>

      <div className="builder-preview">
        <div className="preview-panel graph-panel">
          <div className="panel-header">
            <span className="panel-title">◫ Graph</span>
            <span className="panel-meta">{draftNodes.length} node{draftNodes.length !== 1 ? 's' : ''}</span>
          </div>
          <div className="panel-body">
            {draftNodes.length > 0 ? (
              <WorkflowGraph nodes={wfEntry.nodes} edges={wfEntry.edges} />
            ) : (
              <div className="graph-empty">Workflow graph will appear here</div>
            )}
          </div>
        </div>
        <div className="preview-panel hcl-panel">
          <div className="panel-header">
            <span className="panel-title">⌥ HCL</span>
            <span className="panel-meta">{hclPreview ? `${hclPreview.split('\n').length} lines` : '—'}</span>
          </div>
          <div className="panel-body">
            {hclPreview ? (
              <pre className="hcl-preview">{hclPreview}</pre>
            ) : (
              <div className="graph-empty">Generated HCL will appear here</div>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}
