import { useEffect, useRef, useState } from "react";
import { api, ApiError, message, scoped, type Run } from "./api";

type Checkpoint = {
  id: string;
  runId: string;
  actionCallId: string;
  status: string;
  revision: number;
  summary: string;
  risk: string;
  policyReason?: string;
  proposedAction?: Record<string, unknown>;
  eligibleApprovers: { type: string; id: string }[];
  expiresAt: string;
  timeoutDecision?: string;
  decisionReason?: string;
  decisionBy?: { type: string; id: string };
};
type Call = {
  id: string;
  runId: string;
  approvalId: string;
  status: string;
  skillId: string;
  skillVersion: string;
  action: string;
  arguments?: Record<string, unknown>;
  sideEffect: string;
  invocationDigest?: string;
  revision: number;
};
type Decision = "approve" | "reject" | "request_changes";
const labels = {
  approve: "Approve action",
  reject: "Reject action",
  request_changes: "Request changes",
};
const consequences = {
  approve:
    "This permits the exact action shown above to execute. It may affect systems outside OpenSeal.",
  reject:
    "This blocks this action. The agent may continue its task and choose another approach.",
  request_changes:
    "This blocks this action and sends your guidance back to the agent. A revised action requires a new review.",
};
export default function RunApproval({
  run,
  approvalId,
  canResolve,
  onResolved,
}: {
  run: Run;
  approvalId?: string;
  canResolve: boolean;
  onResolved: (run: Run) => void;
}) {
  const id =
    approvalId ||
    (run.wakeCondition?.type === "approval" ? run.wakeCondition.reference : "");
  const [checkpoint, setCheckpoint] = useState<Checkpoint | null>(null);
  const [call, setCall] = useState<Call | null>(null);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [loading, setLoading] = useState(false);
  const [busy, setBusy] = useState(false);
  const [choice, setChoice] = useState<Decision | null>(null);
  const [reason, setReason] = useState("");
  const [reviewed, setReviewed] = useState(false);
  const [now, setNow] = useState(Date.now());
  const alive = useRef(true);
  const locked = useRef(false);
  const epoch = useRef(0);
  const signature = useRef("");
  const pending = useRef<{ fingerprint: string; key: string } | null>(null);
  const feedback = useRef<HTMLParagraphElement>(null);
  const confirm = useRef<HTMLButtonElement>(null);
  const review = useRef<HTMLInputElement>(null);
  const guidance = useRef<HTMLTextAreaElement>(null);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
      epoch.current++;
    };
  }, []);
  useEffect(() => {
    if (choice === "request_changes" && !reason.trim())
      guidance.current?.focus();
    else if (choice) confirm.current?.focus();
  }, [choice]);
  async function load(manual = false) {
    if (!id || locked.current) return;
    const request = ++epoch.current;
    setLoading(true);
    try {
      const approval = await api<Checkpoint>(
        scoped(`/action-approvals/${encodeURIComponent(id)}`),
      );
      const action = await api<Call>(
        scoped(`/action-calls/${encodeURIComponent(approval.actionCallId)}`),
      );
      if (!alive.current || request !== epoch.current) return;
      if (
        approval.id !== id ||
        approval.runId !== run.id ||
        action.runId !== run.id ||
        action.id !== approval.actionCallId ||
        action.approvalId !== approval.id
      )
        throw new Error(
          "The approval does not match this task and action. Reload before reviewing.",
        );
      const next = JSON.stringify([approval, action]);
      if (signature.current && signature.current !== next) {
        setChoice(null);
        setReviewed(false);
        pending.current = null;
        setNotice(
          approval.status === "pending"
            ? "The approval changed. Review its current details before deciding."
            : "Saved review updated.",
        );
      }
      signature.current = next;
      setCheckpoint(approval);
      setCall(action);
      setError("");
      if (manual)
        setNotice(
          approval.status === "pending"
            ? "Approval refreshed. Review its current details before deciding."
            : "Saved review refreshed.",
        );
    } catch (e) {
      if (alive.current && request === epoch.current) {
        setError(`Could not load approval. ${message(e)}`);
        setChoice(null);
        setReviewed(false);
      }
    } finally {
      if (alive.current && request === epoch.current) {
        setLoading(false);
        if (manual) requestAnimationFrame(() => feedback.current?.focus());
      }
    }
  }
  useEffect(() => {
    if (!id) return;
    void load();
    const timer = setInterval(() => {
      setNow(Date.now());
      if (!document.hidden) void load();
    }, 5000);
    return () => {
      clearInterval(timer);
      epoch.current++;
    };
  }, [id]);
  async function decide() {
    if (
      !checkpoint ||
      !choice ||
      !allowed ||
      locked.current ||
      !reviewed ||
      loading ||
      !!error ||
      (choice === "request_changes" && !reason.trim())
    )
      return;
    const decision = choice;
    const fingerprint = JSON.stringify([
      checkpoint.id,
      checkpoint.revision,
      decision,
      reason.trim(),
    ]);
    if (pending.current?.fingerprint !== fingerprint)
      pending.current = { fingerprint, key: crypto.randomUUID() };
    locked.current = true;
    epoch.current++;
    setBusy(true);
    setLoading(false);
    setError("");
    setNotice("");
    try {
      const result = await api<{ approval: Checkpoint; call: Call; run: Run }>(
        scoped(
          `/action-approvals/${encodeURIComponent(checkpoint.id)}/decisions`,
        ),
        {
          method: "POST",
          key: pending.current.key,
          body: {
            expectedRevision: checkpoint.revision,
            decisionId: pending.current.key,
            decision,
            principal: { type: "user", id: "local-operator" },
            reason: reason.trim(),
          },
        },
      );
      if (!alive.current) return;
      if (result.approval.id !== checkpoint.id || result.run.id !== run.id)
        throw new Error(
          "The decision response did not match this task. Refresh the approval to check its status.",
        );
      signature.current = JSON.stringify([result.approval, result.call]);
      setCheckpoint(result.approval);
      setCall(result.call);
      setChoice(null);
      setReviewed(false);
      pending.current = null;
      setNotice(
        result.approval.status === "approved"
          ? "Action approved. Execution remains subject to the run’s current state."
          : result.approval.status === "changes_requested"
            ? "Changes requested. Your guidance was saved for the agent."
            : result.approval.status === "rejected"
              ? "Action rejected. The agent may continue with another approach."
              : `Approval is ${result.approval.status.replaceAll("_", " ")}.`,
      );
      onResolved(result.run);
    } catch (e) {
      if (!alive.current) return;
      setError(
        e instanceof ApiError && e.status === 409
          ? "This approval changed. Refresh and review it before deciding again."
          : `Could not confirm the decision. ${message(e)} Refresh to check whether it was recorded; retrying the same decision uses the same request.`,
      );
    } finally {
      locked.current = false;
      if (alive.current) {
        setBusy(false);
        requestAnimationFrame(() => feedback.current?.focus());
      }
    }
  }
  if (!id && !checkpoint) return null;
  const deadline = checkpoint ? new Date(checkpoint.expiresAt) : null;
  const expired =
    !!deadline &&
    (!Number.isFinite(deadline.getTime()) || now >= deadline.getTime());
  const eligible = checkpoint?.eligibleApprovers?.some(
    (p) =>
      (p.type === "user" && p.id === "local-operator") ||
      (p.type === "role" && p.id === "operator"),
  );
  const allowed =
    !!checkpoint &&
    !!call &&
    checkpoint.id === id &&
    checkpoint.status === "pending" &&
    call.status === "waiting_for_approval" &&
    run.status === "waiting_for_approval" &&
    run.wakeCondition?.reference === checkpoint.id &&
    !expired &&
    canResolve &&
    !!eligible;
  return (
    <section aria-label="Action approval" className="action-approval">
      <h3>Action review</h3>
      {!checkpoint ? (
        <p className="muted" role="status">
          {loading
            ? "Loading the action for review…"
            : "Approval details are unavailable."}
        </p>
      ) : (
        <>
          <h4>{checkpoint.summary}</h4>
          <p className="muted">
            {checkpoint.policyReason ||
              "This action requires a decision before it can proceed."}
          </p>
          <dl>
            <dt>Risk</dt>
            <dd>{checkpoint.risk}</dd>
            <dt>Status</dt>
            <dd>{checkpoint.status.replaceAll("_", " ")}</dd>
            {call && (
              <>
                <dt>Action</dt>
                <dd>
                  {call.skillId} · {call.action} · version {call.skillVersion}
                </dd>
                <dt>Effects</dt>
                <dd>{call.sideEffect || "Not specified"}</dd>
              </>
            )}
            {deadline && Number.isFinite(deadline.getTime()) && (
              <>
                <dt>Deadline</dt>
                <dd>
                  <time dateTime={deadline.toISOString()}>
                    {deadline.toLocaleString()}
                  </time>
                </dd>
              </>
            )}
          </dl>
          {checkpoint.status === "pending" && (
            <p className="muted">
              {checkpoint.timeoutDecision === "approve"
                ? "The configured policy may approve this action automatically after its deadline."
                : "If no decision is recorded before the deadline, this approval expires."}
            </p>
          )}
          {call && (
            <>
              <h4>Action inputs</h4>
              <pre>{JSON.stringify(call.arguments || {}, null, 2)}</pre>
            </>
          )}
          {checkpoint.proposedAction && (
            <>
              <h4>Action preview</h4>
              <pre>{JSON.stringify(checkpoint.proposedAction, null, 2)}</pre>
            </>
          )}
          {checkpoint.status === "pending" && !allowed && (
            <p className="inline-help">
              {expired
                ? "The review deadline has passed. Refresh to check the recorded outcome."
                : run.status === "paused"
                  ? "Resume the task before deciding on this action."
                  : !canResolve
                    ? "This workspace does not allow approval decisions in the app."
                    : !eligible
                      ? "This action requires a different reviewer. The local operator is not eligible."
                      : "The task is no longer waiting on this action. Refresh to check its status."}
            </p>
          )}
          {checkpoint.decisionReason && (
            <p className="preserve-lines">
              Reviewer guidance: {checkpoint.decisionReason}
            </p>
          )}
          {checkpoint.decisionBy && (
            <p className="muted">Reviewed by {checkpoint.decisionBy.id}.</p>
          )}
          {allowed && (
            <>
              <label className="approval-review-check">
                <input
                  ref={review}
                  type="checkbox"
                  checked={reviewed}
                  disabled={busy || loading}
                  onChange={(e) => setReviewed(e.target.checked)}
                />
                I reviewed this action, its inputs, and its effects.
              </label>
              <label className="form-field">
                Reviewer guidance
                {choice === "request_changes" ? " (required)" : " (optional)"}
                <textarea
                  ref={guidance}
                  rows={3}
                  maxLength={4000}
                  value={reason}
                  disabled={busy}
                  onChange={(e) => setReason(e.target.value)}
                />
              </label>
              {choice ? (
                <>
                  <p id="approval-consequence">{consequences[choice]}</p>
                  <div className="inspector-actions">
                    <button
                      ref={confirm}
                      className="button primary"
                      aria-describedby="approval-consequence"
                      disabled={
                        busy ||
                        loading ||
                        !!error ||
                        !reviewed ||
                        (choice === "request_changes" && !reason.trim())
                      }
                      onClick={() => void decide()}
                    >
                      {busy
                        ? "Saving decision…"
                        : `Confirm: ${labels[choice].toLowerCase()}`}
                    </button>
                    <button
                      className="button"
                      disabled={busy}
                      onClick={() => {
                        setChoice(null);
                        requestAnimationFrame(() => review.current?.focus());
                      }}
                    >
                      Go back
                    </button>
                  </div>
                </>
              ) : (
                <div className="inspector-actions">
                  {(["approve", "request_changes", "reject"] as Decision[]).map(
                    (decision) => (
                      <button
                        className="button"
                        key={decision}
                        disabled={!reviewed || loading || !!error}
                        onClick={() => setChoice(decision)}
                      >
                        {labels[decision]}
                      </button>
                    ),
                  )}
                </div>
              )}
            </>
          )}
        </>
      )}
      <button
        className="text-button"
        disabled={busy || loading || !id}
        onClick={() => void load(true)}
      >
        Refresh approval
      </button>
      <p
        ref={feedback}
        tabIndex={-1}
        role={error ? "alert" : "status"}
        className={error ? "error-text" : "muted"}
      >
        {error || notice}
      </p>
    </section>
  );
}
