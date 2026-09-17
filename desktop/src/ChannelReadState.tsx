import { useEffect, useRef, useState } from "react";
import { api, ApiError, message, scope, scoped } from "./api";

type Cursor = {
  scope: { kind: string; id: string };
  conversationId: string;
  participant: { type: string; id: string };
  deliveredSequence: number;
  readSequence: number;
  revision: number;
};
const participant = { type: "user", id: "local-operator" };
export default function ChannelReadState({
  id,
  lastSequence,
  shownThrough,
  ready,
  onUnread,
  onRead,
}: {
  id: string;
  lastSequence: number;
  shownThrough: number;
  ready: boolean;
  onUnread: (sequence: number) => void;
  onRead: (sequence: number, revision: number) => void;
}) {
  const [cursor, setCursor] = useState<Cursor | null>(null);
  const [loaded, setLoaded] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [check, setCheck] = useState(false);
  const alive = useRef(true),
    lock = useRef(false),
    known = useRef<Cursor | null>(null);
  const feedback = useRef<HTMLParagraphElement>(null);
  const [feedbackRevision, setFeedbackRevision] = useState(0);
  const notifyRead = useRef(onRead);
  notifyRead.current = onRead;
  const path = `/conversations/${encodeURIComponent(id)}/cursor`;
  function validate(value: Cursor) {
    if (
      !value ||
      value.conversationId !== id ||
      value.scope?.kind !== scope.kind ||
      value.scope?.id !== scope.id ||
      value.participant?.type !== participant.type ||
      value.participant?.id !== participant.id ||
      !Number.isSafeInteger(value.revision) ||
      value.revision < 1 ||
      !Number.isSafeInteger(value.readSequence) ||
      value.readSequence < 0 ||
      !Number.isSafeInteger(value.deliveredSequence) ||
      value.deliveredSequence < value.readSequence
    )
      throw new Error(
        "The saved read position does not match this channel and reader.",
      );
    if (
      known.current &&
      (value.revision < known.current.revision ||
        value.readSequence < known.current.readSequence ||
        value.deliveredSequence < known.current.deliveredSequence)
    )
      throw new Error(
        "The saved read position is older than the one already shown.",
      );
    return value;
  }
  function accept(value: Cursor | null) {
    known.current = value;
    setCursor(value);
    setLoaded(true);
    notifyRead.current(value?.readSequence || 0, value?.revision || 0);
  }
  async function load(manual = false) {
    if (lock.current) return;
    lock.current = true;
    setBusy(true);
    try {
      let value: Cursor | null;
      try {
        value = validate(
          await api<Cursor>(
            scoped(`${path}?participantType=user&participantId=local-operator`),
          ),
        );
      } catch (e) {
        if (e instanceof ApiError && e.status === 404 && !known.current)
          value = null;
        else throw e;
      }
      if (!alive.current) return;
      accept(value);
      setError("");
      if (manual) {
        setCheck(false);
        setNotice("Saved read position refreshed.");
      }
    } catch (e) {
      if (alive.current)
        setError(`Could not load your read position. ${message(e)}`);
    } finally {
      lock.current = false;
      if (alive.current) {
        setBusy(false);
        if (manual) setFeedbackRevision((n) => n + 1);
      }
    }
  }
  useEffect(() => {
    alive.current = true;
    void load();
    return () => {
      alive.current = false;
    };
  }, [id]);
  useEffect(() => {
    if (check || error) return;
    const timer = setInterval(() => void load(), 10000);
    return () => clearInterval(timer);
  }, [id, check, error]);
  useEffect(() => {
    if (feedbackRevision) feedback.current?.focus();
  }, [feedbackRevision]);
  async function mark() {
    if (
      lock.current ||
      !loaded ||
      !ready ||
      check ||
      error ||
      shownThrough <= (cursor?.readSequence || 0)
    )
      return;
    const target = shownThrough;
    lock.current = true;
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const response = await api<{ cursor: Cursor }>(path, {
        method: "PUT",
        body: {
          scope,
          participant,
          expectedRevision: cursor?.revision || 0,
          deliveredSequence: Math.max(target, cursor?.deliveredSequence || 0),
          readSequence: target,
        },
      });
      if (!alive.current) return;
      const value = validate(response.cursor);
      if (value.readSequence !== target)
        throw new Error(
          "The response did not confirm the requested read position.",
        );
      accept(value);
      setNotice(`Marked read through message ${target}.`);
    } catch (e) {
      if (alive.current) {
        setCheck(true);
        setError(
          `Your read position may have changed. Check the saved position before trying again. ${message(e)}`,
        );
      }
    } finally {
      lock.current = false;
      if (alive.current) {
        setBusy(false);
        setFeedbackRevision((n) => n + 1);
      }
    }
  }
  const read = cursor?.readSequence || 0;
  const unread = Math.max(0, lastSequence - read);
  return (
    <section className="channel-read-state" aria-label="Your reading position">
      <p
        ref={feedback}
        tabIndex={-1}
        role={error ? "alert" : "status"}
        className={error ? "error-text" : "muted"}
      >
        {error ||
          (!loaded
            ? "Loading your read position…"
            : unread
              ? `${unread} unread message${unread === 1 ? "" : "s"}. ${read ? `You marked read through message ${read}.` : "You have not marked any messages read."}`
              : "You’re caught up with this channel.")}
        {notice && !error && ` ${notice}`}
      </p>
      <div className="inspector-actions">
        {loaded && unread > 0 && (
          <button
            className="button"
            disabled={busy || !ready || !!error || check}
            onClick={() => onUnread(read)}
          >
            Start at unread messages
          </button>
        )}
        {loaded && shownThrough > read && (
          <button
            className="button"
            disabled={busy || !ready || !!error || check}
            onClick={() => void mark()}
          >
            Mark read through message {shownThrough}
          </button>
        )}
        <button
          className="button"
          disabled={busy}
          onClick={() => void load(true)}
        >
          {check ? "Check saved read position" : "Refresh read position"}
        </button>
      </div>
      {loaded && shownThrough > read && (
        <p className="inline-help">
          Marking read includes all earlier messages. Opening a channel does not
          mark it read.
        </p>
      )}
    </section>
  );
}
