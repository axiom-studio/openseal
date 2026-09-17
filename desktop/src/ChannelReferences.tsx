import { useEffect, useId, useRef, useState } from "react";
import {
  api,
  message,
  scoped,
  supports,
  type Capabilities,
  type Run,
  type RunInspection,
} from "./api";
import TaskArtifacts from "./TaskArtifacts";

export type ChannelReference = { kind: string; id: string; version?: number };
const names: Record<string, string> = {
  run: "Work",
  artifact: "Artifact",
  agent_request: "Agent request",
  approval: "Approval",
  objective: "Objective",
  project: "Project",
  activity: "Activity",
  external_source: "External source",
  agent_control: "Agent control",
  agent_approvals: "Agent approvals",
  embed_session: "Session",
};

export default function ChannelReferences({
  references,
  capabilities,
  onOpenRun,
}: {
  references: ChannelReference[];
  capabilities: Capabilities | null;
  onOpenRun: (run: Run, detail?: RunInspection) => void;
}) {
  const [selected, setSelected] = useState<ChannelReference | null>(null);
  const source = useRef<HTMLButtonElement | null>(null);
  const panelId = useId();
  return (
    <section className="channel-references" aria-label="Message references">
      <ul>
        {references.map((ref, index) => {
          const valid =
            typeof ref.id === "string" &&
            !!ref.id.trim() &&
            (ref.version === undefined ||
              (Number.isSafeInteger(ref.version) && ref.version >= 0));
          const available =
            valid &&
            (ref.kind === "artifact"
              ? supports(capabilities, "artifacts", "get")
              : ["run", "agent_request", "approval"].includes(ref.kind) &&
                !ref.version &&
                supports(capabilities, "agent-runs", "get") &&
                (ref.kind !== "agent_request" ||
                  supports(capabilities, "agent-requests", "get")) &&
                (ref.kind !== "approval" ||
                  (supports(capabilities, "action-approvals", "get") &&
                    supports(capabilities, "action-calls", "get"))));
          const label = `${names[ref.kind] || "Reference"} · ${ref.id}${ref.version ? ` · Version ${ref.version}` : ""}`;
          const expanded =
            selected?.kind === ref.kind &&
            selected?.id === ref.id &&
            (selected?.version || 0) === (ref.version || 0);
          return (
            <li key={`${ref.kind}:${ref.id}:${ref.version}:${index}`}>
              {available ? (
                <button
                  className="button"
                  aria-expanded={expanded}
                  aria-controls={panelId}
                  onClick={(event) => {
                    source.current = event.currentTarget;
                    setSelected(expanded ? null : ref);
                  }}
                >
                  {label}
                </button>
              ) : (
                <p className="muted">
                  {label} —{" "}
                  {valid
                    ? "Opening this reference is unavailable in this workspace."
                    : "This reference is invalid."}
                </p>
              )}
            </li>
          );
        })}
      </ul>
      {selected && (
        <ReferenceDetail
          key={`${selected.kind}:${selected.id}:${selected.version}`}
          reference={selected}
          panelId={panelId}
          capabilities={capabilities}
          onOpenRun={onOpenRun}
          onClose={() => {
            setSelected(null);
            source.current?.focus();
          }}
        />
      )}
    </section>
  );
}

