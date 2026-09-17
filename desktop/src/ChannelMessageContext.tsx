import { useEffect, useId, useRef, useState } from "react";
import { api, message, scoped } from "./api";
import type { Message } from "./TeamChannels";

// An exact read keeps older context available without moving the history window
// or remounting the message composer. The channel owns this component's lifetime.
export default function ChannelMessageContext({
  conversationId,
  messageId,
  label,
  sender,
}: {
  conversationId: string;
  messageId: string;
  label: string;
  sender: (value: Message) => string;
}) {
  const [open, setOpen] = useState(false);
  const [value, setValue] = useState<Message | null>(null);
  const [error, setError] = useState("");
  const [attempt, setAttempt] = useState(0);
  const trigger = useRef<HTMLButtonElement>(null);
  const heading = useRef<HTMLHeadingElement>(null);
  const feedback = useRef<HTMLParagraphElement>(null);
  const panelId = useId();

  useEffect(() => {
    if (!open) return;
    let canceled = false;
    setValue(null);
    setError("");
    void api<Message>(
      scoped(
        `/conversations/${encodeURIComponent(conversationId)}/messages/${encodeURIComponent(messageId)}`,
      ),
    )
      .then((result) => {
        if (canceled) return;
        if (
          result?.id !== messageId ||
          result.conversationId !== conversationId ||
          !Number.isSafeInteger(result.sequence) ||
          result.sequence < 1 ||
          typeof result.content !== "string" ||
          !result.sender?.id ||
          !result.sender.type ||
          !result.audience?.kind ||
          !Number.isFinite(Date.parse(result.createdAt))
        )
          throw new Error(
            "The returned message did not match this conversation.",
          );
        setValue(result);
      })
      .catch((e) => {
        if (!canceled)
          setError(
            `Could not open this message. ${message(e)} Your place and draft are unchanged.`,
          );
      });
    return () => {
      canceled = true;
    };
  }, [open, conversationId, messageId, attempt]);

  useEffect(() => {
    if (open && value) heading.current?.focus();
    else if (open && error) feedback.current?.focus();
  }, [open, value, error]);

  return (
    <div className="channel-context">
      <button
        className="button"
        ref={trigger}
        aria-expanded={open}
        aria-controls={panelId}
        onClick={() => {
          setValue(null);
          setError("");
          setOpen((current) => !current);
        }}
      >
        {label}
      </button>
      {open && (
        <section
          id={panelId}
          className="channel-context-body"
          aria-label={label}
        >
          {value ? (
            <>
              <h4 ref={heading} tabIndex={-1}>
                {label} · Message {value.sequence}
              </h4>
              <div className="channel-message-heading">
                <strong title={value.sender.id}>{sender(value)}</strong>
                <time className="muted" dateTime={value.createdAt}>
                  {new Date(value.createdAt).toLocaleString()}
                </time>
              </div>
              {value.audience.kind !== "channel" && (
                <p className="inline-help">
                  Audience:{" "}
                  {value.audience.kind === "roles"
                    ? value.audience.roles?.join(", ")
                    : value.audience.participants
                        ?.map((p) => `${p.type}: ${p.id}`)
                        .join(", ")}
                </p>
              )}
              <p className="channel-message-content">{value.content}</p>
            </>
          ) : (
            <p
              ref={feedback}
              tabIndex={-1}
              role={error ? "alert" : "status"}
              className={error ? "error-text" : "muted"}
            >
              {error || "Opening message…"}
            </p>
          )}
          <div className="inspector-actions">
            {error && (
              <button
                className="button"
                onClick={() => setAttempt((n) => n + 1)}
              >
                Retry opening message
              </button>
            )}
            <button
              className="button"
              onClick={() => {
                setOpen(false);
                trigger.current?.focus();
              }}
            >
              Close message
            </button>
          </div>
        </section>
      )}
    </div>
  );
}
