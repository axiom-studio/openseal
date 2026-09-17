import { useEffect, useId, useRef, useState } from "react";
import type { Run } from "./api";

const dimensions: Record<string, string> = {
  input_tokens: "input tokens",
  output_tokens: "output tokens",
  total_tokens: "total tokens",
  turns: "turns",
  attempts: "attempts",
  actions: "actions",
  duration_ms: "milliseconds of execution time",
  cost_micros: "millionths of the configured cost unit",
};
export default function WorkStatus({
  run,
  onProvider,
  onNewAttempt,
  hasOtherDraft = false,
}: {
  run: Run;
  onProvider: () => void;
  onNewAttempt?: () => void;
  hasOtherDraft?: boolean;
}) {
  const [replaceDraft, setReplaceDraft] = useState(false);
  const explanation = useId();
  const reviewButton = useRef<HTMLButtonElement>(null);
  const replaceButton = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (replaceDraft) replaceButton.current?.focus();
  }, [replaceDraft]);
  const providerRetry =
    run.status === "sleeping" &&
    run.wakeCondition?.reference === "hosted-turn-retry";
  const limited =
    run.status === "paused" &&
    (run.budgetState === "exhausted" || !!run.budgetAdmission);
  const descriptions: Record<string, string> = {
    queued:
      "Waiting for an available worker. Progress will appear here when the task starts.",
    planning: "The agent is preparing its next step.",
    running:
      "The agent is working. Progress and results are saved as they become available.",
    paused:
      "Work is paused. Resume returns it to its previous state, including any wait that was already in progress.",
    sleeping: "The run is waiting for its scheduled wake-up.",
    waiting_for_approval:
      "An action needs approval before this run can continue. Review the requested action below when approval details are available.",
    waiting_for_dependency:
      "This run is waiting for work it depends on to finish.",
    waiting_for_agent: "This run is waiting for a response from another agent.",
    waiting_for_event: "This run is waiting for its configured event.",
    failed:
      "This attempt ended with an error. Review the error before preparing another attempt.",
    canceled:
      "This run was canceled. Preparing another attempt creates a new run; it does not resume this one.",
  };
  const description = providerRetry
    ? "The model provider was unavailable. This run is waiting for an automatic retry. You can check the provider settings or pause the run."
    : descriptions[run.status];
  const scheduled = run.wakeCondition?.wakeAt
    ? new Date(run.wakeCondition.wakeAt)
    : null;
  const showTime =
    (run.status === "sleeping" || run.status === "waiting_for_event") &&
    scheduled &&
    Number.isFinite(scheduled.getTime());
  if (!description) return null;
  return (
    <section aria-label="Work status">
      <p id="work-state-description" className="muted">
        {description}
      </p>
      {limited && (
        <p className="muted">
          {run.budgetAdmission
            ? "At the last budget check, the next step needed more capacity than remained. Resume will recheck it. This app cannot increase the run’s budget yet."
            : "This run’s budget is used or reserved by pending work. Resume will check whether work can continue; reserved capacity is not necessarily spent."}
        </p>
      )}
      {limited && run.budgetAdmission && (
        <p className="muted">
          The next step needs {run.budgetAdmission.required.toLocaleString()}{" "}
          {dimensions[run.budgetAdmission.dimension] || "units"};{" "}
          {run.budgetAdmission.remaining.toLocaleString()} remain.
        </p>
      )}
      {showTime && (
        <p className="muted">
          Scheduled time:{" "}
          <time dateTime={scheduled.toISOString()}>
            {scheduled.toLocaleString()}
          </time>
        </p>
      )}
      {providerRetry && (
        <button className="button" onClick={onProvider}>
          Open provider settings
        </button>
      )}
      {onNewAttempt &&
        ["failed", "canceled"].includes(run.status) &&
        (replaceDraft ? (
          <div>
            <p id={explanation}>
              Replace your unfinished draft in Home with this task? No new run
              will start until you review and submit it.
            </p>
            <div className="inspector-actions">
              <button
                className="button"
                ref={replaceButton}
                aria-describedby={explanation}
                onClick={onNewAttempt}
              >
                Replace draft and review
              </button>
              <button
                className="button"
                onClick={() => {
                  setReplaceDraft(false);
                  requestAnimationFrame(() => reviewButton.current?.focus());
                }}
              >
                Keep draft
              </button>
            </div>
          </div>
        ) : (
          <button
            className="button"
            ref={reviewButton}
            onClick={() => {
              if (hasOtherDraft) setReplaceDraft(true);
              else onNewAttempt();
            }}
          >
            Review a new attempt
          </button>
        ))}
    </section>
  );
}
