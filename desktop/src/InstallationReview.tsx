import { useEffect, useRef, useState } from "react";
import { Check, LoaderCircle } from "lucide-react";
import { teamReviewIssue } from "./TeamProposal";
import {
  api,
  isActivationProposal,
  ApiError,
  message,
  scope,
  scoped,
  type Capability,
  type Proposal,
} from "./api";

type Operation = "evaluations" | "approvals" | "apply" | "activation";
export default function InstallationReview({
  proposal,
  capability,
  onChange,
  onInstalled,
  onDerived,
  onViewAgents,
  onViewTeams,
}: {
  proposal: Proposal;
  capability?: Capability;
  onChange: (value: Proposal) => void;
  onInstalled: () => void;
  onDerived: (value: Proposal) => void;
  onViewAgents: () => void;
  onViewTeams: () => void;
}) {
  const [reviewed, setReviewed] = useState(false);
  const [busy, setBusy] = useState<Operation | null>(null);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const pending = useRef<{ fingerprint: string; key: string } | null>(null);
  const lock = useRef(false);
  const feedback = useRef<HTMLDivElement>(null);
  const installedHeading = useRef<HTMLHeadingElement>(null);
  const focusInstalled = useRef(false);
  useEffect(() => {
    setReviewed(false);
    if (proposal.applyReceipt) {
      if (focusInstalled.current) {
        installedHeading.current?.focus();
        focusInstalled.current = false;
        setError("");
        setNotice("");
      }
    }
  }, [proposal.revision, proposal.candidateDigest, proposal.applyReceipt]);
  const can = (operation: string) =>
    capability?.context?.changeSetId === proposal.id &&
    capability.context.revision === proposal.revision &&
    capability.operations.includes(operation);
  const activating = isActivationProposal(proposal);
  const candidate = proposal.result?.candidate;
  const reviewIssue = teamReviewIssue(candidate);
  const activation = candidate?.activation;
  const knownIntent = activation === "active" || activation === "inactive";
  const requirements = capability?.context?.eligibleApprovalRequirements || [];
  const approval = requirements.find((r) =>
    proposal.evaluations?.some(
      (e) =>
        e.id === r.evaluationId &&
        e.allowed &&
        e.candidateDigest === proposal.candidateDigest,
    ),
  );
  const lifecycleAvailable = can("evaluate") || can("approve") || can("apply");
  async function act(operation: Operation) {
    const permission = {
      evaluations: "evaluate",
      approvals: "approve",
      apply: "apply",
      activation: "activate",
    }[operation];
    if (
      lock.current ||
      !can(permission) ||
      !knownIntent ||
      (operation !== "evaluations" && !!reviewIssue) ||
      !proposal.candidateDigest ||
      (operation === "approvals" && (!reviewed || !approval))
    )
      return;
    const body = {
      scope,
      changeSetId: proposal.id,
      expectedRevision: proposal.revision,
      ...(operation === "approvals"
        ? {
            ...approval,
            approved: true,
            reason:
              "Reviewed the proposal and installation behavior in the desktop app",
          }
        : {
            candidateDigest: proposal.candidateDigest,
            ...(operation === "apply" || operation === "activation"
              ? {
                  reason:
                    operation === "activation"
                      ? "Prepare a reviewed activation proposal for the installed resources"
                      : activation === "active"
                        ? "Install and activate the approved proposal"
                        : "Install the approved proposal without activation",
                }
              : {}),
          }),
    };
    const fingerprint = JSON.stringify({ operation, body });
    if (pending.current?.fingerprint !== fingerprint)
      pending.current = { fingerprint, key: crypto.randomUUID() };
    lock.current = true;
    setBusy(operation);
    setError("");
    setNotice("");
    const path = `/authoring/workforce/change-sets/${encodeURIComponent(proposal.id)}`;
    let installed = false;
    try {
      const result = await api<Proposal>(`${path}/${operation}`, {
        method: "POST",
        body,
        key: pending.current.key,
      });
      pending.current = null;
      if (operation === "activation") {
        onDerived(result);
        return;
      }
      installed = !!result.applyReceipt;
      focusInstalled.current = installed;
      onChange(result);
      if (result.applyReceipt) onInstalled();
      else
        setNotice(
          operation === "evaluations"
            ? activating
              ? "Activation checks finished. Review the result below."
              : "Installation checks finished. Review the result below."
            : result.status === "ready"
              ? activating
                ? "Approval saved. The proposal is ready to activate."
                : "Approval saved. The proposal is ready to install."
              : "Decision saved. Review any remaining requirements.",
        );
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        try {
          const latest = await api<Proposal>(scoped(path));
          installed = operation !== "activation" && !!latest.applyReceipt;
          focusInstalled.current = installed;
          onChange(latest);
        } catch {
          /* The parent continues polling. */
        }
        setError(
          "This proposal changed. Review the latest version before continuing.",
        );
      } else setError(message(e));
    } finally {
      lock.current = false;
      setBusy(null);
      if (!installed) requestAnimationFrame(() => feedback.current?.focus());
    }
  }
  const installedView = proposal.applyReceipt ? (
    <div className="proposal-installation">
      <h3 ref={installedHeading} tabIndex={-1}>
        <Check size={16} /> {activating ? "Activated" : "Installed"}
      </h3>
      <p role="status">
        {proposal.applyReceipt.activation === "active"
          ? "The approved resources are installed with activation enabled."
          : "The approved resources are installed and remain inactive."}
      </p>
      {proposal.applyReceipt.activation === "inactive" && can("activate") && (
        <>
          <p>
            Prepare an activation proposal to review before enabling these
            resources. They remain inactive until you approve and apply it.
          </p>
          <button
            className="button primary"
            disabled={!!busy || !!reviewIssue}
            onClick={() => void act("activation")}
          >
            {busy === "activation"
              ? "Preparing activation…"
              : "Review activation"}
          </button>
        </>
      )}
      <button className="button" onClick={onViewAgents}>
        View agents
      </button>
      {candidate?.team && (
        <button className="button" onClick={onViewTeams}>
          View teams
        </button>
      )}
    </div>
  ) : null;
  const unavailableView = (
    <p className="muted">
      {proposal.status === "rejected"
        ? "This proposal was rejected and has not been installed."
        : "This proposal has not been installed. Installation actions appear when the workspace authorizes the next step."}
    </p>
  );
  return (
    <div>
      {reviewIssue && (
        <p role="alert" className="error-text">
          {reviewIssue}
        </p>
      )}
      {proposal.applyReceipt ? (
        installedView
      ) : !lifecycleAvailable ? (
        unavailableView
      ) : (
        <section
          className="proposal-installation"
          aria-labelledby="installation-heading"
        >
          <h3 id="installation-heading">
            {activating
              ? "Activate installed resources"
              : "Install in this workspace"}
          </h3>
          <p>
            {activating
              ? "This proposal enables the existing installed resources. Their configured triggers, permissions, and approval rules will take effect."
              : activation === "active"
                ? "Installation activates the proposed resources. Their configured triggers, permissions, and approval rules will take effect."
                : activation === "inactive"
                  ? "Installation saves the proposed resources without activating them."
                  : "Activation intent is unavailable. Refresh the proposal before installing."}
          </p>
          <dl className="installation-summary">
            <div>
              <dt>Agents</dt>
              <dd>
                {candidate?.agents?.map((a) => a.displayName).join(", ") ||
                  "None"}
              </dd>
            </div>
            {candidate?.team && (
              <div>
                <dt>Team</dt>
                <dd>{candidate.team.displayName || "Proposed team"}</dd>
              </div>
            )}
            <div>
              <dt>Activation</dt>
              <dd>
                {activation === "active"
                  ? activating
                    ? "Enabled when approved activation is applied"
                    : "Enabled after installation"
                  : activation === "inactive"
                    ? "Inactive"
                    : "Unknown"}
              </dd>
            </div>
          </dl>
          {candidate?.agents?.map((agent) => (
            <p className="muted" key={agent.id}>
              {agent.displayName}: maximum risk{" "}
              {agent.authority?.maximumRisk || "not specified"}
              {agent.authority?.requireApprovalAt
                ? `; approval required at ${agent.authority.requireApprovalAt} risk`
                : ""}
              .
            </p>
          ))}
          {proposal.evaluations
            ?.filter((e) => e.candidateDigest === proposal.candidateDigest)
            .slice(-1)
            .map((e) =>
              e.findings?.map((finding, i) => (
                <p className="muted" key={`${e.id}-${i}`}>
                  {finding.message}
                </p>
              )),
            )}
          {can("evaluate") && (
            <button
              className="button primary"
              disabled={!!busy || !knownIntent}
              onClick={() => void act("evaluations")}
            >
              {busy === "evaluations" && (
                <LoaderCircle size={14} className="spin" />
              )}
              {busy === "evaluations"
                ? activating
                  ? "Checking activation…"
                  : "Checking installation…"
                : activating
                  ? "Check activation"
                  : "Check installation"}
            </button>
          )}
          {can("approve") && approval && (
            <>
              <label className="installation-consent">
                <input
                  type="checkbox"
                  checked={reviewed}
                  disabled={!!busy}
                  onChange={(e) => setReviewed(e.target.checked)}
                />
                <span>
                  I reviewed the proposed resources, instructions, permissions,
                  and activation behavior.
                </span>
              </label>
              <button
                className="button primary"
                disabled={!!busy || !reviewed || !knownIntent || !!reviewIssue}
                onClick={() => void act("approvals")}
              >
                {busy === "approvals"
                  ? "Saving approval…"
                  : activating
                    ? "Approve activation"
                    : "Approve installation"}
              </button>
            </>
          )}
          {can("approve") && !approval && (
            <p className="muted">
              Approval details do not match this version. Refresh the proposal
              before continuing.
            </p>
          )}
          {can("apply") && (
            <button
              className="button primary"
              disabled={!!busy || !knownIntent || !!reviewIssue}
              onClick={() => void act("apply")}
            >
              {busy === "apply" && <LoaderCircle size={14} className="spin" />}
              {busy === "apply"
                ? activating
                  ? "Activating…"
                  : "Installing…"
                : activating
                  ? "Activate resources"
                  : activation === "active"
                    ? "Install and activate"
                    : "Install without activating"}
            </button>
          )}
        </section>
      )}
      <div
        className={
          error || notice
            ? `proposal-feedback ${error ? "error-text" : "muted"}`
            : undefined
        }
        ref={feedback}
        tabIndex={-1}
        role={error ? "alert" : "status"}
      >
        {error || notice}
      </div>
    </div>
  );
}
