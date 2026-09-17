import { useEffect, useRef, useState } from "react";
import {
  api,
  message,
  scoped,
  supports,
  type Capabilities,
  type Agent,
  type Run,
} from "./api";
export type CollaborationRequest = {
  id: string;
  sourceRunId: string;
  childRunId?: string;
  status: string;
  kind: string;
  goal: string;
  revision: number;
  requester: { type: string; id: string };
  recipient: { type: string; id: string };
  clarification?: string;
  response?: string;
  completionSummary?: string;
  resolutionReason?: string;
  completionReviewSummary?: string;
  reviewDisagreement?: boolean;
};
export default function WorkCollaboration({
  run,
  capabilities,
  onGuide,
  onOpen,
  agents,
  requestId,
}: {
  run: Run;
  requestId?: string;
  agents: Agent[];
  capabilities: Capabilities | null;
  onGuide: (request: CollaborationRequest) => void;
  onOpen: (run: Run) => void;
}) {
  const [open, setOpen] = useState(
    !!requestId || run.status === "waiting_for_agent",
  );
  const [focusedId, setFocusedId] = useState(requestId);
  const [items, setItems] = useState<CollaborationRequest[]>([]);
  const [offset, setOffset] = useState(0);
  const [more, setMore] = useState(false);
  const [busy, setBusy] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState("");
  const [refresh, setRefresh] = useState(0);
  const [opening, setOpening] = useState("");
  const [openError, setOpenError] = useState("");
  const alive = useRef(true),
    openingLock = useRef(false);
  const feedback = useRef<HTMLParagraphElement>(null);
  const focusAfterLoad = useRef(false);
  function partyName(party: { type: string; id: string }) {
    const agent =
      party.type === "agent"
        ? agents.find((item) => item.deployment.id === party.id)
        : undefined;
    return (
      agent?.deployment.displayName ||
      agent?.definition.displayName ||
      (party.type === "team" &&
      run.owner?.type === "team" &&
      party.id === run.owner.id
        ? "This team"
        : party.id)
    );
  }
  const canList = supports(capabilities, "agent-requests", "list");
  const canRead = focusedId
    ? supports(capabilities, "agent-requests", "get")
    : canList;
  const canInspect = supports(capabilities, "agent-runs", "get");
  const canGuide =
    supports(capabilities, "agent-runs", "intervene") &&
    !["completed", "failed", "canceled"].includes(run.status);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);
  useEffect(() => {
    if (!open || !canRead) return;
    let canceled = false,
      inflight = false;
    async function load() {
      if (inflight) return;
      inflight = true;
      try {
        let result: CollaborationRequest[] | null;
        if (focusedId) {
          const item = await api<CollaborationRequest>(
            scoped(`/agent-requests/${encodeURIComponent(focusedId)}`),
          );
          if (item.id !== focusedId || item.sourceRunId !== run.id)
            throw new Error("The returned request does not match this task.");
          result = [item];
        } else {
          result = await api<CollaborationRequest[] | null>(
            scoped(
              `/agent-requests?sourceRunId=${encodeURIComponent(run.id)}&limit=21&offset=${offset}`,
            ),
          );
        }
        if (canceled) return;
        if (result !== null && !Array.isArray(result))
          throw new Error("Unreadable collaboration list.");
        setItems((result || []).slice(0, 20));
        setMore((result || []).length > 20);
        setLoaded(true);
        setError("");
      } catch (e) {
        if (!canceled)
          setError(
            `Could not refresh requests. ${message(e)} Previously loaded requests may be out of date.`,
          );
      } finally {
        inflight = false;
        if (!canceled) {
          setBusy(false);
          if (focusAfterLoad.current) {
            focusAfterLoad.current = false;
            requestAnimationFrame(() => feedback.current?.focus());
          }
        }
      }
    }
    setBusy(true);
    void load();
    const timer = setInterval(() => void load(), 3000);
    return () => {
      canceled = true;
      clearInterval(timer);
    };
  }, [open, canRead, run.id, offset, refresh, focusedId]);
  async function inspect(id: string) {
    if (openingLock.current || !canInspect) return;
    openingLock.current = true;
    setOpening(id);
    setOpenError("");
    try {
      const next = await api<Run>(
        scoped(`/agent-runs/${encodeURIComponent(id)}`),
      );
      if (!alive.current) return;
      if (next.id !== id)
        throw new Error("The returned run did not match this request.");
      onOpen(next);
    } catch (e) {
      if (alive.current)
        setOpenError(`Could not open related work. ${message(e)}`);
    } finally {
      openingLock.current = false;
      if (alive.current) setOpening("");
    }
  }
  function page(next: number) {
    setItems([]);
    setLoaded(false);
    setOffset(next);
    focusAfterLoad.current = true;
  }
  if (!canRead && !run.parentRunId) return null;
  return (
    <section className="work-collaboration" aria-label="Collaboration">
      {run.parentRunId && canInspect && (
        <button
          className="button"
          disabled={!!opening}
          onClick={() => void inspect(run.parentRunId!)}
        >
          View parent work
        </button>
      )}
      {openError && (
        <p role="alert" className="error-text">
          {openError}
        </p>
      )}
      {canRead && (
        <>
          <button
            className="button"
            aria-expanded={open}
            onClick={() => setOpen((value) => !value)}
          >
            {open ? "Hide collaboration" : "Delegation and requests"}
          </button>
          {open && (
            <div>
              <h3>Collaboration</h3>
              {focusedId && canList && (
                <button
                  className="button"
                  onClick={() => {
                    setFocusedId(undefined);
                    setItems([]);
                    setLoaded(false);
                    setOffset(0);
                  }}
                >
                  Show all requests for this task
                </button>
              )}
              <p className="muted">
                Requests sent by this run. Agents handle acceptance,
                clarification, and completion review. You can guide the lead
                while work is active.
              </p>
              <button
                className="button"
                disabled={busy}
                onClick={() => {
                  focusAfterLoad.current = true;
                  setRefresh((value) => value + 1);
                }}
              >
                Refresh requests
              </button>
              <p
                ref={feedback}
                tabIndex={-1}
                role={error ? "alert" : "status"}
                className={error ? "error-text" : "muted"}
              >
                {error ||
                  (busy
                    ? "Loading requests…"
                    : loaded
                      ? `${items.length} request${items.length === 1 ? "" : "s"} on this page.`
                      : "")}
              </p>
              {!busy && loaded && !items.length && !error && (
                <p className="muted">No outgoing requests on this page.</p>
              )}
              <ol className="collaboration-requests" start={offset + 1}>
                {items.map((request) => (
                  <li key={request.id}>
                    <h4>{request.goal}</h4>
                    <p>
                      {request.status.replaceAll("_", " ")} · {request.kind}
                    </p>
                    <dl>
                      <dt>From</dt>
                      <dd title={request.requester.id}>
                        {partyName(request.requester)}
                      </dd>
                      <dt>To</dt>
                      <dd title={request.recipient.id}>
                        {partyName(request.recipient)}
                      </dd>
                    </dl>
                    {request.clarification && (
                      <>
                        <h4>Recipient’s question</h4>
                        <p className="collaboration-text">
                          {request.clarification}
                        </p>
                      </>
                    )}
                    {request.response &&
                      request.response !== request.clarification && (
                        <>
                          <h4>Latest response</h4>
                          <p className="collaboration-text">
                            {request.response}
                          </p>
                        </>
                      )}
                    {request.completionSummary && (
                      <>
                        <h4>Completion summary</h4>
                        <p className="collaboration-text">
                          {request.completionSummary}
                        </p>
                      </>
                    )}
                    {request.completionReviewSummary && (
                      <p className="collaboration-text">
                        Review: {request.completionReviewSummary}
                      </p>
                    )}
                    {request.resolutionReason && (
                      <p className="collaboration-text">
                        Resolution: {request.resolutionReason}
                      </p>
                    )}
                    {request.reviewDisagreement && (
                      <p className="inline-help">
                        Reviewers disagree. The team’s saved escalation policy
                        determines what happens next.
                      </p>
                    )}
                    {request.status === "clarification_requested" && (
                      <>
                        <p className="inline-help">
                          The recipient asked the lead for more information.
                          Guidance goes to the lead; it does not directly answer
                          or approve this request.
                        </p>
                        {canGuide && (
                          <button
                            className="button"
                            onClick={() => onGuide(request)}
                          >
                            Guide the lead
                          </button>
                        )}
                      </>
                    )}
                    {request.childRunId && canInspect && (
                      <button
                        className="button"
                        disabled={!!opening}
                        onClick={() => void inspect(request.childRunId!)}
                      >
                        {opening === request.childRunId
                          ? "Opening delegated work…"
                          : "View delegated work"}
                      </button>
                    )}
                  </li>
                ))}
              </ol>
              {(offset > 0 || more) && (
                <div className="team-work-pages">
                  <button
                    className="button"
                    disabled={busy || offset === 0}
                    onClick={() => page(Math.max(0, offset - 20))}
                  >
                    Previous requests
                  </button>
                  <button
                    className="button"
                    disabled={busy || !more}
                    onClick={() => page(offset + 20)}
                  >
                    More requests
                  </button>
                </div>
              )}
            </div>
          )}
        </>
      )}
    </section>
  );
}
