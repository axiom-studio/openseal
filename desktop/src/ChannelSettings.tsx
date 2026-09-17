import { useEffect, useId, useRef, useState } from "react";
import { api, ApiError, message, scope, scoped } from "./api";
import type { Conversation } from "./TeamChannels";
type Change = { expectedRevision: number; title?: string; status?: string };
type Draft = { title: string; baseTitle: string; pending?: Change };
function restore(key: string, current: Conversation): Draft {
  try {
    const value = JSON.parse(localStorage.getItem(key) || "null");
    if (
      value &&
      typeof value.title === "string" &&
      value.title.length <= 240 &&
      typeof value.baseTitle === "string" &&
      (!value.pending ||
        (Number.isSafeInteger(value.pending.expectedRevision) &&
          (typeof value.pending.title === "string" ||
            ["active", "archived"].includes(value.pending.status))))
    )
      return value;
  } catch {
    /* Saving will explain storage failures. */
  }
  return { title: current.title, baseTitle: current.title };
}
export default function ChannelSettings({
  conversation,
  onChange,
}: {
  conversation: Conversation;
  onChange: (value: Conversation) => void;
}) {
  const key = `openseal.channel-settings.${conversation.id}`;
  const [draft, setDraft] = useState(() => restore(key, conversation));
  const [open, setOpen] = useState(!!draft.pending);
  const [confirm, setConfirm] = useState<{
    revision: number;
    status: string;
  } | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [storageError, setStorageError] = useState(false);
  const lock = useRef(false),
    alive = useRef(true);
  const feedback = useRef<HTMLParagraphElement>(null),
    confirmButton = useRef<HTMLButtonElement>(null);
  const nameField = useRef<HTMLInputElement>(null);
  const availabilityButton = useRef<HTMLButtonElement>(null);
  const returnFocus = useRef(false);
  const fieldId = useId();
  const stale =
    draft.baseTitle !== conversation.title && draft.title !== draft.baseTitle;
  const tooLong = new TextEncoder().encode(draft.title.trim()).length > 240;
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);
  useEffect(() => {
    if (open && !draft.pending) nameField.current?.focus();
  }, [open]);
  useEffect(() => {
    if (confirm) confirmButton.current?.focus();
    else if (returnFocus.current) {
      returnFocus.current = false;
      availabilityButton.current?.focus();
    }
  }, [confirm]);
  useEffect(() => {
    if (error || notice) feedback.current?.focus();
  }, [error, notice]);
  function persist(next: Draft) {
    try {
      next.pending || next.title !== next.baseTitle
        ? localStorage.setItem(key, JSON.stringify(next))
        : localStorage.removeItem(key);
      setStorageError(false);
      return true;
    } catch {
      setStorageError(true);
      return false;
    }
  }
  useEffect(() => {
    if (
      !draft.pending &&
      draft.title === draft.baseTitle &&
      draft.baseTitle !== conversation.title
    ) {
      const next = { title: conversation.title, baseTitle: conversation.title };
      setDraft(next);
      persist(next);
    }
    if (
      confirm &&
      !lock.current &&
      confirm.revision !== conversation.revision
    ) {
      setConfirm(null);
      setError(
        "This channel changed. Review its latest state before changing availability.",
      );
    }
  }, [conversation.revision, conversation.title]);
  function validate(value: Conversation) {
    if (
      value.id !== conversation.id ||
      value.owner?.type !== conversation.owner.type ||
      value.owner.id !== conversation.owner.id ||
      value.revision < conversation.revision
    )
      throw new Error(
        "The returned channel does not match the current conversation.",
      );
  }
  function accept(value: Conversation, pending?: Change) {
    const title =
      pending?.title === value.title || draft.title === draft.baseTitle
        ? value.title
        : draft.title;
    const next = { title, baseTitle: value.title };
    if (persist(next)) setDraft(next);
    else
      setError(
        "The channel state is known, but local recovery could not be cleared. Check saved channel again when storage is available.",
      );
    setConfirm(null);
    onChange(value);
  }
  async function check() {
    if (lock.current) return;
    lock.current = true;
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const current = await api<Conversation>(
        scoped(`/conversations/${encodeURIComponent(conversation.id)}`),
      );
      if (!alive.current) return;
      validate(current);
      const pending = draft.pending;
      const matches =
        pending &&
        (pending.title === undefined || pending.title === current.title) &&
        (pending.status === undefined || pending.status === current.status);
      accept(current, pending);
      setNotice(
        matches
          ? "The requested channel state is saved. No change was sent again."
          : "Latest channel loaded. Your name draft is kept; review it before saving. No change was sent.",
      );
    } catch (e) {
      if (alive.current)
        setError(
          `Could not check the saved channel. ${message(e)} Your draft and pending change are kept.`,
        );
    } finally {
      lock.current = false;
      if (alive.current) setBusy(false);
    }
  }
  async function change(changes: Omit<Change, "expectedRevision">) {
    if (lock.current || draft.pending || stale) return;
    const pending = { ...changes, expectedRevision: conversation.revision };
    const next = { ...draft, pending };
    if (!persist(next)) {
      setError(
        "Recovery storage is unavailable. Nothing was changed. Enable local storage and try again.",
      );
      return;
    }
    lock.current = true;
    setDraft(next);
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const current = await api<Conversation>(
        `/conversations/${encodeURIComponent(conversation.id)}`,
        { method: "PATCH", body: { scope, ...pending } },
      );
      if (!alive.current) return;
      validate(current);
      if (
        (pending.title !== undefined && current.title !== pending.title) ||
        (pending.status !== undefined && current.status !== pending.status)
      )
        throw new Error("Could not confirm the requested channel state.");
      accept(current, pending);
      setNotice(
        pending.title !== undefined
          ? "Channel renamed."
          : pending.status === "archived"
            ? "Channel archived. Its messages and your drafts are kept."
            : "Channel restored. You can post messages again.",
      );
    } catch (e) {
      if (!alive.current) return;
      if (
        e instanceof ApiError &&
        [400, 401, 403, 404, 422].includes(e.status)
      ) {
        const retained = { title: draft.title, baseTitle: draft.baseTitle };
        persist(retained);
        setDraft(retained);
        setConfirm(null);
        setError(`Not changed. ${message(e)} Your name draft is kept.`);
      } else
        setError(
          e instanceof ApiError && e.status === 409
            ? "This channel changed before your edit was saved. Check the saved channel, then review your change."
            : `Could not confirm the change. ${message(e)} Check the saved channel before trying again.`,
        );
    } finally {
      lock.current = false;
      if (alive.current) setBusy(false);
    }
  }
  return (
    <section className="channel-settings" aria-label="Channel settings">
      <button
        className="button"
        aria-expanded={open}
        onClick={() => {
          setOpen((v) => !v);
          setConfirm(null);
        }}
      >
        {open ? "Hide channel settings" : "Channel settings"}
      </button>
      {open && (
        <>
          <h4>Channel settings</h4>
          <form
            className="channel-composer"
            onSubmit={(e) => {
              e.preventDefault();
              if (
                !tooLong &&
                draft.title.trim() &&
                draft.title.trim() !== conversation.title
              )
                void change({ title: draft.title.trim() });
            }}
          >
            <label htmlFor={fieldId}>Channel name</label>
            <input
              ref={nameField}
              id={fieldId}
              value={draft.title}
              maxLength={240}
              disabled={busy || !!draft.pending}
              onChange={(e) => {
                const next = { ...draft, title: e.target.value };
                setDraft(next);
                persist(next);
                setNotice("");
              }}
            />
            {tooLong && (
              <p className="error-text">
                This name is too long. Try a shorter name.
              </p>
            )}
            {stale && (
              <p className="inline-help">
                The saved name is now “{conversation.title}”. Review the latest
                channel before replacing it.
              </p>
            )}
            <button
              className="button"
              disabled={
                busy ||
                !!draft.pending ||
                stale ||
                tooLong ||
                !draft.title.trim() ||
                draft.title.trim() === conversation.title
              }
            >
              Save channel name
            </button>
          </form>
          <div className="channel-availability">
            <h4>
              {conversation.status === "archived"
                ? "Restore this channel"
                : "Archive this channel"}
            </h4>
            <p className="muted">
              {conversation.status === "archived"
                ? "Restore posting while keeping the same messages and channel identity."
                : "Stop new messages while keeping the conversation and your drafts. This does not cancel team tasks."}
            </p>
            {!confirm ? (
              <button
                className="button"
                ref={availabilityButton}
                disabled={busy || !!draft.pending || stale}
                onClick={() => {
                  setError("");
                  setNotice("");
                  setConfirm({
                    revision: conversation.revision,
                    status:
                      conversation.status === "archived"
                        ? "active"
                        : "archived",
                  });
                }}
              >
                {conversation.status === "archived"
                  ? "Restore channel"
                  : "Archive channel"}
              </button>
            ) : (
              <div className="inspector-actions">
                <button
                  ref={confirmButton}
                  className="button"
                  disabled={busy || !!draft.pending}
                  onClick={() => void change({ status: confirm.status })}
                >
                  {confirm.status === "archived"
                    ? "Confirm archive"
                    : "Confirm restore"}
                </button>
                <button
                  className="button"
                  disabled={busy}
                  onClick={() => {
                    returnFocus.current = true;
                    setConfirm(null);
                  }}
                >
                  Keep current state
                </button>
              </div>
            )}
          </div>
          {(draft.pending || stale) && (
            <button
              className="button"
              disabled={busy}
              onClick={() => void check()}
            >
              {busy ? "Checking…" : "Check saved channel"}
            </button>
          )}
          {storageError && (
            <p role="alert" className="error-text">
              Channel settings could not be saved on this computer. Keep this
              view open until storage is available.
            </p>
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
    </section>
  );
}
