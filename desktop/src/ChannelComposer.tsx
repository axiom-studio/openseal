import { useEffect, useId, useRef, useState } from "react";
import { api, ApiError, message } from "./api";

type Pending = { key: string; body: Record<string, unknown> };
type Draft = { text: string; pending?: Pending };
function restore(key: string): Draft {
  try {
    const value = JSON.parse(localStorage.getItem(key) || "null");
    if (
      value &&
      typeof value.text === "string" &&
      value.text.length <= 16000 &&
      (!value.pending ||
        (typeof value.pending.key === "string" &&
          value.pending.body &&
          typeof value.pending.body === "object"))
    )
      return value;
  } catch {
    /* The editor explains storage failures before sending. */
  }
  return { text: "" };
}
export default function ChannelComposer({
  storageKey,
  path,
  label,
  action,
  enabled,
  titleOnly = false,
  focusOnMount = false,
  context,
  body,
  committed,
  rejected,
}: {
  storageKey: string;
  path: string;
  label: string;
  action: string;
  enabled: boolean;
  titleOnly?: boolean;
  focusOnMount?: boolean;
  context?: string;
  body: (text: string) => Record<string, unknown>;
  committed: (result: any) => void;
  rejected: () => void;
}) {
  const [draft, setDraft] = useState(() => restore(storageKey));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [storageError, setStorageError] = useState(false);
  const [notice, setNotice] = useState("");
  const lock = useRef(false),
    alive = useRef(true);
  const field = useRef<HTMLInputElement | HTMLTextAreaElement | null>(null);
  useEffect(() => {
    if (focusOnMount) field.current?.focus();
  }, [focusOnMount]);
  const feedback = useRef<HTMLParagraphElement>(null);
  const id = useId();
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);
  function save(next: Draft) {
    try {
      next.text || next.pending
        ? localStorage.setItem(storageKey, JSON.stringify(next))
        : localStorage.removeItem(storageKey);
      setStorageError(false);
      return true;
    } catch {
      setStorageError(true);
      return false;
    }
  }
  function edit(text: string) {
    const next = { text };
    setDraft(next);
    save(next);
    setNotice("");
  }
  async function submit() {
    if (lock.current || (!enabled && !draft.pending) || !draft.text.trim())
      return;
    const pending = draft.pending || {
      key: crypto.randomUUID(),
      body: body(draft.text.trim()),
    };
    if (!save({ ...draft, pending })) {
      setError(
        "Draft recovery is unavailable. Nothing was sent. Enable local storage and try again.",
      );
      return;
    }
    lock.current = true;
    setDraft({ ...draft, pending });
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const result = await api<any>(path, {
        method: "POST",
        key: pending.key,
        body: pending.body,
      });
      if (!alive.current) return;
      committed(result);
      if (save({ text: "" })) {
        setDraft({ text: "" });
        setNotice(`${titleOnly ? "Channel created" : "Message posted"}.`);
      } else
        setNotice(
          "Saved to the workspace. Local recovery could not be cleared; retrying will check the same saved operation.",
        );
    } catch (e) {
      if (!alive.current) return;
      const definitive =
        e instanceof ApiError &&
        [400, 401, 403, 404, 409, 422].includes(e.status);
      if (definitive) {
        const next = { text: draft.text };
        save(next);
        setDraft(next);
        rejected();
        setError(
          e instanceof ApiError && e.status === 409
            ? "This channel changed. Your draft is kept. Review the refreshed conversation before posting again."
            : `Not saved. ${message(e)} Your draft is kept.`,
        );
      } else
        setError(
          `Could not confirm whether it was saved. ${message(e)} Retry to check the same operation without posting twice.`,
        );
    } finally {
      lock.current = false;
      if (alive.current) {
        setBusy(false);
        requestAnimationFrame(() => feedback.current?.focus());
      }
    }
  }
  return (
    <form
      className="channel-composer"
      onSubmit={(e) => {
        e.preventDefault();
        void submit();
      }}
    >
      <label htmlFor={id}>{label}</label>
      {context && <p className="inline-help">{context}</p>}
      {titleOnly ? (
        <input
          id={id}
          ref={(el) => {
            field.current = el;
          }}
          maxLength={60}
          value={draft.text}
          disabled={busy || !!draft.pending}
          onChange={(e) => edit(e.target.value)}
        />
      ) : (
        <textarea
          id={id}
          ref={(el) => {
            field.current = el;
          }}
          rows={3}
          maxLength={16000}
          value={draft.text}
          disabled={busy || !!draft.pending}
          onChange={(e) => edit(e.target.value)}
        />
      )}
      {storageError && (
        <p role="alert" className="error-text">
          This draft could not be saved on this computer. Keep this view open
          until storage is available.
        </p>
      )}
      {draft.pending && (
        <p className="inline-help">
          A saved attempt is awaiting confirmation. Its contents are kept
          unchanged for retry.
        </p>
      )}
      <button
        className="button primary"
        disabled={busy || (!enabled && !draft.pending) || !draft.text.trim()}
      >
        {busy
          ? "Saving…"
          : draft.pending
            ? `Retry ${titleOnly ? "creating channel" : "posting message"}`
            : action}
      </button>
      <p
        ref={feedback}
        tabIndex={-1}
        role={error ? "alert" : "status"}
        className={error ? "error-text" : "muted"}
      >
        {error || notice}
      </p>
    </form>
  );
}
