import brandMark from "./brand-mark.png";
import {
  readHomeSubmission,
  newHomeSubmission,
  persistHomeSubmission,
  finishHomeSubmission,
  type HomeSubmission,
} from "./homeSubmission";
import TeamBrowser from "./TeamBrowser";
import TaskArtifacts from "./TaskArtifacts";
import ReviewInbox from "./ReviewInbox";
import RunApproval from "./RunApproval";
import WorkBrowser from "./WorkBrowser";
import RequestInbox from "./RequestInbox";
import WorkHistory from "./WorkHistory";
import WorkStatus from "./WorkStatus";
import WorkControls from "./WorkControls";
import WorkResult from "./WorkResult";
import ProposalReview from "./ProposalReview";
import ProviderSettings from "./ProviderSettings";
import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type FormEvent,
  type ReactNode,
  type KeyboardEvent as ReactKeyboardEvent,
} from "react";
import {
  ArrowDown,
  ArrowRight,
  ArrowUp,
  Check,
  ChevronRight,
  CircleHelp,
  Circle,
  Copy,
  FileText,
  Home,
  Layers3,
  LoaderCircle,
  Menu,
  Play,
  Plus,
  RefreshCw,
  Search,
  Settings2,
  ShieldCheck,
  SlidersHorizontal,
  Sparkles,
  Users,
  Workflow,
  X,
  type LucideIcon,
} from "lucide-react";
import {
  api,
  status,
  scoped,
  scope,
  supports,
  message,
  type Agent,
  type Run,
  type Capabilities,
  type DesktopStatus,
  type Proposal,
} from "./api";

type Page = "Home" | "Agents" | "Work" | "Reviews" | "Teams" | "Settings";
type Selection =
  | { kind: "agent"; value: Agent }
  | { kind: "run"; value: Run; approvalId?: string; requestId?: string }
  | null;
const pages: { name: Page; icon: LucideIcon; shortcut: string }[] = [
  { name: "Home", icon: Home, shortcut: "1" },
  { name: "Agents", icon: Users, shortcut: "2" },
  { name: "Work", icon: Layers3, shortcut: "3" },
  { name: "Reviews", icon: ShieldCheck, shortcut: "4" },
  { name: "Teams", icon: Workflow, shortcut: "5" },
];
const starters = [
  {
    title: "Research a topic",
    text: "Create a research agent that investigates a topic, compares reliable sources, and delivers a concise report with citations.",
    icon: Search,
  },
  {
    title: "Keep an eye on changes",
    text: "Create an agent that monitors changes relevant to my work, explains what matters, and asks for approval before taking external actions.",
    icon: Workflow,
  },
  {
    title: "Make a plan",
    text: "Create a planning agent that turns a goal into clear milestones, identifies dependencies, and helps me decide what to do next.",
    icon: FileText,
  },
];
function readLocal(key: string) {
  try {
    return localStorage.getItem(key) || "";
  } catch {
    return "";
  }
}
function saveLocal(key: string, value: string) {
  try {
    value ? localStorage.setItem(key, value) : localStorage.removeItem(key);
  } catch {
    /* Private mode may disable storage. */
  }
}
type ComposerDraft = {
  version: 1;
  prompt: string;
  mode: "agent" | "team" | "work";
  agentID: string;
};
function readDraft(): ComposerDraft {
  try {
    const draft = JSON.parse(readLocal("openseal.draft"));
    if (
      draft?.version === 1 &&
      typeof draft.prompt === "string" &&
      (draft.mode === "agent" ||
        draft.mode === "team" ||
        draft.mode === "work") &&
      typeof draft.agentID === "string"
    )
      return draft;
  } catch {
    /* Earlier versions saved only the prompt. */
  }
  return {
    version: 1,
    prompt: readLocal("openseal.prompt"),
    mode: "agent",
    agentID: "",
  };
}
const label = (value: string) =>
  value.replaceAll("_", " ").replace(/^./, (c) => c.toUpperCase());
function date(value: string) {
  const d = new Date(value);
  return Number.isNaN(d.getTime())
    ? "—"
    : new Intl.DateTimeFormat(undefined, {
        month: "short",
        day: "numeric",
        hour: "numeric",
        minute: "2-digit",
      }).format(d);
}
function StatusPill({ value }: { value: string }) {
  return (
    <span className={`status-pill status-${value}`}>
      <span aria-hidden="true" />
      {label(value)}
    </span>
  );
}
function Empty({
  icon: Icon,
  title,
  children,
  action,
}: {
  icon: LucideIcon;
  title: string;
  children: ReactNode;
  action?: ReactNode;
}) {
  return (
    <div className="empty">
      <Icon size={27} strokeWidth={1.5} aria-hidden="true" />
      <h3>{title}</h3>
      <p>{children}</p>
      {action}
    </div>
  );
}
function IconButton({
  icon: Icon,
  title,
  onClick,
  disabled = false,
}: {
  icon: LucideIcon;
  title: string;
  onClick: () => void;
  disabled?: boolean;
}) {
  return (
    <button
      className="icon-button"
      aria-label={title}
      title={title}
      onClick={onClick}
      disabled={disabled}
    >
      <Icon size={18} />
    </button>
  );
}

function trapNavigationFocus(e: ReactKeyboardEvent<HTMLElement>) {
  if (e.key !== "Tab" || !window.matchMedia("(max-width:700px)").matches)
    return;
  const controls = [
    ...e.currentTarget.querySelectorAll<HTMLElement>(
      "button:not(:disabled), input, select, a[href]",
    ),
  ];
  const first = controls[0],
    last = controls[controls.length - 1];
  if (e.shiftKey && document.activeElement === first) {
    e.preventDefault();
    last?.focus();
  } else if (!e.shiftKey && document.activeElement === last) {
    e.preventDefault();
    first?.focus();
  }
}

