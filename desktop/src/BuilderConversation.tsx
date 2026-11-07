import { useEffect, useRef, useState } from "react";
import {
  api,
  message,
  scoped,
  supports,
  type Agent,
  type Capabilities,
  type Run,
  type RunInspection,
} from "./api";
import { ChannelThread } from "./TeamChannels";
import RunApproval from "./RunApproval";

const cardKey = (run: Run) => `${run.id}:${run.wakeCondition?.reference}`;

// BuilderConversation is the chat Home opens for an agent or team request:
// the builder's messages, the person's replies, and the review card of the
// builder's newest Run while it waits on one. Nothing is created until the
// person approves that card.
export default function BuilderConversation({
  id,
  builderAgentID,
  agents,
  capabilities,
  onOpenRun,
  onResolved,
  onClose,
}: {
  id: string;
  builderAgentID: string;
  agents: Agent[];
  capabilities: Capabilities | null;
  onOpenRun: (run: Run, detail?: RunInspection) => void;
  onResolved: (run: Run) => void;
  onClose: () => void;
}) {
  const [waiting, setWaiting] = useState<Run | null>(null);
  const [error, setError] = useState("");
  const [refresh, setRefresh] = useState(0);
  const heading = useRef<HTMLHeadingElement>(null);
  // The card the person just decided stays, with its outcome, until the
  // builder puts up the next one.
  const decided = useRef("");
  const canReview =
    supports(capabilities, "channels", "runs") &&
    supports(capabilities, "action-approvals", "get") &&
    supports(capabilities, "action-calls", "get");
  useEffect(() => {
    if (!canReview || !builderAgentID) return;
    let canceled = false,
      inflight = false;
    async function load() {
      if (inflight) return;
      inflight = true;
      try {
        const runs = await api<Run[] | null>(
          scoped(`/conversations/${encodeURIComponent(id)}/runs?limit=5`),
        );
        if (canceled) return;
        if (runs !== null && !Array.isArray(runs))
          throw new Error("Unreadable builder activity.");
        const next =
          (runs || []).find(
            (run) =>
              run.owner?.type === "agent" &&
              run.owner.id === builderAgentID &&
              run.status === "waiting_for_approval" &&
              run.wakeCondition?.type === "approval" &&
              !!run.wakeCondition.reference,
          ) || null;
        setWaiting((current) =>
          next
            ? current &&
              current.id === next.id &&
              current.revision > next.revision
              ? current
              : next
            : current && cardKey(current) === decided.current
              ? current
              : null,
        );
        setError("");
      } catch (e) {
        if (!canceled)
          setError(`Could not check for a review card. ${message(e)}`);
      } finally {
        inflight = false;
      }
    }
    void load();
    const timer = setInterval(() => {
      if (!document.hidden) void load();
    }, 3000);
    return () => {
      canceled = true;
      clearInterval(timer);
    };
  }, [id, builderAgentID, canReview, refresh]);
  return (
    <section
      className="home-section builder-chat"
      aria-labelledby="builder-heading"
    >
      <div className="section-heading">
        <h2 id="builder-heading" ref={heading} tabIndex={-1}>
          Builder chat
        </h2>
        <button className="text-button" onClick={onClose}>
          Close builder chat
        </button>
      </div>
      <p className="inline-help">
        Builder drafts with you here. Reply to refine the draft; each agent or
        team is created only after you approve its review card.
      </p>
      {waiting && canReview && (
        <RunApproval
          key={cardKey(waiting)}
          run={waiting}
          canResolve={supports(capabilities, "action-approvals", "resolve")}
          onResolved={(next) => {
            decided.current = cardKey(waiting);
            setRefresh((n) => n + 1);
            onResolved(next);
          }}
        />
      )}
      {error && (
        <p role="alert" className="error-text">
          {error}
        </p>
      )}
      {builderAgentID ? (
        <ChannelThread
          id={id}
          owner={{ type: "agent", id: builderAgentID }}
          agents={agents}
          capabilities={capabilities}
          onOpenRun={onOpenRun}
        />
      ) : (
        <p className="muted" role="status">
          Opening the builder chat…
        </p>
      )}
    </section>
  );
}
