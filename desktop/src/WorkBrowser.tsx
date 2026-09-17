import { useEffect, useRef, useState, type ReactNode } from "react";
import { Search, Layers3 } from "lucide-react";
import { api, message, scoped, type Run } from "./api";

const pageSize = 25;
const activeStatuses =
  "queued,planning,running,paused,sleeping,waiting_for_dependency,waiting_for_agent,waiting_for_approval,waiting_for_event";
export default function WorkBrowser({
  refreshToken,
  available,
  renderRows,
  onResults,
  onStart,
}: {
  refreshToken: number;
  available: boolean;
  renderRows: (items: Run[]) => ReactNode;
  onResults: (items: Run[]) => void;
  onStart: () => void;
}) {
  const [query, setQuery] = useState("");
  const [status, setStatus] = useState("all");
  const [offset, setOffset] = useState(0);
  const [loadedView, setLoadedView] = useState("");
  const view = JSON.stringify([query, status, offset]);
  const [items, setItems] = useState<Run[]>([]);
  const [more, setMore] = useState(false);
  const [busy, setBusy] = useState(true);
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);
  const focusAfterPage = useRef(false);
  const feedback = useRef<HTMLParagraphElement>(null);
  useEffect(() => {
    if (!available) {
      setBusy(false);
      return;
    }
    let canceled = false;
    setBusy(true);
    setError("");
    const timer = setTimeout(async () => {
      try {
        const params = new URLSearchParams({
          limit: String(pageSize + 1),
          offset: String(offset),
          order: "created_desc",
        });
        if (query.trim()) params.set("q", query.trim());
        if (status !== "all")
          params.set("status", status === "active" ? activeStatuses : status);
        const result = await api<Run[] | null>(scoped(`/agent-runs?${params}`));
        if (canceled) return;
        if (result !== null && !Array.isArray(result))
          throw new Error("OpenSeal returned an unreadable work list.");
        const page = result || [];
        setLoadedView(JSON.stringify([query, status, offset]));
        setItems(page.slice(0, pageSize));
        setMore(page.length > pageSize);
        onResults(page.slice(0, pageSize));
      } catch (e) {
        if (!canceled) {
          setError(message(e));
          setItems([]);
          setMore(false);
        }
      } finally {
        if (!canceled) {
          setBusy(false);
          if (focusAfterPage.current) {
            focusAfterPage.current = false;
            requestAnimationFrame(() => feedback.current?.focus());
          }
        }
      }
    }, 250);
    return () => {
      canceled = true;
      clearTimeout(timer);
    };
  }, [query, status, offset, refreshToken, retry, available, onResults]);
  function reset() {
    setQuery("");
    setStatus("all");
    setOffset(0);
  }
  function changePage(next: number) {
    focusAfterPage.current = true;
    setOffset(next);
  }
  if (!available)
    return (
      <p className="inline-help">
        Work browsing is unavailable in this workspace.
      </p>
    );
  return (
    <>
      <div className="list-toolbar">
        <label className="filter-search">
          <Search size={16} />
          <span className="sr-only">Search work</span>
          <input
            type="search"
            maxLength={250}
            placeholder="Search all work…"
            value={query}
            onChange={(e) => {
              setQuery(e.target.value);
              setOffset(0);
            }}
          />
        </label>
        <label className="filter-select">
          <span className="sr-only">Work status</span>
          <select
            value={status}
            onChange={(e) => {
              setStatus(e.target.value);
              setOffset(0);
            }}
          >
            <option value="all">All work</option>
            <option value="active">In progress</option>
            <option value="completed">Completed</option>
            <option value="failed">Failed</option>
            <option value="paused">Paused</option>
            <option value="canceled">Canceled</option>
          </select>
        </label>
      </div>
      <p className="inline-help">
        Search task descriptions and statuses across this workspace. Newest
        first.
      </p>
      {error ? (
        <div>
          <p className="error-text" role="alert">
            Could not load work. {error}
          </p>
          <button
            className="button"
            onClick={() => {
              focusAfterPage.current = true;
              setRetry((n) => n + 1);
            }}
          >
            Try again
          </button>
        </div>
      ) : loadedView !== view ? (
        <div
          className="skeleton-list"
          role="group"
          aria-label="Loading work"
          aria-busy="true"
        >
          <span />
          <span />
          <span />
        </div>
      ) : items.length ? (
        renderRows(items)
      ) : (
        <div className="empty">
          <Layers3 size={27} strokeWidth={1.5} aria-hidden="true" />
          <h3>
            {query || status !== "all"
              ? "No work matches this view"
              : offset
                ? "No more work on this page"
                : "Good work starts with a clear goal"}
          </h3>
          <p>
            {query || status !== "all"
              ? "Try another search or show all work."
              : offset
                ? "Return to a previous page to continue browsing."
                : "Give an agent a task and return here to follow its progress."}
          </p>
          <button
            className="button"
            onClick={
              query || status !== "all"
                ? reset
                : offset
                  ? () => changePage(0)
                  : onStart
            }
          >
            {query || status !== "all"
              ? "Clear filters"
              : offset
                ? "Back to newest work"
                : "Start work"}
          </button>
        </div>
      )}
      <div className="inspector-actions" role="group" aria-label="Work pages">
        <button
          className="button"
          disabled={busy || offset === 0}
          onClick={() => changePage(Math.max(0, offset - pageSize))}
        >
          Previous page
        </button>
        <button
          className="button"
          disabled={busy || !more}
          onClick={() => changePage(offset + pageSize)}
        >
          Next page
        </button>
      </div>
      <p className="muted" ref={feedback} tabIndex={-1} role="status">
        {loadedView !== view && !error
          ? "Loading work…"
          : error
            ? "Work could not be loaded."
            : items.length
              ? `Showing ${offset + 1}–${offset + items.length}${more ? ". More work is available." : "."}`
              : "No results."}
      </p>
    </>
  );
}
