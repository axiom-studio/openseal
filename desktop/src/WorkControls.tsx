import WorkCollaboration from "./WorkCollaboration";
import WorkGuidance from "./WorkGuidance";
import { useEffect, useId, useRef, useState } from "react";
import { Pause, Play, Square } from "lucide-react";
import {
  api,
  ApiError,
  message,
  scoped,
  supports,
  type Capabilities,
  type Agent,
  type Run,
} from "./api";

const activeStatuses = [
  "queued",
  "running",
  "planning",
  "sleeping",
  "paused",
  "waiting_for_dependency",
  "waiting_for_agent",
  "waiting_for_approval",
  "waiting_for_event",
];
type Command = "pause" | "resume" | "cancel";
export default function WorkControls({
  run,
  capabilities,
  onChange,
  onOpenRun,
  agents,
  requestId,
}: {
  run: Run;
  agents: Agent[];
  requestId?: string;
  capabilities: Capabilities | null;
  onChange: (run: Run) => void;
  onOpenRun: (run: Run) => void;
}) {
  const [guidanceLaunch, setGuidanceLaunch] = useState<{
    id: string;
    context: string;
  }>();
  const cancelExplanation = useId();
  const [confirmCancel, setConfirmCancel] = useState(false);
  const [busy, setBusy] = useState<Command | null>(null);
  const [feedback, setFeedback] = useState("");
  const [failed, setFailed] = useState(false);
  const lock = useRef(false);
  const feedbackElement = useRef<HTMLParagraphElement>(null);
  const cancelButton = useRef<HTMLButtonElement>(null);
  const confirmButton = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (confirmCancel) {
      setConfirmCancel(false);
      setFeedback(
        "This run changed. Review its latest state before canceling.",
      );
      requestAnimationFrame(() => feedbackElement.current?.focus());
    }
  }, [run.revision]);
  useEffect(() => {
    if (confirmCancel) confirmButton.current?.focus();
  }, [confirmCancel]);
  const active = activeStatuses.includes(run.status);
  const can = (kind: Command) =>
    supports(capabilities, "agent-runs", kind) &&
    active &&
    (kind === "resume"
      ? run.status === "paused"
      : kind === "pause"
        ? run.status !== "paused"
        : true);
  async function command(kind: Command) {
    if (lock.current || !can(kind) || (kind === "cancel" && !confirmCancel))
      return;
    lock.current = true;
    setBusy(kind);
    setFeedback("");
    setFailed(false);
    try {
      const result = await api<{ run: Run }>(
        scoped(`/agent-runs/${encodeURIComponent(run.id)}/commands`),
        {
          method: "POST",
          body: {
            kind,
            expectedRevision: run.revision,
            actor: { type: "user", id: "local-operator" },
            summary: `${kind} requested from desktop`,
            visibility: "scope",
          },
        },
      );
      onChange(result.run);
      setConfirmCancel(false);
      setFeedback(
        kind === "cancel"
          ? "Work canceled. Saved results remain available."
          : kind === "pause"
            ? "Work paused."
            : "Work resumed.",
      );
    } catch (error) {
      setFailed(true);
      if (error instanceof ApiError && error.status === 409) {
        try {
          onChange(
            await api<Run>(scoped(`/agent-runs/${encodeURIComponent(run.id)}`)),
          );
          setConfirmCancel(false);
          setFeedback(
            "This run changed. Its latest state is shown. Review it before trying again.",
          );
        } catch {
          setFeedback(
            "This run changed, but its latest state could not be loaded. Check the connection before trying again.",
          );
        }
      } else
        setFeedback(
          `${message(error)} The request may have reached OpenSeal. Check the run status before trying again.`,
        );
    } finally {
      lock.current = false;
      setBusy(null);
      requestAnimationFrame(() => feedbackElement.current?.focus());
    }
  }
  return (
    <section aria-label="Work controls">
      <div className="inspector-actions">
        {can("resume") && (
          <button
            className="button"
            disabled={!!busy}
            aria-describedby={
              run.budgetState === "exhausted"
                ? "work-state-description"
                : undefined
            }
            onClick={() => void command("resume")}
          >
            <Play size={15} />
            {busy === "resume" ? "Resuming…" : "Resume"}
          </button>
        )}
        {can("pause") && (
          <button
            className="button"
            disabled={!!busy}
            onClick={() => void command("pause")}
          >
            <Pause size={15} />
            {busy === "pause" ? "Pausing…" : "Pause work"}
          </button>
        )}
        {can("cancel") && !confirmCancel && (
          <button
            ref={cancelButton}
            className="button"
            disabled={!!busy}
            onClick={() => setConfirmCancel(true)}
          >
            <Square size={15} />
            Cancel work
          </button>
        )}
      </div>
      {can("cancel") && confirmCancel && (
        <div>
          <p id={cancelExplanation}>
            End this run? Saved results remain available. External actions
            already started may still finish.
          </p>
          <div className="inspector-actions">
            <button
              ref={confirmButton}
              aria-describedby={cancelExplanation}
              className="button"
              disabled={!!busy}
              onClick={() => void command("cancel")}
            >
              {busy === "cancel" ? "Canceling…" : "Confirm cancellation"}
            </button>
            <button
              className="button"
              disabled={!!busy}
              onClick={() => {
                setConfirmCancel(false);
                requestAnimationFrame(() => cancelButton.current?.focus());
              }}
            >
              Go back
            </button>
          </div>
        </div>
      )}
      <p
        ref={feedbackElement}
        tabIndex={-1}
        role={failed ? "alert" : "status"}
        className={failed ? "error-text" : "muted"}
      >
        {feedback}
      </p>
      <WorkCollaboration
        requestId={requestId}
        agents={agents}
        run={run}
        capabilities={capabilities}
        onOpen={onOpenRun}
        onGuide={(request) =>
          setGuidanceLaunch({
            id: crypto.randomUUID(),
            context: `For collaboration request ${request.id}: ${request.goal.slice(0, 500)}
Recipient asks: ${request.clarification?.slice(0, 1500) || "More information needed."}

Guidance for the lead: `,
          })
        }
      />
      <WorkGuidance
        run={run}
        capabilities={capabilities}
        onChange={onChange}
        launch={guidanceLaunch}
      />
    </section>
  );
}