export default function App() {
  const [page, setPage] = useState<Page>("Home");
  const [desktop, setDesktop] = useState<DesktopStatus>({
    state: "starting",
    message: "",
    workspace: "",
  });
  const [caps, setCaps] = useState<Capabilities | null>(null);
  const [agents, setAgents] = useState<Agent[]>([]);
  const [runs, setRuns] = useState<Run[]>([]);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState("");
  const [connectionError, setConnectionError] = useState("");
  const [notice, setNotice] = useState("");
  const [selection, setSelection] = useState<Selection>(null);
  const selectedRun = useRef<Run | null>(null);
  selectedRun.current = selection?.kind === "run" ? selection.value : null;

  const [query, setQuery] = useState("");
  const [workView, setWorkView] = useState("tasks");
  const [workRefresh, setWorkRefresh] = useState(0);
  const [hasPendingReviews, setHasPendingReviews] = useState(false);
  const [savedSubmission] = useState(readHomeSubmission);
  const submission = useRef<HomeSubmission | null>(savedSubmission.pending);
  const [pendingSubmission, setPendingSubmission] = useState(
    savedSubmission.pending,
  );
  const [submissionError, setSubmissionError] = useState(savedSubmission.error);
  const [recoveryError, setRecoveryError] = useState(savedSubmission.error);
  const [forgetRequest, setForgetRequest] = useState(false);
  const forgetTrigger = useRef<HTMLButtonElement>(null);
  const forgetConfirm = useRef<HTMLButtonElement>(null);
  const submissionLock = useRef(false);
  const submissionFeedback = useRef<HTMLParagraphElement>(null);
  const activePage = useRef(page);
  activePage.current = page;
  const [savedDraft] = useState(
    () => savedSubmission.pending?.draft || readDraft(),
  );
  const [prompt, updatePrompt] = useState(savedDraft.prompt);
  const [composerMode, updateComposerMode] = useState<
    "agent" | "team" | "work"
  >(savedDraft.mode);
  const [agentID, updateAgentID] = useState(savedDraft.agentID);
  const setPrompt = useCallback((value: string) => {
    if (!submission.current) updatePrompt(value);
  }, []);
  const setComposerMode = useCallback((value: "agent" | "team" | "work") => {
    if (!submission.current) updateComposerMode(value);
  }, []);
  const setAgentID = useCallback((value: string) => {
    if (!submission.current) updateAgentID(value);
  }, []);
  const [busy, setBusy] = useState(false);
  const [proposal, setProposal] = useState<Proposal | null>(null);
  const activeProposal = useRef(proposal);
  activeProposal.current = proposal;
  const [proposalID, setProposalID] = useState(() =>
    readLocal("openseal.proposal"),
  );
  const [commandOpen, setCommandOpen] = useState(false);
  const [commandQuery, setCommandQuery] = useState("");
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const [theme, setTheme] = useState(
    () => readLocal("openseal.theme") || "system",
  );
  const [helpOpen, setHelpOpen] = useState(false);
  const composer = useRef<HTMLTextAreaElement>(null);
  const palette = useRef<HTMLDialogElement>(null);
  const help = useRef<HTMLDialogElement>(null);
  const mainHeading = useRef<HTMLHeadingElement>(null);
  const sidebar = useRef<HTMLElement>(null);
  const menuTrigger = useRef<HTMLSpanElement>(null);
  const closeNavigation = useCallback(() => {
    setSidebarOpen(false);
    requestAnimationFrame(() =>
      menuTrigger.current?.querySelector("button")?.focus(),
    );
  }, []);
  useEffect(() => {
    if (sidebarOpen)
      requestAnimationFrame(() =>
        sidebar.current?.querySelector("button")?.focus(),
      );
  }, [sidebarOpen]);
  const inspector = useRef<HTMLElement>(null);
  const selectionTrigger = useRef<HTMLElement | null>(null);
  const selectRecord = (value: NonNullable<Selection>) => {
    selectionTrigger.current = document.activeElement as HTMLElement;
    setSelection(value);
  };
  const closeInspector = useCallback(() => {
    setSelection(null);
    requestAnimationFrame(() => {
      const trigger = selectionTrigger.current;
      (trigger?.isConnected ? trigger : mainHeading.current)?.focus();
    });
  }, []);
  const selectionKey =
    selection?.kind === "run"
      ? `${selection.value.id}:${selection.approvalId || "current"}:${selection.requestId || "all"}`
      : selection?.value.deployment.id;
  useEffect(() => {
    if (selectionKey) requestAnimationFrame(() => inspector.current?.focus());
  }, [selectionKey]);
  const inFlight = useRef(false);

  const canPropose = supports(caps, "workforce-authoring", "propose");
  const canRun = supports(caps, "agent-runs", "create");
  const activeAgents = agents.filter(
    (a) => a.deployment.rolloutStatus === "active",
  );
  const assignedAgentAvailable = activeAgents.some(
    (a) => a.deployment.id === agentID,
  );
  const navigate = useCallback((next: Page) => {
    setPage(next);
    setQuery("");

    setSelection(null);
    setSidebarOpen(false);
    requestAnimationFrame(() => mainHeading.current?.focus());
  }, []);
  const newAgent = useCallback(() => {
    navigate("Home");
    setComposerMode("agent");
    requestAnimationFrame(() => composer.current?.focus());
  }, [navigate]);

  const refresh = useCallback(async (quiet = false) => {
    if (inFlight.current) return;
    inFlight.current = true;
    if (!quiet) setRefreshing(true);
    try {
      const connection = await status();
      setDesktop(connection);
      if (connection.state !== "ready") {
        if (connection.state === "error") setLoading(false);
        return;
      }
      const capabilities = await api<Capabilities>("/capabilities");
      if (!Array.isArray(capabilities.capabilities))
        throw new Error(
          "OpenSeal returned an unsupported capability document.",
        );
      const [agentList, runList, pendingReviews] = await Promise.all([
        supports(capabilities, "agent-definitions", "list")
          ? api<{ items: Agent[] }>(scoped("/agent-deployments"))
          : Promise.resolve({ items: [] }),
        supports(capabilities, "agent-runs", "list")
          ? api<Run[] | null>(
              scoped("/agent-runs?limit=100&order=created_desc"),
            )
          : Promise.resolve([]),
        supports(capabilities, "action-approvals", "list")
          ? api<unknown[] | null>(
              scoped("/action-approvals?status=pending&limit=1"),
            ).catch(() => null)
          : Promise.resolve([]),
      ]);
      if (Array.isArray(pendingReviews))
        setHasPendingReviews(pendingReviews.length > 0);

      setCaps(capabilities);
      setWorkRefresh((value) => value + 1);
      setAgents(agentList.items || []);
      setRuns((current) =>
        (runList || []).map((next) => {
          const existing = current.find((run) => run.id === next.id);
          return existing && existing.revision > next.revision
            ? existing
            : next;
        }),
      );
      const inspectedID = selectedRun.current?.id;
      const inspected =
        inspectedID &&
        !runList?.some((run) => run.id === inspectedID) &&
        supports(capabilities, "agent-runs", "get")
          ? await api<Run>(
              scoped(`/agent-runs/${encodeURIComponent(inspectedID)}`),
            )
          : null;
      setSelection((current) =>
        current?.kind === "run"
          ? {
              ...current,
              value:
                runList?.find(
                  (r) =>
                    r.id === current.value.id &&
                    r.revision >= current.value.revision,
                ) ||
                (inspected?.id === current.value.id &&
                inspected.revision >= current.value.revision
                  ? inspected
                  : current.value),
            }
          : current?.kind === "agent"
            ? {
                kind: "agent",
                value:
                  agentList.items?.find(
                    (a) => a.deployment.id === current.value.deployment.id,
                  ) || current.value,
              }
            : null,
      );
      setConnectionError("");
      setLoading(false);
    } catch (e) {
      setConnectionError(message(e));
      setLoading(false);
    } finally {
      inFlight.current = false;
      setRefreshing(false);
    }
  }, []);
  useEffect(() => {
    void refresh();
    const timer = setInterval(() => {
      if (!document.hidden) void refresh(true);
    }, 5000);
    return () => clearInterval(timer);
  }, [refresh]);
  useEffect(() => {
    saveLocal(
      "openseal.draft",
      JSON.stringify({ version: 1, prompt, mode: composerMode, agentID }),
    );
    saveLocal("openseal.prompt", prompt);
  }, [prompt, composerMode, agentID]);
  useEffect(() => {
    saveLocal("openseal.theme", theme);
    document.documentElement.dataset.theme = theme;
  }, [theme]);
  useEffect(() => {
    if (!notice) return;
    const timer = setTimeout(() => setNotice(""), 4500);
    return () => clearTimeout(timer);
  }, [notice]);
  useEffect(() => {
    commandOpen ? palette.current?.showModal() : palette.current?.close();
  }, [commandOpen]);
  useEffect(() => {
    helpOpen ? help.current?.showModal() : help.current?.close();
  }, [helpOpen]);
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (e.metaKey || e.ctrlKey) {
        if (e.key.toLowerCase() === "k") {
          e.preventDefault();
          setCommandQuery("");
          setCommandOpen((v) => !v);
        }
        if (e.key.toLowerCase() === "n") {
          e.preventDefault();
          newAgent();
        }
        const target = pages.find((p) => p.shortcut === e.key);
        if (target) {
          e.preventDefault();
          navigate(target.name);
        }
      }
      if (e.key === "Escape" && !palette.current?.open && !help.current?.open) {
        if (inspector.current) closeInspector();
        if (sidebar.current?.classList.contains("open")) closeNavigation();
      }
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [navigate, newAgent, closeInspector, closeNavigation]);
  useEffect(() => {
    if (
      !proposalID ||
      desktop.state !== "ready" ||
      !supports(caps, "workforce-authoring", "get")
    )
      return;
    let stopped = false;
    let pending = false;
    const load = async () => {
      if (pending) return;
      pending = true;
      try {
        const p = await api<Proposal>(
          scoped(
            `/authoring/workforce/change-sets/${encodeURIComponent(proposalID)}`,
          ),
        );
        if (!stopped)
          setProposal((current) =>
            !current || current.id !== p.id || p.revision >= current.revision
              ? p
              : current,
          );
      } catch (e) {
        if (!stopped) setError(message(e));
      } finally {
        pending = false;
      }
    };
    void load();
    const timer = setInterval(load, 3000);
    return () => {
      stopped = true;
      clearInterval(timer);
    };
  }, [proposalID, desktop.state, caps]);

  useEffect(() => {
    if (submissionError && page === "Home") submissionFeedback.current?.focus();
  }, [submissionError, page]);
  useEffect(() => {
    if (forgetRequest) forgetConfirm.current?.focus();
  }, [forgetRequest]);
  function forgetSavedRequest() {
    if (submissionLock.current) return;
    try {
      if (
        submission.current &&
        localStorage.getItem("openseal.home-submission") !==
          JSON.stringify(submission.current)
      )
        throw new Error(
          "The saved request changed. Reload before clearing recovery.",
        );
      localStorage.removeItem("openseal.home-submission");
      submission.current = null;
      setPendingSubmission(null);
      setRecoveryError("");
      setSubmissionError("");
      setForgetRequest(false);
      setNotice(
        "Local retry record removed. Any saved proposal or work is unchanged. Your text is kept for review.",
      );
      requestAnimationFrame(() => composer.current?.focus());
    } catch (e) {
      setSubmissionError(`Could not clear local recovery. ${message(e)}`);
    }
  }
  function retryRecovery() {
    const saved = readHomeSubmission();
    setRecoveryError(saved.error);
    setSubmissionError(saved.error);
    submission.current = saved.pending;
    setPendingSubmission(saved.pending);
    if (saved.pending) {
      updatePrompt(saved.pending.draft.prompt);
      updateComposerMode(saved.pending.draft.mode);
      updateAgentID(saved.pending.draft.agentID);
    }
  }
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (submissionLock.current || recoveryError || desktop.state !== "ready")
      return;
    const saved = submission.current;
    if (!saved && composerMode !== "work" && !canPropose) {
      navigate("Settings");
      return;
    }
    if (
      !saved &&
      (!prompt.trim() ||
        (composerMode === "work" && (!canRun || !assignedAgentAvailable)))
    )
      return;
    const pending =
      saved ||
      newHomeSubmission({ version: 1, prompt, mode: composerMode, agentID });
    try {
      persistHomeSubmission(pending);
    } catch (e) {
      setSubmissionError(
        `Recovery storage is unavailable. Nothing was sent. ${message(e)}`,
      );
      return;
    }
    submission.current = pending;
    setPendingSubmission(pending);
    submissionLock.current = true;
    setBusy(true);
    setSubmissionError("");
    let confirmed = false;
    try {
      const result = await api<any>(pending.path, {
        method: "POST",
        key: pending.key,
        body: pending.body,
      });
      const value = pending.draft.mode === "work" ? result?.run : result;
      if (
        !value ||
        typeof value.id !== "string" ||
        !value.id ||
        !Number.isSafeInteger(value.revision) ||
        value.revision < 1 ||
        value.scope?.kind !== scope.kind ||
        value.scope.id !== scope.id ||
        typeof value.status !== "string" ||
        (pending.draft.mode === "work"
          ? value.kind !== "agent_work" ||
            value.owner?.type !== "agent" ||
            value.owner.id !== pending.draft.agentID ||
            value.assignedAgentId !== pending.draft.agentID ||
            value.goal !== pending.draft.prompt.trim()
          : value.prompt !== pending.body.prompt)
      )
        throw new Error(
          "The returned record does not match the saved request.",
        );
      confirmed = true;
      finishHomeSubmission(
        pending,
        pending.draft.mode === "work" ? undefined : value.id,
      );
      setForgetRequest(false);
      submission.current = null;
      setPendingSubmission(null);
      updatePrompt("");
      if (pending.draft.mode !== "work") {
        setProposal(value);
        setProposalID(value.id);
        setNotice("Proposal saved. You can follow its progress in Home.");
        if (activePage.current === "Home")
          requestAnimationFrame(() =>
            document.getElementById("proposal-heading")?.focus(),
          );
      } else {
        if (activePage.current === "Home") {
          setWorkView("tasks");
          navigate("Work");
          setSelection({ kind: "run", value });
        }
        setNotice("Work saved. You can follow its progress in Work.");
      }
      await refresh(true);
    } catch (e) {
      setSubmissionError(
        confirmed
          ? `Saved in the workspace, but local recovery could not be cleared. ${message(e)} Retry the saved request to recover the same record.`
          : `Could not confirm the saved request. ${message(e)} Retry uses the same request identity, including after reopening, so it does not create a second record.`,
      );
    } finally {
      submissionLock.current = false;
      setBusy(false);
    }
  }
  const updateInspectedRun = useCallback((items: Run[]) => {
    setSelection((current) => {
      if (current?.kind !== "run") return current;
      const next = items.find((item) => item.id === current.value.id);
      return next && next.revision >= current.value.revision
        ? { ...current, value: next }
        : current;
    });
  }, []);
  const filteredAgents = agents.filter((a) =>
    `${a.definition?.displayName} ${a.definition?.purpose} ${a.deployment.displayName}`
      .toLowerCase()
      .includes(query.toLowerCase()),
  );
  const running = runs.filter(
    (r) => !["completed", "canceled", "failed"].includes(r.status),
  ).length;
  const connected = desktop.state === "ready" && !connectionError;
  const isMac = navigator.userAgent.includes("Mac");
  const modifier = isMac ? "⌘" : "Ctrl";

  const agentRows = (items: Agent[]) => (
    <div className="record-list">
      {items.map((agent) => (
        <button
          className={`record-row ${selection?.kind === "agent" && selection.value.deployment.id === agent.deployment.id ? "selected" : ""}`}
          key={agent.deployment.id}
          onClick={() => selectRecord({ kind: "agent", value: agent })}
        >
          <span className="avatar" aria-hidden="true">
            {(
              agent.deployment.displayName ||
              agent.definition?.displayName ||
              "A"
            )
              .slice(0, 1)
              .toUpperCase()}
          </span>
          <span className="record-main">
            <strong>
              {agent.deployment.displayName ||
                agent.definition?.displayName ||
                agent.deployment.id}
            </strong>
            <span>
              {agent.definition?.purpose ||
                "Open agent to inspect its configuration."}
            </span>
          </span>
          <StatusPill value={agent.deployment.rolloutStatus} />
          <ChevronRight size={16} aria-hidden="true" />
        </button>
      ))}
    </div>
  );
  const runRows = (items: Run[]) => (
    <div className="record-list">
      {items.map((run) => (
        <button
          className={`record-row ${selection?.kind === "run" && selection.value.id === run.id ? "selected" : ""}`}
          key={run.id}
          onClick={() => selectRecord({ kind: "run", value: run })}
        >
          <span className="run-icon">
            <Workflow size={20} />
          </span>
          <span className="record-main">
            <strong>{run.goal || "Untitled work"}</strong>
            <span>
              {agents.find((a) => a.deployment.id === run.assignedAgentId)
                ?.definition?.displayName ||
                run.owner?.id ||
                "Workspace"}
              <span className="separator">·</span>
              {date(run.updatedAt || run.createdAt)}
            </span>
          </span>
          <StatusPill value={run.status} />
          <ChevronRight size={16} aria-hidden="true" />
        </button>
      ))}
    </div>
  );

  return (
    <div className="app-shell">
      <a className="skip-link" href="#main-content">
        Skip to workspace
      </a>
      {sidebarOpen && (
        <button
          className="sidebar-scrim"
          aria-label="Close navigation"
          onClick={closeNavigation}
        />
      )}
      <aside
        className={`sidebar ${sidebarOpen ? "open" : ""}`}
        aria-label="Workspace navigation"
        ref={sidebar}
        onKeyDown={trapNavigationFocus}
      >
        <div className="brand">
          <span className="brand-mark">
            <img src={brandMark} alt="" width={24} height={24} />
          </span>
          <strong>OpenSeal</strong>
          <span className="edition">Desktop</span>
        </div>
        <button
          className="workspace-switch"
          onClick={() => navigate("Settings")}
        >
          <span className="workspace-avatar">L</span>
          <span>
            <strong>Local workspace</strong>
            <small>On this computer</small>
          </span>
          <SlidersHorizontal size={15} />
        </button>
        <button
          className="search-trigger"
          onClick={() => {
            setCommandQuery("");
            setCommandOpen(true);
          }}
        >
          <Search size={16} />
          <span>Go to…</span>
          <kbd>{modifier} K</kbd>
        </button>
        <nav>
          {pages.map(({ name, icon: Icon, shortcut }) => (
            <button
              key={name}
              aria-current={page === name ? "page" : undefined}
              aria-label={
                name === "Reviews" && hasPendingReviews
                  ? "Reviews, pending actions"
                  : undefined
              }
              onClick={() => navigate(name)}
            >
              <Icon size={18} />
              <span>{name}</span>
              {name === "Reviews" && hasPendingReviews ? (
                <Circle size={8} fill="currentColor" aria-hidden="true" />
              ) : name === "Work" && running > 0 ? (
                <span className="nav-count">{running}</span>
              ) : (
                <kbd>
                  {modifier} {shortcut}
                </kbd>
              )}
            </button>
          ))}
        </nav>
        <div className="sidebar-bottom">
          <div className="local-note">
            <ShieldCheck size={17} />
            <span>
              Work stays in your workspace.
              <br />
              <small>Model requests use your provider.</small>
            </span>
          </div>
          <button
            className={page === "Settings" ? "current" : ""}
            onClick={() => navigate("Settings")}
          >
            <Settings2 size={18} />
            <span>Settings</span>
          </button>
          <button onClick={() => setHelpOpen(true)}>
            <CircleHelp size={18} />
            <span>Keyboard shortcuts</span>
          </button>
          <div className="connection">
            <span
              className={`connection-dot ${connected ? "online" : desktop.state === "starting" ? "starting" : "offline"}`}
            />
            <span>
              {desktop.state === "starting"
                ? "Starting workspace…"
                : connected
                  ? "Connected locally"
                  : "Connection needs attention"}
            </span>
          </div>
        </div>
      </aside>
      <div className="workspace">
        <header className="toolbar">
          <div className="toolbar-left">
            <span className="mobile-menu" ref={menuTrigger}>
              <IconButton
                icon={Menu}
                title="Open navigation"
                onClick={() => setSidebarOpen(true)}
              />
            </span>
            <span className="breadcrumb">
              Workspace <ChevronRight size={13} /> <strong>{page}</strong>
            </span>
          </div>
          <div className="toolbar-actions">
            <IconButton
              icon={RefreshCw}
              title={refreshing ? "Refreshing workspace" : "Refresh workspace"}
              onClick={() => void refresh()}
              disabled={refreshing}
            />
            <span className="toolbar-divider" />
            <button className="button primary compact" onClick={newAgent}>
              <Plus size={15} />
              Create agent
            </button>
          </div>
        </header>
        <div className={`content-layout ${selection ? "with-inspector" : ""}`}>
          <main
            id="main-content"
            className={`main-content page-${page.toLowerCase()}`}
          >
            {(error || connectionError) && (
              <div className="alert" role="alert">
                <span>
                  <strong>Something needs attention</strong>
                  <br />
                  {error || connectionError}
                </span>
                <button
                  className="button"
                  onClick={() => {
                    if (error) setError("");
                    else void refresh();
                  }}
                >
                  {error ? "Dismiss" : "Reconnect"}
                </button>
              </div>
            )}
            {desktop.state === "error" && (
              <div className="alert" role="alert">
                <span>
                  <strong>Couldn’t open the workspace</strong>
                  <br />
                  {desktop.message}
                </span>
                <button className="button" onClick={() => void refresh()}>
                  Check connection
                </button>
              </div>
            )}
            <div className="page-heading">
              <div>
                <h1 tabIndex={-1} ref={mainHeading}>
                  {page === "Home"
                    ? "A little direction. A lot of possibility."
                    : page}
                </h1>
                <p>
                  {page === "Home"
                    ? "Put an agent to work. Stay close to what matters."
                    : page === "Agents"
                      ? "Your people for the work ahead."
                      : page === "Work"
                        ? "Follow the work, from first step to final result."
                        : page === "Reviews"
                          ? "Give waiting actions a clear decision."
                          : page === "Teams"
                            ? "See how your agents work together."
                            : "Make this workspace work for you."}
                </p>
              </div>
              {page === "Agents" && (
                <button className="button primary" onClick={newAgent}>
                  <Plus size={16} />
                  Create agent
                </button>
              )}
              {page === "Teams" && (
                <button
                  className="button primary"
                  onClick={() => {
                    navigate("Home");
                    setComposerMode("team");
                    requestAnimationFrame(() => composer.current?.focus());
                  }}
                >
                  <Plus size={16} />
                  Create team
                </button>
              )}
            </div>
            {page === "Home" && (
              <>
                <form className="composer" onSubmit={submit}>
                  <div
                    className="composer-tabs"
                    role="group"
                    aria-label="What to create"
                  >
                    <button
                      type="button"
                      disabled={busy || !!pendingSubmission}
                      aria-pressed={composerMode === "agent"}
                      onClick={() => setComposerMode("agent")}
                    >
                      <Sparkles size={15} />
                      Create an agent
                    </button>
                    <button
                      type="button"
                      disabled={busy || !!pendingSubmission}
                      aria-pressed={composerMode === "team"}
                      onClick={() => setComposerMode("team")}
                    >
                      <Users size={15} />
                      Create a team
                    </button>
                    <button
                      type="button"
                      disabled={busy || !!pendingSubmission}
                      aria-pressed={composerMode === "work"}
                      onClick={() => setComposerMode("work")}
                    >
                      <Workflow size={15} />
                      Start work
                    </button>
                  </div>
                  <label className="sr-only" htmlFor="prompt">
                    {composerMode === "agent"
                      ? "Describe your agent"
                      : composerMode === "team"
                        ? "Describe your team"
                        : "Describe the work"}
                  </label>
                  <textarea
                    id="prompt"
                    readOnly={busy || !!pendingSubmission}
                    aria-describedby={
                      pendingSubmission || submissionError
                        ? "home-submission-feedback"
                        : undefined
                    }
                    ref={composer}
                    value={prompt}
                    onChange={(e) => setPrompt(e.target.value)}
                    placeholder={
                      composerMode === "agent"
                        ? "What would you like a hand with?"
                        : composerMode === "team"
                          ? "What should your team achieve, and which roles does it need?"
                          : "What should your agent accomplish?"
                    }
                    maxLength={16000}
                    onKeyDown={(e) => {
                      if ((e.metaKey || e.ctrlKey) && e.key === "Enter") {
                        e.preventDefault();
                        e.currentTarget.form?.requestSubmit();
                      }
                    }}
                  />
                  {composerMode === "work" && (
                    <label className="agent-picker">
                      Assign to
                      <select
                        value={agentID}
                        disabled={busy || !!pendingSubmission}
                        aria-describedby={
                          agentID &&
                          !pendingSubmission &&
                          !assignedAgentAvailable &&
                          !loading
                            ? "draft-agent-unavailable"
                            : undefined
                        }
                        onChange={(e) => setAgentID(e.target.value)}
                      >
                        <option value="">Choose an agent</option>
                        {!!agentID && !assignedAgentAvailable && (
                          <option value={agentID} disabled>
                            {loading
                              ? "Loading saved agent…"
                              : "Previously selected agent (unavailable)"}
                          </option>
                        )}
                        {activeAgents.map((a) => (
                          <option key={a.deployment.id} value={a.deployment.id}>
                            {a.definition?.displayName || a.deployment.id}
                          </option>
                        ))}
                      </select>
                    </label>
                  )}
                  {composerMode === "work" &&
                    !!agentID &&
                    !pendingSubmission &&
                    !assignedAgentAvailable &&
                    !loading && (
                      <p id="draft-agent-unavailable" className="inline-help">
                        Your draft is saved. Its assigned agent is unavailable.
                        Choose an active agent before starting work.
                      </p>
                    )}
                  <div className="composer-footer">
                    <span>
                      <ShieldCheck size={14} />
                      {composerMode !== "work"
                        ? "Review the proposal before anything is activated."
                        : "Progress is saved as your agent works."}
                    </span>
                    <button
                      className="button primary"
                      type="submit"
                      disabled={
                        busy ||
                        recoveryError !== "" ||
                        desktop.state !== "ready" ||
                        (!pendingSubmission &&
                          ((!prompt.trim() && canPropose) ||
                            (composerMode === "work" &&
                              (!canRun ||
                                !assignedAgentAvailable ||
                                !prompt.trim()))))
                      }
                    >
                      {busy ? (
                        <LoaderCircle size={16} className="spin" />
                      ) : null}
                      {busy
                        ? "Saving request…"
                        : pendingSubmission
                          ? "Retry saved request"
                          : composerMode !== "work"
                            ? canPropose
                              ? "Create proposal"
                              : "Check setup"
                            : "Start work"}
                      {!busy && <ArrowUp size={16} />}
                    </button>
                  </div>
                  {(pendingSubmission || submissionError) && (
                    <section
                      className="home-recovery"
                      aria-label="Saved request recovery"
                    >
                      <p
                        id="home-submission-feedback"
                        ref={submissionFeedback}
                        tabIndex={-1}
                        role={submissionError ? "alert" : "status"}
                        className={
                          submissionError ? "error-text" : "inline-help"
                        }
                      >
                        {submissionError ||
                          (busy
                            ? "Saving your request. Its recovery identity is stored on this device."
                            : "A saved request needs confirmation. The original text and destination are kept. Retry the saved request before creating another proposal or task.")}
                      </p>
                      {recoveryError && (
                        <button
                          type="button"
                          className="button"
                          onClick={retryRecovery}
                        >
                          Retry recovery
                        </button>
                      )}
                      {!busy &&
                        (pendingSubmission || recoveryError) &&
                        (forgetRequest ? (
                          <div>
                            <p id="home-forget-help" className="inline-help">
                              Forget only this device’s retry record? This does
                              not cancel saved proposals or work. Check Home and
                              Work first: sending the same prompt again may
                              create another record.
                            </p>
                            <div className="inspector-actions">
                              <button
                                type="button"
                                className="button"
                                ref={forgetConfirm}
                                aria-describedby="home-forget-help"
                                onClick={forgetSavedRequest}
                              >
                                Forget local retry record
                              </button>
                              <button
                                type="button"
                                className="button"
                                onClick={() => {
                                  setForgetRequest(false);
                                  requestAnimationFrame(() =>
                                    forgetTrigger.current?.focus(),
                                  );
                                }}
                              >
                                Keep retry record
                              </button>
                            </div>
                          </div>
                        ) : (
                          <button
                            type="button"
                            className="button"
                            ref={forgetTrigger}
                            onClick={() => setForgetRequest(true)}
                          >
                            Forget saved request…
                          </button>
                        ))}
                    </section>
                  )}
                  {composerMode === "team" && (
                    <p className="inline-help">
                      OpenSeal will request one team with new agents for your
                      goal. Review their roles, instructions, and permissions
                      before installation. Your prompt is saved as a draft.
                    </p>
                  )}
                  {composerMode === "work" &&
                    canRun &&
                    activeAgents.length > 0 && (
                      <p className="inline-help">
                        Tasks use your workspace model with a limit of 16 turns
                        and 15 minutes. Ask for a text, Markdown, CSV, or JSON
                        file to preview and save from the result. Reading files
                        from your computer and image input are not available
                        yet.
                      </p>
                    )}
                  {composerMode === "work" &&
                    (!canRun || !activeAgents.length) && (
                      <p className="inline-help">
                        {!activeAgents.length
                          ? "Create and activate an agent before starting work."
                          : "Work execution is not configured in this workspace."}
                      </p>
                    )}
                </form>
                {!agents.length && !proposal && (
                  <div className="starter-section">
                    <span className="section-label">A few places to begin</span>
                    <div className="starters">
                      {starters.map(({ title, text, icon: Icon }) => (
                        <button
                          key={title}
                          onClick={() => {
                            setPrompt(text);
                            setComposerMode("agent");
                            requestAnimationFrame(() =>
                              composer.current?.focus(),
                            );
                          }}
                        >
                          <Icon size={17} />
                          <span>{title}</span>
                          <ArrowRight size={14} />
                        </button>
                      ))}
                    </div>
                  </div>
                )}
                {!canPropose && !loading && (
                  <div className="setup-strip">
                    <span className="setup-symbol">
                      <Sparkles size={19} />
                    </span>
                    <div>
                      <strong>One step before your first agent</strong>
                      <p>
                        Check workspace configuration to enable agent creation.
                      </p>
                    </div>
                    <button
                      className="text-button"
                      onClick={() => navigate("Settings")}
                    >
                      Open provider settings <ArrowRight size={15} />
                    </button>
                  </div>
                )}
                {proposal && (
                  <ProposalReview
                    key={proposal.id}
                    proposal={proposal}
                    onDerived={(next) => {
                      if (
                        activeProposal.current?.id !== proposal.id ||
                        next.parentId !== proposal.id
                      )
                        return;
                      activeProposal.current = next;
                      setProposal(next);
                      setProposalID(next.id);
                      saveLocal("openseal.proposal", next.id);
                      requestAnimationFrame(() =>
                        document.getElementById("proposal-heading")?.focus(),
                      );
                    }}
                    onInstalled={() => void refresh(true)}
                    onViewAgents={() => navigate("Agents")}
                    onViewTeams={() => navigate("Teams")}
                    onChange={(next) =>
                      setProposal((current) =>
                        current?.id === next.id &&
                        next.revision >= current.revision
                          ? next
                          : current,
                      )
                    }
                  />
                )}
                <section className="home-section">
                  <div className="section-heading">
                    <h2>Recent work</h2>
                    <button
                      className="text-button"
                      onClick={() => {
                        setWorkView("tasks");
                        navigate("Work");
                      }}
                    >
                      View all <ArrowRight size={14} />
                    </button>
                  </div>
                  {loading ? (
                    <div
                      className="skeleton-list"
                      aria-label="Loading work"
                      aria-busy="true"
                    >
                      <span />
                      <span />
                      <span />
                    </div>
                  ) : runs.length ? (
                    runRows(runs.slice(0, 4))
                  ) : (
                    <Empty
                      icon={Layers3}
                      title="Your work will find a home here"
                    >
                      Once an agent starts a task, follow its progress and
                      return to the results here.
                    </Empty>
                  )}
                </section>
                <section className="home-section">
                  <div className="section-heading">
                    <h2>Your agents</h2>
                    <button
                      className="text-button"
                      onClick={() => navigate("Agents")}
                    >
                      View all <ArrowRight size={14} />
                    </button>
                  </div>
                  {agents.length ? (
                    agentRows(agents.slice(0, 3))
                  ) : (
                    <div className="agent-empty">
                      <span className="agent-empty-symbol">
                        <Users size={21} />
                      </span>
                      <div>
                        <strong>A workspace with room to grow</strong>
                        <p>
                          Your agents will appear here, ready for their next
                          task.
                        </p>
                      </div>
                      <button className="button" onClick={newAgent}>
                        Create your first agent
                      </button>
                    </div>
                  )}
                </section>
              </>
            )}
            {page === "Agents" && (
              <>
                <div className="list-toolbar">
                  <label className="filter-search">
                    <Search size={16} />
                    <span className="sr-only">Search agents</span>
                    <input
                      type="search"
                      placeholder="Search agents…"
                      value={query}
                      onChange={(e) => setQuery(e.target.value)}
                    />
                  </label>
                  <span className="result-count">
                    {filteredAgents.length} agents
                  </span>
                </div>
                {loading ? (
                  <div
                    className="skeleton-list"
                    aria-busy="true"
                    aria-label="Loading workspace"
                  >
                    <span />
                    <span />
                    <span />
                  </div>
                ) : filteredAgents.length ? (
                  agentRows(filteredAgents)
                ) : (
                  <Empty
                    icon={Users}
                    title={
                      query ? "No matching agents" : "Meet your next teammate"
                    }
                    action={
                      <button
                        className={query ? "button" : "button primary"}
                        onClick={query ? () => setQuery("") : newAgent}
                      >
                        {!query && <Plus size={16} />}
                        {query ? "Clear search" : "Create an agent"}
                      </button>
                    }
                  >
                    {query
                      ? "Try another name or a word from the agent’s purpose."
                      : "Describe the help you need. OpenSeal will prepare an agent proposal for you to review."}
                  </Empty>
                )}
              </>
            )}
            {page === "Teams" && (
              <TeamBrowser
                capabilities={caps}
                onOpenRun={(run, detail) =>
                  selectRecord({ kind: "run", value: run, ...detail })
                }
                onOpenProposal={(next) => {
                  activeProposal.current = next;
                  setProposal(next);
                  setProposalID(next.id);
                  saveLocal("openseal.proposal", next.id);
                  navigate("Home");
                  setComposerMode("team");
                  requestAnimationFrame(() =>
                    document.getElementById("proposal-heading")?.focus(),
                  );
                }}
                available={supports(caps, "team-definitions", "list")}
                refreshToken={workRefresh}
                agents={agents}
                onOpenAgent={(agent) =>
                  selectRecord({ kind: "agent", value: agent })
                }
              />
            )}
            {page === "Work" && (
              <>
                <div
                  className="composer-tabs work-views"
                  role="group"
                  aria-label="Work view"
                >
                  <button
                    aria-pressed={workView === "tasks"}
                    onClick={() => {
                      setWorkView("tasks");
                      setSelection(null);
                    }}
                  >
                    Tasks
                  </button>
                  <button
                    aria-pressed={workView === "requests"}
                    onClick={() => {
                      setWorkView("requests");
                      setSelection(null);
                    }}
                  >
                    Requests
                  </button>
                </div>
                {workView === "requests" ? (
                  <RequestInbox
                    canNameTeams={supports(caps, "team-definitions", "list")}
                    available={supports(caps, "agent-requests", "list")}
                    canInspect={
                      supports(caps, "agent-runs", "get") &&
                      supports(caps, "agent-requests", "get")
                    }
                    refreshToken={workRefresh}
                    agents={agents}
                    selectedId={
                      selection?.kind === "run"
                        ? selection.requestId
                        : undefined
                    }
                    onOpen={(run, requestId, trigger) => {
                      selectionTrigger.current = trigger;
                      setSelection({ kind: "run", value: run, requestId });
                    }}
                  />
                ) : (
                  <WorkBrowser
                    refreshToken={workRefresh}
                    available={supports(caps, "agent-runs", "list")}
                    renderRows={runRows}
                    onResults={updateInspectedRun}
                    onStart={() => {
                      navigate("Home");
                      setComposerMode(agents.length ? "work" : "agent");
                      requestAnimationFrame(() => composer.current?.focus());
                    }}
                  />
                )}
              </>
            )}
            {page === "Reviews" && (
              <ReviewInbox
                available={supports(caps, "action-approvals", "list")}
                canInspect={
                  supports(caps, "agent-runs", "get") &&
                  supports(caps, "action-approvals", "get") &&
                  supports(caps, "action-calls", "get")
                }
                refreshToken={workRefresh}
                selectedId={
                  selection?.kind === "run" ? selection.approvalId : undefined
                }
                onOpen={(run, approvalId, trigger) => {
                  selectionTrigger.current = trigger;
                  setSelection({ kind: "run", value: run, approvalId });
                }}
              />
            )}
            {page === "Settings" && (
              <div className="settings-content">
                <section className="settings-section">
                  <h2>Workspace</h2>
                  <div className="setting-row">
                    <div>
                      <strong>Local storage</strong>
                      <p>
                        Agents, work, and artifacts are stored on this computer.
                      </p>
                      <code>
                        {desktop.workspace || "Waiting for the workspace…"}
                      </code>
                    </div>
                    <IconButton
                      icon={Copy}
                      title="Copy workspace location"
                      disabled={!desktop.workspace}
                      onClick={() => {
                        void navigator.clipboard
                          .writeText(desktop.workspace)
                          .then(
                            () => setNotice("Workspace location copied."),
                            () =>
                              setError(
                                "Could not copy the location. Select and copy the path instead.",
                              ),
                          );
                      }}
                    />
                  </div>
                  <div className="setting-row">
                    <div>
                      <strong>Connection</strong>
                      <p>
                        {connected
                          ? "The workspace is connected on this computer."
                          : connectionError ||
                            desktop.message ||
                            "The local daemon is starting."}
                      </p>
                    </div>
                    <StatusPill
                      value={
                        connected
                          ? "connected"
                          : connectionError
                            ? "unreachable"
                            : desktop.state
                      }
                    />
                  </div>
                </section>
                <ProviderSettings onSaved={() => refresh()} />
                <section className="settings-section">
                  <h2>Appearance</h2>
                  <div className="setting-row">
                    <div>
                      <strong>Theme</strong>
                      <p>Choose a look, or follow your system.</p>
                    </div>
                    <label>
                      <span className="sr-only">Theme</span>
                      <select
                        aria-label="Theme"
                        value={theme}
                        onChange={(e) => setTheme(e.target.value)}
                      >
                        <option value="system">System</option>
                        <option value="light">Light</option>
                        <option value="dark">Dark</option>
                      </select>
                    </label>
                  </div>
                </section>
                <section className="settings-section">
                  <h2>Available capabilities</h2>
                  <p className="muted">
                    The interface follows what your connected workspace can do.
                  </p>
                  <div className="capability-list">
                    {caps?.capabilities
                      .filter((c) => c.available)
                      .map((c) => (
                        <span key={c.id}>
                          <Check size={13} />
                          {label(c.id.replaceAll("-", " "))}
                        </span>
                      ))}
                  </div>
                </section>
              </div>
            )}
          </main>
          {selection && (
            <aside
              className="inspector"
              ref={inspector}
              tabIndex={-1}
              aria-label={
                selection.kind === "agent" ? "Agent details" : "Work details"
              }
            >
              <div className="inspector-toolbar">
                <strong>
                  {selection.kind === "agent"
                    ? "Agent details"
                    : "Work details"}
                </strong>
                <IconButton
                  icon={X}
                  title="Close details"
                  onClick={closeInspector}
                />
              </div>
              {selection.kind === "agent" ? (
                <div className="inspector-body">
                  <span className="avatar large">
                    {(selection.value.definition?.displayName || "A").slice(
                      0,
                      1,
                    )}
                  </span>
                  <h2>
                    {selection.value.deployment.displayName ||
                      selection.value.definition?.displayName}
                  </h2>
                  <StatusPill
                    value={selection.value.deployment.rolloutStatus}
                  />
                  <p>{selection.value.definition?.purpose}</p>
                  {canRun &&
                    selection.value.deployment.rolloutStatus === "active" && (
                      <button
                        className="button primary"
                        onClick={() => {
                          const id = selection.value.deployment.id;
                          navigate("Home");
                          setComposerMode("work");
                          setAgentID(id);
                          composer.current?.focus();
                        }}
                      >
                        <Play size={15} />
                        Start work
                      </button>
                    )}
                  <dl>
                    <dt>Version</dt>
                    <dd>{selection.value.deployment.activeVersion}</dd>
                    <dt>Deployment</dt>
                    <dd>{selection.value.deployment.id}</dd>
                  </dl>
                  <h3>Instructions</h3>
                  <p className="preserve-lines">
                    {selection.value.definition?.systemPrompt}
                  </p>
                  {selection.value.definition?.operatingPrinciples?.length ? (
                    <>
                      <h3>Operating principles</h3>
                      <ul>
                        {selection.value.definition.operatingPrinciples.map(
                          (p) => (
                            <li key={p}>{p}</li>
                          ),
                        )}
                      </ul>
                    </>
                  ) : null}
                </div>
              ) : (
                <div className="inspector-body">
                  <StatusPill value={selection.value.status} />
                  <h2>{selection.value.goal}</h2>
                  <WorkStatus
                    key={`WorkStatus-${selection.value.id}`}
                    hasOtherDraft={
                      !!prompt.trim() && prompt !== selection.value.goal
                    }
                    run={selection.value}
                    onProvider={() => navigate("Settings")}
                    onNewAttempt={
                      !pendingSubmission &&
                      canRun &&
                      selection.value.kind === "agent_work"
                        ? () => {
                            const previous = selection.value;
                            navigate("Home");
                            setComposerMode("work");
                            setPrompt(previous.goal);
                            setAgentID(
                              activeAgents.some(
                                (agent) =>
                                  agent.deployment.id ===
                                  previous.assignedAgentId,
                              )
                                ? previous.assignedAgentId || ""
                                : "",
                            );
                            setNotice(
                              "Task copied to the composer. Review it before starting a new run.",
                            );
                            requestAnimationFrame(() =>
                              composer.current?.focus(),
                            );
                          }
                        : undefined
                    }
                  />
                  {supports(caps, "action-approvals", "get") &&
                    supports(caps, "action-calls", "get") && (
                      <RunApproval
                        key={`approval-${selection.value.id}-${selection.approvalId || "current"}:${selection.requestId || "all"}`}
                        approvalId={selection.approvalId}
                        run={selection.value}
                        canResolve={supports(
                          caps,
                          "action-approvals",
                          "resolve",
                        )}
                        onResolved={(next) => {
                          updateInspectedRun([next]);
                          void refresh(true);
                          setRuns((current) =>
                            current.map((run) =>
                              run.id === next.id &&
                              next.revision >= run.revision
                                ? next
                                : run,
                            ),
                          );
                        }}
                      />
                    )}
                  <WorkControls
                    agents={agents}
                    key={`WorkControls-${selection.value.id}-${selection.requestId || "all"}`}
                    requestId={selection.requestId}
                    onOpenRun={(next) =>
                      selectRecord({ kind: "run", value: next })
                    }
                    run={selection.value}
                    capabilities={caps}
                    onChange={(next) => {
                      setRuns((current) =>
                        current.map((run) =>
                          run.id === next.id && next.revision >= run.revision
                            ? next
                            : run,
                        ),
                      );
                      setSelection((current) =>
                        current?.kind === "run" &&
                        current.value.id === next.id &&
                        next.revision >= current.value.revision
                          ? { ...current, value: next }
                          : current,
                      );
                    }}
                  />
                  {selection.value.error && (
                    <p className="error-text">{selection.value.error}</p>
                  )}
                  <dl>
                    <dt>Owner</dt>
                    <dd>{selection.value.owner?.id}</dd>
                    <dt>Started</dt>
                    <dd>{date(selection.value.createdAt)}</dd>
                    <dt>Updated</dt>
                    <dd>{date(selection.value.updatedAt)}</dd>
                    <dt>Run</dt>
                    <dd>{selection.value.id}</dd>
                  </dl>
                  <WorkResult
                    key={`WorkResult-${selection.value.id}`}
                    run={selection.value}
                  />
                  {supports(caps, "artifacts", "list") && (
                    <TaskArtifacts
                      key={`artifacts-${selection.value.id}`}
                      run={selection.value}
                      canDownload={supports(caps, "artifacts", "download")}
                    />
                  )}
                  {supports(caps, "activity", "list") && (
                    <WorkHistory
                      key={`WorkHistory-${selection.value.id}`}
                      run={selection.value}
                    />
                  )}
                </div>
              )}
            </aside>
          )}
        </div>
      </div>
      {notice && (
        <div className="toast" role="status">
          <Check size={16} />
          {notice}
          <IconButton
            icon={X}
            title="Dismiss notification"
            onClick={() => setNotice("")}
          />
        </div>
      )}
      <dialog
        aria-label="Go to a page"
        ref={palette}
        className="command-dialog"
        onCancel={() => setCommandOpen(false)}
        onClick={(e) => {
          if (e.target === e.currentTarget) setCommandOpen(false);
        }}
      >
        <div className="command-search">
          <Search size={19} />
          <input
            aria-label="Search commands"
            placeholder="Where would you like to go?"
            value={commandQuery}
            onChange={(e) => setCommandQuery(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "ArrowDown") {
                e.preventDefault();
                palette.current
                  ?.querySelector<HTMLButtonElement>(".command-option")
                  ?.focus();
              }
            }}
          />
          <IconButton
            icon={X}
            title="Close command menu"
            onClick={() => setCommandOpen(false)}
          />
        </div>
        <div className="command-options">
          {[
            ...pages,
            { name: "Settings" as Page, icon: Settings2, shortcut: "" },
          ]
            .filter((p) =>
              p.name.toLowerCase().includes(commandQuery.toLowerCase()),
            )
            .map(({ name, icon: Icon, shortcut }) => (
              <button
                className="command-option"
                key={name}
                onClick={() => {
                  setCommandOpen(false);
                  navigate(name);
                }}
                onKeyDown={(e) => {
                  if (e.key === "ArrowDown") {
                    e.preventDefault();
                    (
                      e.currentTarget.nextElementSibling as HTMLElement
                    )?.focus();
                  }
                  if (e.key === "ArrowUp") {
                    e.preventDefault();
                    (
                      e.currentTarget.previousElementSibling as HTMLElement
                    )?.focus();
                  }
                }}
              >
                <Icon size={18} />
                <span>Go to {name}</span>
                {shortcut && (
                  <kbd>
                    {modifier} {shortcut}
                  </kbd>
                )}
              </button>
            ))}
          {![...pages, { name: "Settings" }].some((p) =>
            p.name.toLowerCase().includes(commandQuery.toLowerCase()),
          ) && (
            <p className="command-empty">
              No matching destination. Try Home, Agents, Work, Reviews, Teams,
              or Settings.
            </p>
          )}
        </div>
        <footer>
          <span>
            <ArrowDown size={12} />
            <ArrowUp size={12} /> to navigate
          </span>
          <span>Enter to open</span>
          <span>Esc to close</span>
        </footer>
      </dialog>
      <dialog
        aria-label="Keyboard shortcuts"
        ref={help}
        className="help-dialog"
        onCancel={() => setHelpOpen(false)}
      >
        <div className="section-heading">
          <h2>Make yourself at home</h2>
          <IconButton
            icon={X}
            title="Close shortcuts"
            onClick={() => setHelpOpen(false)}
          />
        </div>
        <p className="muted">A few shortcuts to keep you in the flow.</p>
        <dl>
          {[
            ["Go to a page", `${modifier} K`],
            ["Create an agent", `${modifier} N`],
            [
              "Home / Agents / Work / Reviews / Teams",
              `${modifier} 1 / 2 / 3 / 4 / 5`,
            ],
            ["Submit a prompt", `${modifier} Enter`],
            ["Close details or a menu", "Esc"],
          ].map(([name, key]) => (
            <div key={name}>
              <dt>{name}</dt>
              <dd>
                <kbd>{key}</kbd>
              </dd>
            </div>
          ))}
        </dl>
      </dialog>
    </div>
  );
}
