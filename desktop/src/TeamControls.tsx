import { useEffect, useId, useRef, useState } from "react";
import {
  api,
  ApiError,
  message,
  scoped,
  supports,
  type Capabilities,
  type Proposal,
} from "./api";
import type { Team } from "./TeamBrowser";

type Operation = "pause" | "resume" | "archive" | "restore";
const operations = {
  pause: { label: "Pause team", state: "paused", done: "Team paused." },
  resume: { label: "Resume team", state: "active", done: "Team resumed." },
  archive: {
    label: "Archive team",
    state: "archived",
    done: "Team archived. Its saved records remain available.",
  },
  restore: {
    label: "Restore team",
    state: "paused",
    done: "Team restored in a paused state. Review it before resuming.",
  },
} as const;
type Change = {
  id: string;
  deploymentRevision: number;
  reason?: string;
  actorType: string;
  actorId: string;
  createdAt: string;
  fromVersion?: string;
  toVersion: string;
};

export default function TeamControls({
  team,
  capabilities,
  onChange,
  onOpenProposal,
}: {
  team: Team;
  capabilities: Capabilities | null;
  onChange: (team: Team) => void;
  onOpenProposal: (proposal: Proposal) => void;
}) {
  const { deployment } = team;
  const [intent, setIntent] = useState<{
    operation: Operation;
    revision: number;
  } | null>(null);
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState("");
  const [failed, setFailed] = useState(false);
  const [needsRefresh, setNeedsRefresh] = useState(false);
  const [historyOpen, setHistoryOpen] = useState(false);
  const [history, setHistory] = useState<Change[]>([]);
  const [historyBusy, setHistoryBusy] = useState(false);
  const [historyError, setHistoryError] = useState("");
  const [historyRefresh, setHistoryRefresh] = useState(0);
  const [count, setCount] = useState(20);
  const alive = useRef(true);
  const lock = useRef(false);
  const feedback = useRef<HTMLParagraphElement>(null);
  const input = useRef<HTMLTextAreaElement>(null);
  const trigger = useRef<HTMLButtonElement | null>(null);
  const reasonID = useId();
  const explanationID = useId();
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);
  useEffect(() => {
    if (intent && intent.revision !== deployment.revision) {
      setIntent(null);
      setFailed(true);
      setNotice(
        "This team changed. Your reason is kept; review its current status before choosing an action again.",
      );
      requestAnimationFrame(() => feedback.current?.focus());
    }
  }, [deployment.revision, intent]);
  const canUpdate =
    supports(capabilities, "team-definitions", "update") &&
    !!deployment.definitionId &&
    deployment.scope?.kind === "local" &&
    deployment.scope.id === "default";
  const canHistory = supports(capabilities, "team-definitions", "list");
  const allowed: Operation[] = deployment.activation
    ? []
    : deployment.status === "active"
      ? ["pause", "archive"]
      : deployment.status === "paused"
        ? ["resume", "archive"]
        : deployment.status === "archived"
          ? ["restore"]
          : [];
  const path = `/team-deployments/${encodeURIComponent(deployment.id)}`;
  useEffect(() => {
    if (!historyOpen || !canHistory) return;
    let canceled = false;
    setHistoryBusy(true);
    setHistoryError("");
    api<Change[] | null>(scoped(`${path}/activations`))
      .then((items) => {
        if (canceled) return;
        if (items !== null && !Array.isArray(items))
          throw new Error("The team history could not be read.");
        setHistory(
          (items || [])
            .slice()
            .sort(
              (a, b) =>
                b.deploymentRevision - a.deploymentRevision ||
                b.createdAt.localeCompare(a.createdAt),
            ),
        );
      })
      .catch((error) => {
        if (!canceled) setHistoryError(message(error));
      })
      .finally(() => {
        if (!canceled) setHistoryBusy(false);
      });
    return () => {
      canceled = true;
    };
  }, [historyOpen, canHistory, path, deployment.revision, historyRefresh]);
  function report(text: string, error = false) {
    setNotice(text);
    setFailed(error);
    requestAnimationFrame(() => feedback.current?.focus());
  }
  async function loadCurrent() {
    const result = await api<{ items: Team[] }>(scoped("/team-deployments"));
    if (!alive.current) return false;
    const current = result.items?.find(
      (item) => item.deployment.id === deployment.id,
    );
    if (!current)
      throw new Error(
        "This team is no longer available. Refresh the team list.",
      );
    if (current.deployment.revision < deployment.revision)
      throw new Error(
        "The saved team state is older than the displayed version.",
      );
    setIntent(null);
    setNeedsRefresh(false);
    onChange(current);
    return true;
  }
  async function checkSaved() {
    if (lock.current) return;
    lock.current = true;
    setBusy(true);
    try {
      if (await loadCurrent())
        report(
          "Saved team state loaded. Review its status and change history before choosing an action.",
        );
    } catch (error) {
      if (alive.current)
        report(`Could not check the team. ${message(error)}`, true);
    } finally {
      lock.current = false;
      if (alive.current) setBusy(false);
    }
  }
  async function submit() {
    if (
      lock.current ||
      !canUpdate ||
      !intent ||
      !allowed.includes(intent.operation) ||
      !reason.trim() ||
      needsRefresh ||
      intent.revision !== deployment.revision
    )
      return;
    const operation = intent.operation;
    lock.current = true;
    setBusy(true);
    setNotice("");
    try {
      const result = await api<{ deployment: Team["deployment"] }>(
        scoped(path),
        {
          method: "PUT",
          body: {
            deployment: { ...deployment, status: operations[operation].state },
            expectedRevision: intent.revision,
            actorType: "user",
            actorId: "local-operator",
            reason: reason.trim(),
          },
        },
      );
      if (!alive.current) return;
      if (
        result.deployment?.id !== deployment.id ||
        result.deployment.revision <= intent.revision ||
        result.deployment.status !== operations[operation].state ||
        result.deployment.activeVersion !== deployment.activeVersion
      )
        throw new Error(
          "The update response did not match this team change. Check saved state.",
        );
      setIntent(null);
      setReason("");
      onChange({ ...team, deployment: result.deployment });
      report(operations[operation].done);
    } catch (error) {
      if (!alive.current) return;
      setIntent(null);
      setNeedsRefresh(true);
      if (error instanceof ApiError && error.status === 409) {
        try {
          if (await loadCurrent())
            report(
              "This team changed. Your reason is kept; review the latest status before choosing an action again.",
              true,
            );
        } catch (refreshError) {
          if (alive.current)
            report(
              `This team changed, but its current state could not be loaded. ${message(refreshError)}`,
              true,
            );
        }
      } else
        report(
          `The change could not be confirmed. ${message(error)} Check saved state before trying again. Your reason is kept.`,
          true,
        );
    } finally {
      lock.current = false;
      if (alive.current) setBusy(false);
    }
  }
  async function reviewActivation() {
    if (lock.current || !deployment.activation) return;
    lock.current = true;
    setBusy(true);
    try {
      const id = deployment.activation.changeSetId;
      const proposal = await api<Proposal>(
        scoped(`/authoring/workforce/change-sets/${encodeURIComponent(id)}`),
      );
      if (!alive.current) return;
      if (proposal.id !== id)
        throw new Error("The installation proposal did not match this team.");
      onOpenProposal(proposal);
    } catch (error) {
      if (alive.current)
        report(`Could not open activation review. ${message(error)}`, true);
    } finally {
      lock.current = false;
      if (alive.current) setBusy(false);
    }
  }
  return (
    <div className="team-controls">
      {deployment.activation ? (
        <>
          <p className="inline-help">
            This team belongs to an inactive installation. Review activation for
            the team and its agents together.
          </p>
          {supports(capabilities, "workforce-authoring", "get") && (
            <button
              className="button"
              disabled={busy}
              onClick={() => void reviewActivation()}
            >
              Review team activation
            </button>
          )}
        </>
      ) : canUpdate && allowed.length > 0 ? (
        <>
          <div className="inspector-actions">
            {!intent &&
              allowed.map((operation) => (
                <button
                  key={operation}
                  id={`${reasonID}-${operation}`}
                  className="button"
                  disabled={busy || needsRefresh}
                  onClick={(event) => {
                    trigger.current = event.currentTarget;
                    setIntent({ operation, revision: deployment.revision });
                    setNotice("");
                    requestAnimationFrame(() => input.current?.focus());
                  }}
                >
                  {operations[operation].label}
                </button>
              ))}
          </div>
          {intent && (
            <form
              onSubmit={(event) => {
                event.preventDefault();
                void submit();
              }}
            >
              <p id={explanationID}>
                {intent.operation === "archive"
                  ? "Archiving makes this team unavailable for coordination and keeps its saved records. You can restore it later in a paused state."
                  : intent.operation === "restore"
                    ? "Restore this team in a paused state. It will stay unavailable for coordination until you resume it."
                    : intent.operation === "resume"
                      ? "Resuming makes this team available for coordination under its existing roles and permissions."
                      : "Pausing makes this team unavailable for coordination."}{" "}
                This does not cancel runs or pause member agents.
              </p>
              <label htmlFor={reasonID}>Reason for this team change</label>
              <textarea
                id={reasonID}
                ref={input}
                value={reason}
                onChange={(event) => setReason(event.target.value)}
                maxLength={1000}
                rows={3}
                disabled={busy}
                aria-describedby={explanationID}
              />
              <div className="inspector-actions">
                <button
                  className="button primary"
                  type="submit"
                  disabled={
                    busy ||
                    !reason.trim() ||
                    needsRefresh ||
                    intent.revision !== deployment.revision
                  }
                >
                  {busy ? "Saving…" : `Confirm ${intent.operation}`}
                </button>
                <button
                  className="button"
                  type="button"
                  disabled={busy}
                  onClick={() => {
                    setIntent(null);
                    requestAnimationFrame(() =>
                      document
                        .getElementById(trigger.current?.id || "")
                        ?.focus(),
                    );
                  }}
                >
                  Keep current status
                </button>
              </div>
            </form>
          )}
        </>
      ) : null}
      {notice && (
        <p
          ref={feedback}
          tabIndex={-1}
          role={failed ? "alert" : "status"}
          className={failed ? "error-text" : "muted"}
        >
          {notice}
        </p>
      )}
      {needsRefresh && (
        <button
          className="button"
          disabled={busy}
          onClick={() => void checkSaved()}
        >
          Check saved team state
        </button>
      )}
      {canHistory && (
        <details
          className="team-history"
          onToggle={(event) => setHistoryOpen(event.currentTarget.open)}
        >
          <summary>Team change history</summary>
          {historyBusy ? (
            <p role="status">Loading team history…</p>
          ) : historyError ? (
            <>
              <p role="alert" className="error-text">
                Could not load team history. {historyError}
              </p>
              <button
                className="button"
                onClick={() => setHistoryRefresh((value) => value + 1)}
              >
                Retry team history
              </button>
            </>
          ) : history.length ? (
            <>
              <ol>
                {history.slice(0, count).map((change) => (
                  <li key={change.id}>
                    <p>{change.reason || "No reason recorded."}</p>
                    <p className="muted">
                      Revision {change.deploymentRevision} · {change.actorType}:{" "}
                      {change.actorId} ·{" "}
                      {new Date(change.createdAt).toLocaleString()}
                    </p>
                  </li>
                ))}
              </ol>
              {history.length > count && (
                <button
                  className="button"
                  onClick={() => setCount((value) => value + 20)}
                >
                  Show older changes
                </button>
              )}
            </>
          ) : (
            <p className="muted">No team changes recorded.</p>
          )}
        </details>
      )}
    </div>
  );
}
