import ChannelReplyActivity from "./ChannelReplyActivity";
import { useEffect, useRef, useState } from "react";
import {
  api,
  message,
  scope,
  scoped,
  supports,
  type Agent,
  type Capabilities,
  type Run,
  type RunInspection,
} from "./api";
import type { Team } from "./TeamBrowser";
import ChannelComposer from "./ChannelComposer";
import ChannelSettings from "./ChannelSettings";
import ChannelReadState from "./ChannelReadState";
import ChannelMessageContext from "./ChannelMessageContext";
import type { ChannelWorkDraft } from "./TeamWork";
import ChannelReferences, { type ChannelReference } from "./ChannelReferences";

export type Conversation = {
  scope?: { kind: string; id: string };
  participation?: { enabled: boolean; afterSequence: number };
  id: string;
  title: string;
  owner: { type: string; id: string };
  status: string;
  revision: number;
  lastSequence: number;
  readPosition?: {
    participant: { type: string; id: string };
    readSequence: number;
    revision: number;
  };
};
export type Message = {
  id: string;
  conversationId: string;
  sequence: number;
  sender: { type: string; id: string };
  senderDisplayName?: string;
  intent: string;
  content: string;
  createdAt: string;
  audience: {
    kind: string;
    roles?: string[];
    participants?: { type: string; id: string }[];
  };
  replyToMessageId?: string;
  resolvesMessageId?: string;
  supersedesMessageId?: string;
  references?: ChannelReference[];
};
export default function TeamChannels({
  team,
  capabilities,
  agents,
  onWorkDraft,
  onOpenRun,
}: {
  team: Team;
  capabilities: Capabilities | null;
  agents: Agent[];
  onWorkDraft: (draft: ChannelWorkDraft) => void;
  onOpenRun: (run: Run, detail?: RunInspection) => void;
}) {
  const [open, setOpen] = useState(false);
  const [items, setItems] = useState<Conversation[]>([]);
  const [selected, setSelected] = useState("");
  const [offset, setOffset] = useState(0);
  const [filter, setFilter] = useState("active");
  const [more, setMore] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [refresh, setRefresh] = useState(0);
  const [creating, setCreating] = useState(false);
  const feedback = useRef<HTMLParagraphElement>(null);
  const focusAfter = useRef(false);
  const canList = supports(capabilities, "channels", "list");
  const canGet =
    supports(capabilities, "channels", "get") &&
    supports(capabilities, "channels", "read");
  const canCreate = supports(capabilities, "channels", "create");
  const canReadPosition = supports(capabilities, "channels", "receipts");
  useEffect(() => {
    if (!open || !canList) return;
    let canceled = false,
      inflight = false;
    async function load() {
      if (inflight) return;
      inflight = true;
      try {
        const result = await api<Conversation[] | null>(
          scoped(
            `/conversations?ownerType=team&ownerId=${encodeURIComponent(team.deployment.id)}&limit=21&offset=${offset}${filter === "all" ? "" : `&status=${filter}`}${canReadPosition ? "&participantType=user&participantId=local-operator" : ""}`,
          ),
        );
        if (canceled) return;
        if (result !== null && !Array.isArray(result))
          throw new Error("Unreadable channel list.");
        if (
          (result || []).some(
            (c) => c.owner.type !== "team" || c.owner.id !== team.deployment.id,
          )
        )
          throw new Error("Channel results do not match this team.");
        if (
          (result || []).some(
            (c) =>
              c.readPosition &&
              (c.readPosition.participant?.type !== "user" ||
                c.readPosition.participant?.id !== "local-operator" ||
                !Number.isSafeInteger(c.readPosition.readSequence) ||
                c.readPosition.readSequence < 0 ||
                !Number.isSafeInteger(c.readPosition.revision) ||
                c.readPosition.revision < 0 ||
                (c.readPosition.revision === 0 &&
                  c.readPosition.readSequence !== 0) ||
                !Number.isSafeInteger(c.lastSequence) ||
                c.lastSequence < 0),
          )
        )
          throw new Error("Unread counts do not match this channel reader.");
        setItems((previous) =>
          (result || []).slice(0, 20).map((item) => {
            const known = previous.find((c) => c.id === item.id);
            const channel =
              known && known.revision > item.revision ? known : item;
            const oldPosition = known?.readPosition;
            const readPosition =
              oldPosition &&
              (!item.readPosition ||
                oldPosition.revision > item.readPosition.revision ||
                oldPosition.readSequence > item.readPosition.readSequence)
                ? oldPosition
                : item.readPosition;
            return { ...channel, readPosition };
          }),
        );
        setMore((result || []).length > 20);
        setLoaded(true);
        setError("");
      } catch (e) {
        if (!canceled)
          setError(
            `Could not refresh channels. ${message(e)} Previously loaded channels may be out of date.`,
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
    const timer = setInterval(() => void load(), 5000);
    return () => {
      canceled = true;
      clearInterval(timer);
    };
  }, [
    open,
    canList,
    canReadPosition,
    team.deployment.id,
    offset,
    refresh,
    filter,
  ]);
  function page(next: number) {
    setItems([]);
    setLoaded(false);
    setOffset(next);
    focusAfter.current = true;
  }
  return (
    <section className="team-channels" aria-label="Team channels">
      <button
        className="button"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        {open ? "Hide channels" : "Channels"}
      </button>
      {open && (
        <>
          <h2>Team channels</h2>
          <p className="muted">
            {supports(capabilities, "channels", "configure-participation")
              ? "Keep the team’s conversations and decisions together. Enable automatic team replies in channel settings, or use Team work for delegated tasks."
              : "Keep the team’s conversations and decisions together. Posting a message saves it here; use Team work when you want agents to produce a result."}
          </p>
          {!canList ? (
            <p className="inline-help">
              Channels are unavailable in this workspace.
            </p>
          ) : (
            <>
              <div className="inspector-actions">
                <label className="filter-select">
                  <span className="sr-only">Channel status</span>
                  <select
                    value={filter}
                    onChange={(e) => {
                      setFilter(e.target.value);
                      setOffset(0);
                      setItems([]);
                      setMore(false);
                      setLoaded(false);
                      setSelected("");
                    }}
                  >
                    <option value="active">Active channels</option>
                    <option value="archived">Archived channels</option>
                    <option value="all">All channels</option>
                  </select>
                </label>
                <button
                  className="button"
                  disabled={busy}
                  onClick={() => {
                    focusAfter.current = true;
                    setRefresh((n) => n + 1);
                  }}
                >
                  Refresh channels
                </button>
                {canCreate && (
                  <button
                    className="button"
                    aria-expanded={creating}
                    onClick={() => setCreating((v) => !v)}
                  >
                    {creating ? "Hide channel form" : "New channel"}
                  </button>
                )}
              </div>
              {creating && canCreate && (
                <ChannelComposer
                  storageKey={`openseal.channel-create.${team.deployment.id}`}
                  path="/conversations"
                  label="Channel name"
                  action="Create channel"
                  titleOnly
                  focusOnMount
                  enabled
                  body={(title) => ({
                    scope,
                    owner: { type: "team", id: team.deployment.id },
                    title,
                  })}
                  rejected={() => setRefresh((n) => n + 1)}
                  committed={(result: Conversation) => {
                    if (
                      !result.id ||
                      result.owner?.type !== "team" ||
                      result.owner.id !== team.deployment.id
                    )
                      throw new Error(
                        "The returned channel does not match this team.",
                      );
                    setFilter("active");
                    setSelected(result.id);
                    setOffset(0);
                    setRefresh((n) => n + 1);
                  }}
                />
              )}
              <p
                ref={feedback}
                tabIndex={-1}
                role={error ? "alert" : "status"}
                className={error ? "error-text" : "muted"}
              >
                {error ||
                  (busy
                    ? "Loading channels…"
                    : loaded
                      ? `${items.length} channel${items.length === 1 ? "" : "s"} on this page.`
                      : "")}
              </p>
              {loaded && !items.length && !error && (
                <p className="muted">
                  {offset
                    ? "No channels on this page. Return to an earlier page."
                    : filter === "archived"
                      ? "No archived channels."
                      : "No channels in this view. Create one for a topic the team shares."}
                </p>
              )}
              <div
                className="channel-picker"
                role="group"
                aria-label="Choose a channel"
              >
                {items.map((c) => (
                  <button
                    key={c.id}
                    className="button"
                    aria-pressed={selected === c.id}
                    disabled={!canGet}
                    onClick={() => setSelected(c.id)}
                  >
                    {c.title}
                    {c.status === "archived" ? " · Archived" : ""}
                    {canReadPosition &&
                    c.readPosition &&
                    c.lastSequence > c.readPosition.readSequence
                      ? ` · ${c.lastSequence - c.readPosition.readSequence} unread`
                      : ""}
                  </button>
                ))}
              </div>
              {(offset > 0 || more) && (
                <div className="inspector-actions">
                  <button
                    className="button"
                    disabled={busy || offset === 0}
                    onClick={() => page(Math.max(0, offset - 20))}
                  >
                    Previous channels
                  </button>
                  <button
                    className="button"
                    disabled={busy || !more}
                    onClick={() => page(offset + 20)}
                  >
                    More channels
                  </button>
                </div>
              )}
              {!canGet && (
                <p className="inline-help">
                  This workspace lists channels but does not provide their
                  messages.
                </p>
              )}
              {selected && canGet && (
                <ChannelThread
                  key={selected}
                  id={selected}
                  teamId={team.deployment.id}
                  agents={agents}
                  capabilities={capabilities}
                  onWorkDraft={onWorkDraft}
                  onOpenRun={onOpenRun}
                  onReadPosition={(readSequence, revision) => {
                    setItems((current) =>
                      current.map((c) =>
                        c.id === selected &&
                        (!c.readPosition ||
                          (revision >= c.readPosition.revision &&
                            readSequence >= c.readPosition.readSequence))
                          ? {
                              ...c,
                              readPosition: {
                                participant: {
                                  type: "user",
                                  id: "local-operator",
                                },
                                readSequence,
                                revision,
                              },
                            }
                          : c,
                      ),
                    );
                  }}
                  onUpdated={(next) => {
                    setItems((current) =>
                      current.map((c) =>
                        c.id === next.id && next.revision >= c.revision
                          ? { ...next, readPosition: c.readPosition }
                          : c,
                      ),
                    );
                    setRefresh((n) => n + 1);
                  }}
                />
              )}
            </>
          )}
        </>
      )}
    </section>
  );
}
function ChannelThread({
  id,
  teamId,
  agents,
  capabilities,
  onUpdated,
  onWorkDraft,
  onOpenRun,
  onReadPosition,
}: {
  onReadPosition: (sequence: number, revision: number) => void;
  onUpdated: (value: Conversation) => void;
  onWorkDraft: (draft: ChannelWorkDraft) => void;
  onOpenRun: (run: Run, detail?: RunInspection) => void;
  id: string;
  teamId: string;
  agents: Agent[];
  capabilities: Capabilities | null;
}) {
  const [conversation, setConversation] = useState<Conversation | null>(null);
  const [messages, setMessages] = useState<Message[]>([]);
  const [before, setBefore] = useState(0);
  const [after, setAfter] = useState<number | null>(null);
  const [readSequence, setReadSequence] = useState<number | null>(null);
  const [more, setMore] = useState(false);
  const [busy, setBusy] = useState(true);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState("");
  const [refresh, setRefresh] = useState(0);
  const [reply, setReply] = useState<Message | null>(null);
  const heading = useRef<HTMLHeadingElement>(null),
    feedback = useRef<HTMLParagraphElement>(null);
  const focusAfter = useRef(false);
  const canPost = supports(capabilities, "channels", "post");
  useEffect(() => {
    if (conversation) heading.current?.focus();
  }, [!!conversation]);
  useEffect(() => {
    let canceled = false,
      inflight = false;
    async function load() {
      if (inflight) return;
      inflight = true;
      try {
        const [current, result] = await Promise.all([
          api<Conversation>(scoped(`/conversations/${encodeURIComponent(id)}`)),
          api<Message[] | null>(
            scoped(
              `/conversations/${encodeURIComponent(id)}/messages?limit=51&${after === null ? `order=desc&beforeSequence=${before}` : `order=asc&afterSequence=${after}`}`,
            ),
          ),
        ]);
        if (canceled) return;
        if (
          current.id !== id ||
          current.owner.type !== "team" ||
          current.owner.id !== teamId
        )
          throw new Error("This channel does not belong to the selected team.");
        if (result !== null && !Array.isArray(result))
          throw new Error("Unreadable message list.");
        if (
          (result || []).some(
            (m) =>
              m.conversationId !== id ||
              !Number.isSafeInteger(m.sequence) ||
              m.sequence < 1 ||
              (before > 0 && m.sequence >= before) ||
              (after !== null && m.sequence <= after),
          )
        )
          throw new Error("Messages do not match this channel.");
        setConversation((prev) =>
          prev && prev.revision > current.revision ? prev : current,
        );
        const visible = (result || []).slice(0, 50);
        setMessages(after === null ? visible.reverse() : visible);
        setMore((result || []).length > 50);
        setLoaded(true);
        setError("");
      } catch (e) {
        if (!canceled)
          setError(
            `Could not refresh this conversation. ${message(e)} Previously loaded messages may be out of date.`,
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
    const timer = setInterval(() => void load(), 5000);
    return () => {
      canceled = true;
      clearInterval(timer);
    };
  }, [id, teamId, before, after, refresh]);
  function changePage(next: number, unreadAfter: number | null = null) {
    setRefresh((n) => n + 1);
    setAfter(unreadAfter);
    setMessages([]);
    setMore(false);
    setBusy(true);
    setLoaded(false);
    setBefore(next);
    focusAfter.current = true;
  }
  function sender(m: Message) {
    const agent = agents.find(
      (a) => m.sender.type === "agent" && a.deployment.id === m.sender.id,
    );
    return (
      m.senderDisplayName ||
      (m.sender.type === "user" && m.sender.id === "local-operator"
        ? "You"
        : agent?.deployment.displayName ||
          agent?.definition.displayName ||
          `${m.sender.type}: ${m.sender.id}`)
    );
  }
  return (
    <section className="channel-thread" aria-label="Channel conversation">
      <h3 ref={heading} tabIndex={-1}>
        {conversation?.title || "Opening channel…"}
      </h3>
      {conversation && supports(capabilities, "channels", "update") && (
        <ChannelSettings
          participationAvailable={supports(
            capabilities,
            "channels",
            "configure-participation",
          )}
          automaticRepliesAvailable={supports(
            capabilities,
            "channels",
            "coordinate-automatically",
          )}
          conversation={conversation}
          onChange={(next) => {
            setConversation((current) =>
              current && current.revision > next.revision ? current : next,
            );
            onUpdated(next);
          }}
        />
      )}
      {conversation && supports(capabilities, "channels", "runs") && (
        <ChannelReplyActivity
          key={id}
          id={id}
          teamId={teamId}
          canInspect={supports(capabilities, "agent-runs", "get")}
          onOpenRun={onOpenRun}
          sender={sender}
        />
      )}
      {conversation && supports(capabilities, "channels", "receipts") && (
        <ChannelReadState
          key={id}
          id={id}
          lastSequence={Math.max(
            conversation.lastSequence,
            messages.at(-1)?.sequence || 0,
          )}
          shownThrough={messages.at(-1)?.sequence || 0}
          ready={loaded && !busy && !error}
          onUnread={(sequence) => changePage(0, sequence)}
          onRead={(sequence, revision) => {
            setReadSequence(sequence);
            onReadPosition(sequence, revision);
          }}
        />
      )}
      <div className="inspector-actions">
        <button
          className="button"
          disabled={busy}
          onClick={() => {
            focusAfter.current = true;
            setRefresh((n) => n + 1);
          }}
        >
          Refresh messages
        </button>
        {(after === null
          ? more
          : !!messages.length && messages[0].sequence > 1) && (
          <button
            className="button"
            disabled={busy}
            onClick={() => changePage(messages[0].sequence)}
          >
            Older messages
          </button>
        )}
        {after !== null && more && (
          <button
            className="button"
            disabled={busy}
            onClick={() =>
              changePage(0, messages[messages.length - 1].sequence)
            }
          >
            Newer messages
          </button>
        )}
        {(before > 0 || after !== null) && (
          <button
            className="button"
            disabled={busy}
            onClick={() => changePage(0)}
          >
            Latest messages
          </button>
        )}
      </div>
      <p
        ref={feedback}
        tabIndex={-1}
        role={error ? "alert" : "status"}
        className={error ? "error-text" : "muted"}
      >
        {error ||
          (busy
            ? "Loading messages…"
            : loaded
              ? messages.length
                ? `Messages ${messages[0].sequence}–${messages[messages.length - 1].sequence}${before ? " · Earlier history" : after !== null ? " · From your unread messages" : ""}.`
                : "No messages yet."
              : "")}
      </p>
      <ol className="channel-messages">
        {messages.map((m) => (
          <li key={m.id}>
            {readSequence !== null &&
              m.sequence > readSequence &&
              !messages.some(
                (previous) =>
                  previous.sequence > readSequence &&
                  previous.sequence < m.sequence,
              ) && (
                <p className="muted">
                  Unread messages
                  {m.sequence > readSequence + 1
                    ? " · More unread messages are in earlier history"
                    : ""}
                </p>
              )}
            <div className="channel-message-heading">
              <strong title={m.sender.id}>{sender(m)}</strong>
              <span className="muted">
                {m.intent.replaceAll("_", " ")} ·{" "}
                <time dateTime={m.createdAt}>
                  {new Date(m.createdAt).toLocaleString()}
                </time>
              </span>
            </div>
            {m.audience.kind !== "channel" && (
              <p className="inline-help">
                Audience:{" "}
                {m.audience.kind === "roles"
                  ? m.audience.roles?.join(", ")
                  : m.audience.participants
                      ?.map((p) => `${p.type}: ${p.id}`)
                      .join(", ")}
              </p>
            )}
            {(
              [
                ["Original message", m.replyToMessageId],
                ["Resolved message", m.resolvesMessageId],
                ["Replaced message", m.supersedesMessageId],
              ] as const
            ).map(
              ([label, target]) =>
                target && (
                  <ChannelMessageContext
                    key={`${label}:${target}`}
                    conversationId={id}
                    messageId={target}
                    label={label}
                    sender={sender}
                  />
                ),
            )}
            <p className="channel-message-content">{m.content}</p>
            {!!m.references?.length && (
              <ChannelReferences
                references={m.references}
                capabilities={capabilities}
                onOpenRun={onOpenRun}
              />
            )}
            <div className="inspector-actions">
              {canPost && conversation?.status === "active" && (
                <button className="button" onClick={() => setReply(m)}>
                  Reply to message {m.sequence}
                </button>
              )}
              {conversation &&
                supports(capabilities, "agent-runs", "create-team") && (
                  <button
                    className="button"
                    onClick={(event) => {
                      const trigger = event.currentTarget;
                      onWorkDraft({
                        teamId,
                        channel: conversation.title,
                        sequence: m.sequence,
                        content: m.content,
                        returnToMessage: () =>
                          trigger.isConnected && trigger.focus(),
                      });
                    }}
                  >
                    Use as team work
                  </button>
                )}
            </div>
          </li>
        ))}
      </ol>
      {conversation?.status === "archived" && (
        <p className="inline-help">
          This channel is archived. Its history remains available.
        </p>
      )}
      {conversation && canPost ? (
        <>
          {reply && (
            <div className="channel-reply">
              <p>
                Replying to {sender(reply)}: {reply.content.slice(0, 200)}
              </p>
              <button className="button" onClick={() => setReply(null)}>
                Cancel reply
              </button>
            </div>
          )}
          <ChannelComposer
            key={reply?.id || "new"}
            storageKey={`openseal.channel-message.${id}.${reply?.id || "new"}`}
            path={`/conversations/${encodeURIComponent(id)}/messages`}
            focusOnMount={!!reply}
            label={reply ? "Your reply" : "Message to this channel"}
            action="Post message"
            enabled={conversation.status === "active" && !error}
            context={
              reply
                ? "Posted as you, keeping the original message’s audience. Replies do not resolve action approvals."
                : conversation?.participation?.enabled
                  ? "Posted as you. Eligible team members may reply using your model provider. Messages do not approve actions."
                  : "Posted as you, visible in this channel. Messages do not start team work or resolve action approvals."
            }
            body={(content) => ({
              scope,
              expectedRevision: conversation.revision,
              sender: { type: "user", id: "local-operator" },
              intent: reply ? "answer" : "update",
              content,
              audience: reply?.audience || { kind: "channel" },
              ...(reply ? { replyToMessageId: reply.id } : {}),
            })}
            rejected={() => setRefresh((n) => n + 1)}
            committed={(result: {
              conversation: Conversation;
              message: Message;
            }) => {
              if (
                result.conversation?.id !== id ||
                result.message?.conversationId !== id
              )
                throw new Error(
                  "The saved message did not match this channel.",
                );
              setConversation((prev) =>
                prev && prev.revision > result.conversation.revision
                  ? prev
                  : result.conversation,
              );
              setBefore(0);
              setAfter(null);
              setRefresh((n) => n + 1);
            }}
          />
        </>
      ) : (
        conversation && (
          <p className="inline-help">
            Posting messages is unavailable in this workspace.
          </p>
        )
      )}
    </section>
  );
}
