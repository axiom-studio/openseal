import { useEffect, useRef, useState } from "react";
import { ChevronRight, ShieldCheck } from "lucide-react";
import { api, message, scoped, type Run } from "./api";

type Review = {
  id: string;
  runId: string;
  summary: string;
  status: string;
  risk: string;
  expiresAt: string;
  decisionBy?: { id: string };
  decisionReason?: string;
};
const size = 25;
export default function ReviewInbox({
  available,
  canInspect,
  refreshToken,
  onOpen,
  selectedId,
}: {
  available: boolean;
  canInspect: boolean;
  refreshToken: number;
  onOpen: (run: Run, approvalId: string, trigger: HTMLElement | null) => void;
  selectedId?: string;
}) {
  const [status, setStatus] = useState("pending");
  const [offset, setOffset] = useState(0);
  const [items, setItems] = useState<Review[]>([]);
  const [more, setMore] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [openError, setOpenError] = useState("");
  const [opening, setOpening] = useState("");
  const [loadedView, setLoadedView] = useState("");
  const [retry, setRetry] = useState(0);
  const view = `${status}:${offset}`;
  const epoch = useRef(0);
  const openingEpoch = useRef(0);
  const focusPage = useRef(false);
  const feedback = useRef<HTMLParagraphElement>(null);
  useEffect(
    () => () => {
      epoch.current++;
      openingEpoch.current++;
    },
    [],
  );
  useEffect(() => {
    if (!available) return;
    const request = ++epoch.current;
    setBusy(true);
    setError("");
    void api<Review[] | null>(
      scoped(
        `/action-approvals?status=${status}&limit=${size + 1}&offset=${offset}&order=${status === "pending" ? "created_asc" : "created_desc"}`,
      ),
    )
      .then((result) => {
        if (request !== epoch.current) return;
        if (result !== null && !Array.isArray(result))
          throw new Error("OpenSeal returned an unreadable review list.");
        setItems((result || []).slice(0, size));
        setMore((result || []).length > size);
        setLoadedView(view);
      })
      .catch((e) => {
        if (request === epoch.current)
          setError(`Could not load reviews. ${message(e)}`);
      })
      .finally(() => {
        if (request === epoch.current) {
          setBusy(false);
          if (focusPage.current) {
            focusPage.current = false;
            requestAnimationFrame(() => feedback.current?.focus());
          }
        }
      });
    return () => {
      epoch.current++;
    };
  }, [status, offset, refreshToken, retry, available]);
  async function open(review: Review) {
    const trigger = document.activeElement as HTMLElement | null;
    const request = ++openingEpoch.current;
    setOpening(review.id);
    setOpenError("");
    try {
      const run = await api<Run>(
        scoped(`/agent-runs/${encodeURIComponent(review.runId)}`),
      );
      if (request !== openingEpoch.current) return;
      if (run.id !== review.runId)
        throw new Error("The returned task does not match this review.");
      onOpen(run, review.id, trigger);
    } catch (e) {
      if (request === openingEpoch.current) {
        setOpenError(
          `Could not open this review. ${message(e)} Select it again to retry.`,
        );
        requestAnimationFrame(() => feedback.current?.focus());
      }
    } finally {
      if (request === openingEpoch.current) setOpening("");
    }
  }
  function changePage(next: number) {
    openingEpoch.current++;
    setOpening("");
    setOpenError("");
    focusPage.current = true;
    setOffset(next);
  }
  if (!available)
    return (
      <p className="inline-help">
        Action reviews are unavailable in this workspace.
      </p>
    );
  const visible = loadedView === view && !error;
  return (
    <>
      <div className="list-toolbar">
        <label className="filter-select">
          <span className="sr-only">Review status</span>
          <select
            value={status}
            onChange={(e) => {
              openingEpoch.current++;
              setOpening("");
              setOpenError("");
              setStatus(e.target.value);
              setOffset(0);
            }}
          >
            <option value="pending">Needs review</option>
            <option value="approved">Approved</option>
            <option value="changes_requested">Changes requested</option>
            <option value="rejected">Rejected</option>
            <option value="expired">Expired</option>
            <option value="canceled">Canceled</option>
          </select>
        </label>
      </div>
      <p className="inline-help">
        {status === "pending"
          ? "Pending action checkpoints, oldest first. Open one to inspect its effects and eligible reviewers."
          : "Recorded action decisions, newest requests first. Open one to inspect its saved details."}
      </p>
      {!canInspect && (
        <p className="inline-help">
          This workspace lists reviews but does not provide the task and action
          details needed to open them.
        </p>
      )}
      {error ? (
        <div>
          <p role="alert" className="error-text">
            {error}
          </p>
          <button
            className="button"
            onClick={() => {
              focusPage.current = true;
              setRetry((n) => n + 1);
            }}
          >
            Try again
          </button>
        </div>
      ) : !visible ? (
        <div
          className="skeleton-list"
          role="group"
          aria-label="Loading reviews"
          aria-busy="true"
        >
          <span />
          <span />
          <span />
        </div>
      ) : items.length ? (
        <div className="record-list">
          {items.map((review) => {
            const deadline = new Date(review.expiresAt);
            return (
              <button
                key={review.id}
                className={`record-row ${selectedId === review.id ? "selected" : ""}`}
                disabled={!canInspect || !!opening}
                onClick={() => void open(review)}
              >
                <span className="run-icon">
                  <ShieldCheck size={20} />
                </span>
                <span className="record-main">
                  <strong>{review.summary}</strong>
                  <span>
                    {review.risk} risk
                    {status === "pending" && Number.isFinite(deadline.getTime())
                      ? ` · ${deadline.getTime() <= Date.now() ? "Deadline passed" : "Review by"} ${deadline.toLocaleString()}`
                      : review.decisionBy
                        ? ` · Reviewed by ${review.decisionBy.id}`
                        : ""}
                  </span>
                </span>
                {opening === review.id ? (
                  <span>Opening…</span>
                ) : (
                  <ChevronRight size={16} aria-hidden="true" />
                )}
              </button>
            );
          })}
        </div>
      ) : (
        <div className="empty">
          <ShieldCheck size={27} strokeWidth={1.5} aria-hidden="true" />
          <h3>
            {offset
              ? "No reviews on this page"
              : status === "pending"
                ? "No actions waiting for review"
                : "No reviews in this view"}
          </h3>
          <p>
            {offset
              ? "Reviews may have moved as decisions were recorded. Return to the first page."
              : status === "pending"
                ? "Actions that need a decision will appear here. Agent installation proposals remain in Home."
                : "Choose another status to explore recorded decisions."}
          </p>
          {offset > 0 && (
            <button className="button" onClick={() => changePage(0)}>
              Back to first page
            </button>
          )}
        </div>
      )}
      <div className="inspector-actions" role="group" aria-label="Review pages">
        <button
          className="button"
          disabled={busy || offset === 0}
          onClick={() => changePage(Math.max(0, offset - size))}
        >
          Previous page
        </button>
        <button
          className="button"
          disabled={busy || !more}
          onClick={() => changePage(offset + size)}
        >
          Next page
        </button>
      </div>
      <p className="muted" role="status" ref={feedback} tabIndex={-1}>
        {openError ||
          (busy && !visible
            ? "Loading reviews…"
            : error
              ? "Reviews could not be loaded."
              : items.length
                ? `Showing ${offset + 1}–${offset + items.length}${more ? ". More reviews are available." : "."}`
                : "No results.")}
      </p>
    </>
  );
}
