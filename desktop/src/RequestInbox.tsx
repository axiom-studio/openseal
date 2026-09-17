import { useEffect, useRef, useState } from "react";
import { ChevronRight, MessagesSquare } from "lucide-react";
import { api, message, scoped, type Agent, type Run } from "./api";
import type { Team } from "./TeamBrowser";
import type { CollaborationRequest } from "./WorkCollaboration";

const size = 20;
export default function RequestInbox({
  available,
  canNameTeams,
  canInspect,
  refreshToken,
  agents,
  onOpen,
  selectedId,
}: {
  available: boolean;
  canNameTeams: boolean;
  canInspect: boolean;
  refreshToken: number;
  agents: Agent[];
  onOpen: (run: Run, requestId: string, trigger: HTMLElement | null) => void;
  selectedId?: string;
}) {
  const [teams, setTeams] = useState<Team[]>([]);
  const [status, setStatus] = useState("clarification_requested");
  const [offset, setOffset] = useState(0);
  const [items, setItems] = useState<CollaborationRequest[]>([]);
  const [more, setMore] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [busy, setBusy] = useState(true);
  const [error, setError] = useState("");
  const [opening, setOpening] = useState("");
  const [openError, setOpenError] = useState("");
  const [retry, setRetry] = useState(0);
  const epoch = useRef(0);
  const openingEpoch = useRef(0);
  const openingLock = useRef(false);
  const focusAfter = useRef(false);
  const feedback = useRef<HTMLParagraphElement>(null);
  useEffect(
    () => () => {
      openingEpoch.current++;
    },
    [],
  );
  useEffect(() => {
    if (!available) return;
    const generation = ++epoch.current;
    let inflight = false;
    async function load() {
      if (inflight) return;
      inflight = true;
      try {
        const params = new URLSearchParams({
          limit: String(size + 1),
          offset: String(offset),
        });
        if (status !== "all") params.set("status", status);
        const result = await api<CollaborationRequest[] | null>(
          scoped(`/agent-requests?${params}`),
        );
        if (generation !== epoch.current) return;
        if (result !== null && !Array.isArray(result))
          throw new Error("Unreadable request list.");
        setItems((result || []).slice(0, size));
        setMore((result || []).length > size);
        setLoaded(true);
        setError("");
      } catch (e) {
        if (generation === epoch.current)
          setError(`Could not refresh requests. ${message(e)}`);
      } finally {
        inflight = false;
        if (generation === epoch.current) {
          setBusy(false);
          if (focusAfter.current) {
            focusAfter.current = false;
            requestAnimationFrame(() => feedback.current?.focus());
          }
        }
      }
    }
    setBusy(true);
    void load();
    const timer = setInterval(() => void load(), 5000);
    return () => {
      epoch.current++;
      clearInterval(timer);
    };
  }, [available, status, offset, refreshToken, retry]);
  useEffect(() => {
    if (!available || !canNameTeams) {
      setTeams([]);
      return;
    }
    let canceled = false;
    void api<{ items: Team[] | null }>(scoped("/team-deployments"))
      .then((result) => {
        if (!canceled)
          setTeams(Array.isArray(result?.items) ? result.items : []);
      })
      .catch(() => {
        if (!canceled) setTeams([]);
      });
    return () => {
      canceled = true;
    };
  }, [available, canNameTeams, refreshToken]);
  function changeView(nextStatus: string, nextOffset: number) {
    openingEpoch.current++;
    openingLock.current = false;
    setOpening("");
    setOpenError("");
    setError("");
    setItems([]);
    setMore(false);
    setLoaded(false);
    setBusy(true);
    setStatus(nextStatus);
    setOffset(nextOffset);
  }
  async function open(request: CollaborationRequest) {
    if (openingLock.current || !canInspect) return;
    openingLock.current = true;
    const generation = ++openingEpoch.current;
    const trigger = document.activeElement as HTMLElement | null;
    setOpening(request.id);
    setOpenError("");
    try {
      const run = await api<Run>(
        scoped(`/agent-runs/${encodeURIComponent(request.sourceRunId)}`),
      );
      if (generation !== openingEpoch.current) return;
      if (run.id !== request.sourceRunId)
        throw new Error("The returned task does not match this request.");
      onOpen(run, request.id, trigger);
    } catch (e) {
      if (generation === openingEpoch.current)
        setOpenError(
          `Could not open this request. ${message(e)} Select it again to retry.`,
        );
    } finally {
      if (generation === openingEpoch.current) {
        openingLock.current = false;
        setOpening("");
      }
    }
  }
  function partyName(party: CollaborationRequest["recipient"]) {
    const agent =
      party.type === "agent"
        ? agents.find((item) => item.deployment.id === party.id)
        : undefined;
    return (
      teams.find(
        (team) => party.type === "team" && team.deployment.id === party.id,
      )?.definition.displayName ||
      agent?.deployment.displayName ||
      agent?.definition.displayName ||
      `${party.type === "team" ? "Team" : "Agent"} ${party.id}`
    );
  }
  if (!available)
    return (
      <p className="inline-help">
        Collaboration requests are unavailable in this workspace.
      </p>
    );
  return (
    <section aria-label="Workspace requests">
      <div className="list-toolbar">
        <label className="filter-select">
          <span className="sr-only">Request status</span>
          <select
            value={status}
            onChange={(e) => changeView(e.target.value, 0)}
          >
            <option value="clarification_requested">
              Clarification requested
            </option>
            <option value="pending">Awaiting acceptance</option>
            <option value="accepted">Accepted</option>
            <option value="completion_review">Completion review</option>
            <option value="completed">Completed</option>
            <option value="failed">Failed</option>
            <option value="rejected">Rejected</option>
            <option value="canceled">Canceled</option>
            <option value="all">All requests</option>
          </select>
        </label>
        <button
          className="button"
          disabled={busy}
          onClick={() => {
            focusAfter.current = true;
            setRetry((n) => n + 1);
          }}
        >
          Refresh requests
        </button>
      </div>
      <p className="inline-help">
        Requests between agents across this workspace. Open the parent task to
        inspect the conversation and guide its lead. Agents handle acceptance
        and completion review.
      </p>
      {!canInspect && (
        <p className="inline-help">
          This workspace lists requests but does not provide the task and
          request details needed to open them.
        </p>
      )}
      <p
        ref={feedback}
        tabIndex={-1}
        role={error ? "alert" : "status"}
        className={error ? "error-text" : "muted"}
      >
        {error
          ? `${error}${loaded ? " Previously loaded requests may be out of date." : " Try refreshing again."}`
          : busy
            ? "Loading requests…"
            : items.length
              ? `Showing ${offset + 1}–${offset + items.length}${more ? ". More requests are available." : "."}`
              : "No results."}
      </p>
      {openError && (
        <p role="alert" className="error-text">
          {openError}
        </p>
      )}
      {!loaded && busy ? (
        <div
          className="skeleton-list"
          role="group"
          aria-label="Loading requests"
          aria-busy="true"
        >
          <span />
          <span />
          <span />
        </div>
      ) : items.length ? (
        <div className="record-list">
          {items.map((request) => (
            <button
              key={request.id}
              className={`record-row ${selectedId === request.id ? "selected" : ""}`}
              disabled={!canInspect || !!opening}
              onClick={() => void open(request)}
            >
              <span className="run-icon">
                <MessagesSquare size={20} aria-hidden="true" />
              </span>
              <span className="record-main">
                <strong>{request.goal}</strong>
                <span
                  title={`${request.requester.id} → ${request.recipient.id}`}
                >
                  {partyName(request.requester)} →{" "}
                  {partyName(request.recipient)}
                </span>
                <span>
                  {request.status.replaceAll("_", " ")}
                  {request.clarification ? ` · ${request.clarification}` : ""}
                </span>
              </span>
              {opening === request.id ? (
                <span>Opening…</span>
              ) : (
                <ChevronRight size={16} aria-hidden="true" />
              )}
            </button>
          ))}
        </div>
      ) : loaded && !error ? (
        <div className="empty">
          <MessagesSquare size={27} strokeWidth={1.5} aria-hidden="true" />
          <h3>
            {offset
              ? "No requests on this page"
              : status === "clarification_requested"
                ? "No clarification questions waiting"
                : "No requests in this view"}
          </h3>
          <p>
            {offset
              ? "Requests may have moved as agents responded. Return to the first page."
              : "Requests appear here when agents delegate, hand off, or escalate work. Choose another status to see their progress."}
          </p>
          {offset > 0 && (
            <button className="button" onClick={() => changeView(status, 0)}>
              Back to first page
            </button>
          )}
        </div>
      ) : null}
      <div
        className="inspector-actions"
        role="group"
        aria-label="Request pages"
      >
        <button
          className="button"
          disabled={busy || offset === 0}
          onClick={() => {
            focusAfter.current = true;
            changeView(status, Math.max(0, offset - size));
          }}
        >
          Previous page
        </button>
        <button
          className="button"
          disabled={busy || !more}
          onClick={() => {
            focusAfter.current = true;
            changeView(status, offset + size);
          }}
        >
          Next page
        </button>
      </div>
    </section>
  );
}