function ReferenceDetail({
  reference,
  panelId,
  capabilities,
  onOpenRun,
  onClose,
}: {
  reference: ChannelReference;
  panelId: string;
  capabilities: Capabilities | null;
  onOpenRun: (run: Run, detail?: RunInspection) => void;
  onClose: () => void;
}) {
  const [linked, setLinked] = useState<{
    run: Run;
    detail?: RunInspection;
    summary?: string;
    status?: string;
    question?: string;
    risk?: string;
  } | null>(null);
  const [error, setError] = useState("");
  const [attempt, setAttempt] = useState(0);
  const heading = useRef<HTMLHeadingElement>(null);
  const feedback = useRef<HTMLParagraphElement>(null);
  useEffect(() => {
    heading.current?.focus();
  }, []);
  useEffect(() => {
    if (reference.kind === "artifact") return;
    let canceled = false;
    setError("");
    setLinked(null);
    async function load() {
      let runId = reference.id;
      let detail: RunInspection | undefined;
      let summary: string | undefined,
        status: string | undefined,
        question: string | undefined,
        risk: string | undefined;
      if (reference.kind === "agent_request") {
        const item = await api<{
          id: string;
          sourceRunId: string;
          goal: string;
          status: string;
          clarification?: string;
        }>(scoped(`/agent-requests/${encodeURIComponent(reference.id)}`));
        if (canceled) return;
        if (
          item?.id !== reference.id ||
          !item.sourceRunId ||
          typeof item.goal !== "string" ||
          typeof item.status !== "string" ||
          (item.clarification !== undefined &&
            typeof item.clarification !== "string")
        )
          throw new Error("The returned request did not match this reference.");
        runId = item.sourceRunId;
        detail = { requestId: item.id };
        summary = item.goal;
        status = item.status;
        question = item.clarification;
      } else if (reference.kind === "approval") {
        const item = await api<{
          id: string;
          runId: string;
          actionCallId: string;
          summary: string;
          status: string;
          risk: string;
        }>(scoped(`/action-approvals/${encodeURIComponent(reference.id)}`));
        if (canceled) return;
        if (
          item?.id !== reference.id ||
          !item.runId ||
          !item.actionCallId ||
          typeof item.summary !== "string" ||
          typeof item.status !== "string" ||
          typeof item.risk !== "string"
        )
          throw new Error(
            "The returned approval did not match this reference.",
          );
        const action = await api<{
          id: string;
          runId: string;
          approvalId: string;
        }>(scoped(`/action-calls/${encodeURIComponent(item.actionCallId)}`));
        if (canceled) return;
        if (
          action?.id !== item.actionCallId ||
          action.runId !== item.runId ||
          action.approvalId !== item.id
        )
          throw new Error("The approval does not match its action and work.");
        runId = item.runId;
        detail = { approvalId: item.id };
        summary = item.summary;
        status = item.status;
        risk = item.risk;
      }
      const value = await api<Run>(
        scoped(`/agent-runs/${encodeURIComponent(runId)}`),
      );
      if (canceled) return;
      if (
        value?.id !== runId ||
        typeof value.goal !== "string" ||
        !value.owner?.id ||
        typeof value.status !== "string" ||
        !Number.isSafeInteger(value.revision) ||
        value.revision < 1
      )
        throw new Error("The returned work did not match this reference.");
      setLinked({ run: value, detail, summary, status, question, risk });
    }
    void load().catch((e) => {
      if (!canceled)
        setError(
          `Could not open referenced ${names[reference.kind]?.toLowerCase() || "record"}. ${message(e)}`,
        );
    });
    return () => {
      canceled = true;
    };
  }, [reference.id, reference.kind, attempt]);
  useEffect(() => {
    if (error) feedback.current?.focus();
  }, [error]);
  return (
    <section
      id={panelId}
      className="channel-context-body"
      aria-label="Reference details"
    >
      <h4 ref={heading} tabIndex={-1}>
        Referenced {names[reference.kind]?.toLowerCase() || "record"}
      </h4>
      {reference.kind === "artifact" ? (
        <TaskArtifacts
          reference={reference}
          canDownload={supports(capabilities, "artifacts", "download")}
        />
      ) : linked ? (
        <>
          <p className="channel-message-content">
            {linked.summary || linked.run.goal}
          </p>
          {linked.question && (
            <p className="channel-message-content">{linked.question}</p>
          )}
          <p className="muted">
            {(linked.status || linked.run.status).replaceAll("_", " ")} ·{" "}
            {linked.run.owner.type}: {linked.run.owner.id}
            {linked.risk && ` · ${linked.risk} risk`}
          </p>
          {reference.kind === "approval" && (
            <p className="inline-help">
              {linked.status === "pending"
                ? "Open the review to check the current action and its consequences before deciding."
                : "Open the review to inspect the action, its inputs, and the recorded outcome."}
            </p>
          )}
          <button
            className="button"
            onClick={(event) => {
              event.currentTarget.focus();
              onOpenRun(linked.run, linked.detail);
            }}
          >
            {reference.kind === "approval"
              ? "Open approval review"
              : reference.kind === "agent_request"
                ? "Open request details"
                : "Open work inspector"}
          </button>
        </>
      ) : (
        <>
          <p
            ref={feedback}
            tabIndex={-1}
            role={error ? "alert" : "status"}
            className={error ? "error-text" : "muted"}
          >
            {error ||
              `Loading referenced ${names[reference.kind]?.toLowerCase() || "record"}…`}
          </p>
          {error && (
            <button className="button" onClick={() => setAttempt((n) => n + 1)}>
              Retry opening{" "}
              {names[reference.kind]?.toLowerCase() || "reference"}
            </button>
          )}
        </>
      )}
      <button className="button" onClick={onClose}>
        Close reference
      </button>
    </section>
  );
}
