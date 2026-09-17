import { useEffect, useRef, useState } from "react";
import { api, message, scoped, type Run } from "./api";

type Activity = {
  id: string;
  eventType: string;
  summary: string;
  severity: string;
  createdAt: string;
};
type Page = { items: Activity[]; hasMore: boolean; nextCursor?: string };
export default function WorkHistory({ run }: { run: Run }) {
  const [open, setOpen] = useState(false);
  const [items, setItems] = useState<Activity[]>([]);
  const [cursor, setCursor] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [loadedRevision, setLoadedRevision] = useState(0);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const feedback = useRef<HTMLParagraphElement>(null);
  const epoch = useRef(0);
  const readingOlder = useRef(false);
  useEffect(
    () => () => {
      epoch.current++;
    },
    [],
  );
  async function load(older = false, manual = false) {
    const request = ++epoch.current;
    const revision = run.revision;
    const next = older ? cursor : undefined;
    readingOlder.current = older;
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const page = await api<Page>(
        scoped(
          `/activity?runId=${encodeURIComponent(run.id)}&limit=20${next ? `&cursor=${encodeURIComponent(next)}` : ""}`,
        ),
      );
      if (request !== epoch.current) return;
      if (
        !Array.isArray(page.items) ||
        (page.hasMore && (!page.nextCursor || page.nextCursor === next))
      )
        throw new Error("OpenSeal returned an unreadable activity page.");
      setItems((current) =>
        older
          ? [
              ...current,
              ...page.items.filter(
                (event) =>
                  !current.some((existing) => existing.id === event.id),
              ),
            ]
          : page.items,
      );
      setCursor(page.hasMore ? page.nextCursor : undefined);
      setLoaded(true);
      if (!older) setLoadedRevision(revision);
      if (manual)
        setNotice(
          older
            ? page.items.length
              ? "Earlier activity loaded."
              : "No earlier activity."
            : "History refreshed.",
        );
    } catch (e) {
      if (request === epoch.current)
        setError(`Could not load activity. ${message(e)}`);
    } finally {
      if (request === epoch.current) {
        setBusy(false);
        if (manual) requestAnimationFrame(() => feedback.current?.focus());
      }
    }
  }
  useEffect(() => {
    if (!open) {
      epoch.current++;
      setBusy(false);
      return;
    }
    if (!readingOlder.current) void load();
  }, [open, run.revision]);
  return (
    <details
      className="work-history"
      onToggle={(event) => setOpen(event.currentTarget.open)}
    >
      <summary>Activity history</summary>
      {open && (
        <>
          <p className="muted">Newest first. Recorded events from this run.</p>
          {readingOlder.current && run.revision > loadedRevision && (
            <p className="muted">
              The run changed. Refresh to see its latest activity.
            </p>
          )}
          <div className="inspector-actions">
            <button
              className="button"
              disabled={busy}
              onClick={() => void load(false, true)}
            >
              {busy && !readingOlder.current
                ? "Refreshing…"
                : "Refresh history"}
            </button>
          </div>
          {!loaded && busy && (
            <p role="status" className="muted">
              Loading activity…
            </p>
          )}
          {loaded && !items.length && (
            <p className="muted">
              No activity has been recorded for this run yet.
            </p>
          )}
          {!!items.length && (
            <ol aria-label="Recorded activity" className="work-history-list">
              {items.map((event) => {
                const date = new Date(event.createdAt);
                return (
                  <li key={event.id}>
                    <p
                      className={
                        event.severity === "error" ? "error-text" : undefined
                      }
                    >
                      {event.summary}
                    </p>
                    <span className="muted">
                      {event.eventType.replace(/[._]/g, " ")}
                      {Number.isFinite(date.getTime()) && (
                        <>
                          {" "}
                          ·{" "}
                          <time dateTime={date.toISOString()}>
                            {date.toLocaleString()}
                          </time>
                        </>
                      )}
                    </span>
                  </li>
                );
              })}
            </ol>
          )}
          {cursor && (
            <button
              className="button"
              disabled={busy}
              onClick={() => void load(true, true)}
            >
              {busy && readingOlder.current
                ? "Loading earlier activity…"
                : "Load earlier activity"}
            </button>
          )}
          <p
            ref={feedback}
            tabIndex={-1}
            role={error ? "alert" : "status"}
            className={error ? "error-text" : "muted"}
          >
            {error || notice}
          </p>
        </>
      )}
    </details>
  );
}
