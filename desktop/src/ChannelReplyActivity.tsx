import { useEffect, useId, useRef, useState } from "react";
import { api, message, scope, scoped, type Run } from "./api";
import ChannelMessageContext from "./ChannelMessageContext";
import type { Message } from "./TeamChannels";

type ReplyRun = Run & {
  scope: { kind: string; id: string };
  context: {
    conversationId: string;
    triggerMessageId: string;
    triggerSequence: number;
  };
};
const states: Record<string, string> = {
  queued: "Waiting for a worker",
  planning: "Preparing team replies",
  running: "Reviewing the message",
  paused: "Reply activity is paused",
  sleeping: "Waiting to retry",
  waiting_for_dependency: "Waiting for related work",
  waiting_for_agent: "Waiting for an agent",
  waiting_for_approval: "Waiting for approval",
  waiting_for_event: "Waiting for an event",
  failed: "Reply attempt failed",
  canceled: "Reply attempt canceled",
  completed: "Reply attempt completed",
};
function outcome(run: ReplyRun) {
  if (run.status === "completed") {
    if (run.output?.participationSkipped === true)
      return "Stopped — replies were disabled for this message";
    if (run.output?.speakerCount === 0)
      return "Completed — no reply was offered";
    const count = run.output?.speakerCount;
    if (typeof count === "number" && Number.isSafeInteger(count) && count > 0)
      return `Completed — ${count} ${count === 1 ? "message" : "messages"} posted`;
  }
  if (
    run.status === "paused" &&
    (run.budgetAdmission || run.budgetState === "exhausted")
  )
    return "Paused — a budget limit needs attention";
  return states[run.status];
}
const pageSize = 5;
export default function ChannelReplyActivity({
  id,
  teamId,
  canInspect,
  onOpenRun,
  sender,
}: {
  id: string;
  teamId: string;
  canInspect: boolean;
  onOpenRun: (run: Run) => void;
  sender: (message: Message) => string;
}) {
  const [open, setOpen] = useState(false);
  const [offset, setOffset] = useState(0);
  const [items, setItems] = useState<ReplyRun[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [busy, setBusy] = useState(true);
  const [more, setMore] = useState(false);
  const [error, setError] = useState("");
  const [refresh, setRefresh] = useState(0);
  const [opening, setOpening] = useState("");
  const [openError, setOpenError] = useState("");
  const panelId = useId();
  const feedback = useRef<HTMLParagraphElement>(null);
  const openFeedback = useRef<HTMLParagraphElement>(null);
  const focusAfter = useRef(false);
  const generation = useRef(0);
  function validate(run: ReplyRun) {
    if (
      !run ||
      typeof run.id !== "string" ||
      !run.id ||
      run.kind !== "conversation" ||
      run.scope?.kind !== scope.kind ||
      run.scope.id !== scope.id ||
      run.owner?.type !== "team" ||
      run.owner.id !== teamId ||
      run.context?.conversationId !== id ||
      typeof run.context.triggerMessageId !== "string" ||
      !run.context.triggerMessageId ||
      !Number.isSafeInteger(run.context.triggerSequence) ||
      run.context.triggerSequence < 1 ||
      !Number.isSafeInteger(run.revision) ||
      run.revision < 1 ||
      !Object.hasOwn(states, run.status) ||
      typeof run.goal !== "string" ||
      !Number.isFinite(Date.parse(run.createdAt))
    )
      throw new Error(
        "The returned reply activity does not match this channel.",
      );
  }
  useEffect(() => {
    let canceled = false,
      inflight = false;
    async function load() {
      if (inflight) return;
      inflight = true;
      try {
        const result = await api<ReplyRun[] | null>(
          scoped(
            `/conversations/${encodeURIComponent(id)}/runs?limit=${pageSize + 1}&offset=${offset}`,
          ),
        );
        if (canceled) return;
        if (result !== null && !Array.isArray(result))
          throw new Error("Unreadable reply activity.");
        const rows = result || [];
        rows.forEach(validate);
        if (new Set(rows.map((run) => run.id)).size !== rows.length)
          throw new Error("Reply activity contains duplicate attempts.");
        setItems(rows.slice(0, pageSize));
        setMore(rows.length > pageSize);
        setLoaded(true);
        setError("");
      } catch (e) {
        if (!canceled)
          setError(
            `Could not refresh reply activity. ${message(e)} Saved activity may be out of date.`,
          );
      } finally {
        inflight = false;
        if (!canceled) {
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
    // Keep the latest attempt useful while collapsed. Freeze older pages until
    // an explicit refresh so new attempts do not shift the user's reading place.
    const timer =
      offset === 0 ? setInterval(() => void load(), 5000) : undefined;
    return () => {
      canceled = true;
      clearInterval(timer);
    };
  }, [id, teamId, offset, refresh]);
  useEffect(
    () => () => {
      generation.current++;
    },
    [id, teamId],
  );
  useEffect(() => {
    if (openError) openFeedback.current?.focus();
  }, [openError]);
  function page(next: number) {
    setItems([]);
    setLoaded(false);
    setMore(false);
    setError("");
    setBusy(true);
    focusAfter.current = true;
    setOffset(next);
  }
  async function inspect(run: ReplyRun) {
    if (opening) return;
    const request = ++generation.current;
    setOpening(run.id);
    setOpenError("");
    try {
      const current = await api<ReplyRun>(
        scoped(`/agent-runs/${encodeURIComponent(run.id)}`),
      );
      if (request !== generation.current) return;
      validate(current);
      if (
        current.id !== run.id ||
        current.revision < run.revision ||
        current.context.triggerMessageId !== run.context.triggerMessageId
      )
        throw new Error("The returned work does not match this reply attempt.");
      onOpenRun(current);
    } catch (e) {
      if (request === generation.current)
        setOpenError(
          `Could not open reply details. ${message(e)} Try opening the attempt again.`,
        );
    } finally {
      if (request === generation.current) setOpening("");
    }
  }
  return (
    <section className="channel-availability" aria-label="Reply activity">
      <button
        className="button"
        aria-expanded={open}
        aria-controls={panelId}
        onClick={() => {
          setOpen(!open);
          setOpenError("");
          generation.current++;
          setOpening("");
          if (open && offset) page(0);
        }}
      >
        {open ? "Hide reply activity" : "Show reply activity"}
      </button>
      <p
        ref={feedback}
        tabIndex={-1}
        role={error ? "alert" : "status"}
        className={error ? "error-text" : "muted"}
      >
        {error ||
          (!loaded
            ? "Loading reply activity…"
            : open
              ? items.length
                ? `Reply attempts ${offset + 1}–${offset + items.length}. Newest first.`
                : "No reply attempts on this page."
              : items[0]
                ? `Latest attempt: ${outcome(items[0])}.`
                : "No reply attempts recorded for this channel.")}
      </p>
      {error && (
        <button
          className="button"
          disabled={busy}
          onClick={() => {
            focusAfter.current = true;
            setRefresh((n) => n + 1);
          }}
        >
          Retry reply activity
        </button>
      )}
      {open && (
        <div id={panelId}>
          <h4>Reply activity</h4>
          <p className="muted">
            Each attempt follows one message. A completed review may offer no
            reply. Open details to inspect progress, usage, or available work
            controls.
          </p>
          <button
            className="button"
            disabled={busy}
            onClick={() => {
              focusAfter.current = true;
              setRefresh((n) => n + 1);
            }}
          >
            Refresh reply activity
          </button>
          {loaded && !items.length && !offset && (
            <p className="muted">
              Attempts appear after a new message is scheduled for team replies.
              Enabling replies does not replay earlier messages.
            </p>
          )}
          {!!items.length && (
            <ol className="work-history-list" aria-label="Reply attempts">
              {items.map((run) => (
                <li key={run.id}>
                  <p>
                    <strong>Message {run.context.triggerSequence}</strong> ·{" "}
                    <time dateTime={run.createdAt}>
                      {new Date(run.createdAt).toLocaleString()}
                    </time>
                  </p>
                  <p>{outcome(run)}</p>
                  {run.output?.degraded === true && (
                    <p className="muted">
                      Some team members were unavailable for this attempt.
                    </p>
                  )}
                  <ChannelMessageContext
                    conversationId={id}
                    messageId={run.context.triggerMessageId}
                    label="Triggering message"
                    sender={sender}
                  />
                  {canInspect && (
                    <div className="inspector-actions">
                      <button
                        className="button"
                        aria-disabled={!!opening}
                        onClick={(event) => {
                          event.currentTarget.focus();
                          void inspect(run);
                        }}
                      >
                        {opening === run.id
                          ? "Opening reply details…"
                          : "Open reply details"}
                      </button>
                    </div>
                  )}
                </li>
              ))}
            </ol>
          )}
          {openError && (
            <p
              className="error-text"
              ref={openFeedback}
              tabIndex={-1}
              role="alert"
            >
              {openError}
            </p>
          )}
          <div
            className="inspector-actions"
            role="group"
            aria-label="Reply activity pages"
          >
            <button
              className="button"
              disabled={busy || offset === 0}
              onClick={() => page(Math.max(0, offset - pageSize))}
            >
              Newer attempts
            </button>
            <button
              className="button"
              disabled={busy || !more}
              onClick={() => page(offset + pageSize)}
            >
              Older attempts
            </button>
          </div>
        </div>
      )}
    </section>
  );
}
