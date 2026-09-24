import InstallationReview from "./InstallationReview";
import TeamProposal from "./TeamProposal";
import SkillInstall, { clawHubReference } from "./SkillInstall";
import SkillConfiguration from "./SkillConfiguration";
import SkillDiscovery from "./SkillDiscovery";
import {
  missingRequirementHelp,
  validationIssueHelp,
} from "./proposalBlockers";
import { useEffect, useRef, useState, type FormEvent } from "react";
import { LoaderCircle, RefreshCw } from "lucide-react";
import {
  api,
  isActivationProposal,
  ApiError,
  message,
  scope,
  scoped,
  type Capabilities,
  type Proposal,
  type RefinementQuestion,
  type RefinementValue,
} from "./api";

function nextQuestion(proposal: Proposal) {
  const answers = new Map(
    proposal.refinement?.answers?.map((a) => [a.questionId, a.value]),
  );
  return proposal.refinement?.questions
    ?.filter(
      (q) =>
        !answers.has(q.id) &&
        (q.dependsOn || []).every((d) => {
          const answer = answers.get(d.questionId);
          return (
            answer &&
            (d.requiredOptionIds || []).every((id) =>
              [
                ...(answer.optionIds || []),
                ...(answer.skillIds || []),
              ].includes(id),
            )
          );
        }),
    )
    .sort((a, b) => b.priority - a.priority)[0];
}

function Question({
  question,
  disabled,
  saving,
  onAnswer,
}: {
  question: RefinementQuestion;
  disabled: boolean;
  saving: boolean;
  onAnswer: (value: RefinementValue) => Promise<void>;
}) {
  const [text, setText] = useState("");
  const [selected, setSelected] = useState<string[]>([]);
  const [boolean, setBoolean] = useState("");
  const kind = question.answer.kind;
  const choice =
    ["single_select", "multi_select", "skill_selection"].includes(kind) &&
    !!question.answer.options?.length;
  const supported = choice || ["text", "string_list", "boolean"].includes(kind);
  const items = [
    ...new Set(
      text
        .split("\n")
        .map((s) => s.trim())
        .filter(Boolean),
    ),
  ];
  const cardinal = choice || kind === "string_list";
  const minimum = Math.max(1, question.answer.minimum || 1);
  const maximum =
    kind === "single_select" ? 1 : question.answer.maximum || Infinity;
  const count = choice
    ? selected.length
    : kind === "string_list"
      ? items.length
      : 1;
  const ready =
    count >= minimum &&
    count <= maximum &&
    (kind === "boolean"
      ? boolean !== ""
      : choice
        ? selected.length > 0
        : !!text.trim());
  const constraint = cardinal
    ? minimum === maximum
      ? `Provide exactly ${minimum} ${minimum === 1 ? "value" : "values"}.`
      : maximum < Infinity
        ? `Provide ${minimum} to ${maximum} values.`
        : `Provide at least ${minimum} ${minimum === 1 ? "value" : "values"}.`
    : "";
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!ready || disabled) return;
    const value: RefinementValue =
      kind === "boolean"
        ? { boolean: boolean === "yes" }
        : kind === "skill_selection"
          ? { skillIds: selected }
          : choice
            ? { optionIds: selected }
            : kind === "string_list"
              ? { items }
              : { text: text.trim() };
    await onAnswer(value);
  }
  return (
    <form className="proposal-question" onSubmit={submit}>
      <fieldset disabled={disabled}>
        <legend>{question.prompt}</legend>
        <p className="muted" id={`why-${question.id}`}>
          {question.whyNeeded}
        </p>
        {constraint && <p className="muted">{constraint}</p>}
        {choice ? (
          <div className="proposal-options">
            {question.answer.options?.map((option) => (
              <label key={option.id}>
                <input
                  type={kind === "single_select" ? "radio" : "checkbox"}
                  name={question.id}
                  value={option.id}
                  checked={selected.includes(option.id)}
                  disabled={
                    kind !== "single_select" &&
                    selected.length >= maximum &&
                    !selected.includes(option.id)
                  }
                  onChange={(e) =>
                    setSelected(
                      kind === "single_select"
                        ? [option.id]
                        : e.target.checked
                          ? [...selected, option.id]
                          : selected.filter((id) => id !== option.id),
                    )
                  }
                />
                <span>
                  {option.label}
                  {option.description && <small>{option.description}</small>}
                </span>
              </label>
            ))}
          </div>
        ) : kind === "boolean" ? (
          <div className="proposal-options">
            {["yes", "no"].map((value) => (
              <label key={value}>
                <input
                  type="radio"
                  name={question.id}
                  checked={boolean === value}
                  onChange={() => setBoolean(value)}
                />
                <span>{value === "yes" ? "Yes" : "No"}</span>
              </label>
            ))}
          </div>
        ) : supported ? (
          <label className="form-field">
            <span>
              {kind === "string_list"
                ? "Your answer · one item per line"
                : "Your answer"}
            </span>
            <textarea
              value={text}
              onChange={(e) => setText(e.target.value)}
              aria-describedby={`why-${question.id}`}
              rows={3}
              required
            />
          </label>
        ) : (
          <p className="muted">
            {kind === "credential_reference"
              ? "This question needs a credential reference. Answer it in a client that supports credential selection. Do not enter a secret here."
              : "No supported answer controls are available for this question. Your proposal is saved; use another compatible client to continue."}
          </p>
        )}
        {supported && (
          <button
            className="button primary"
            disabled={disabled || !ready}
            type="submit"
          >
            {saving ? "Saving answer…" : "Save answer and continue"}
          </button>
        )}
      </fieldset>
    </form>
  );
}

export default function ProposalReview({
  proposal,
  onChange,
  onInstalled,
  onDerived,
  onViewAgents,
  onViewTeams,
  onPrepareFreshProposal,
}: {
  proposal: Proposal;
  onChange: (value: Proposal) => void;
  onInstalled: () => void;
  onDerived: (value: Proposal) => void;
  onViewAgents: () => void;
  onViewTeams: () => void;
  onPrepareFreshProposal: (prompt: string) => void;
}) {
  const activating = isActivationProposal(proposal);
  const [capabilities, setCapabilities] = useState<Capabilities | null>(null);
  const [capError, setCapError] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [check, setCheck] = useState(0);
  const [installedSkill, setInstalledSkill] = useState(false);
  const locked = useRef(false);
  const pending = useRef<{ fingerprint: string; key: string } | null>(null);
  const feedback = useRef<HTMLDivElement>(null);
  const path = `/authoring/workforce/change-sets/${encodeURIComponent(proposal.id)}`;
  useEffect(() => {
    let current = true;
    setCapabilities(null);
    setCapError("");
    api<Capabilities>(
      scoped(`/capabilities?changeSetId=${encodeURIComponent(proposal.id)}`),
    ).then(
      (result) => {
        if (current) setCapabilities(result);
      },
      (e) => {
        if (current) setCapError(message(e));
      },
    );
    return () => {
      current = false;
    };
  }, [proposal.id, proposal.revision, check]);
  const capability = capabilities?.capabilities.find(
    (c) =>
      c.id === "workforce-authoring" &&
      c.available &&
      c.context?.changeSetId === proposal.id &&
      c.context.revision === proposal.revision,
  );
  const can = (operation: string) =>
    !!capability?.operations.includes(operation);
  const question = nextQuestion(proposal);
  async function mutate(
    operation: "retry" | "refinements",
    value?: RefinementValue,
  ) {
    if (locked.current || !can(operation === "retry" ? "retry" : "refine"))
      return;
    locked.current = true;
    setBusy(true);
    setError("");
    setNotice("");
    const body = {
      scope,
      changeSetId: proposal.id,
      expectedRevision: proposal.revision,
      ...(operation === "retry"
        ? { reason: "Retry generation from the desktop proposal review" }
        : { questionId: question?.id, value, source: "user" }),
    };
    const fingerprint = JSON.stringify({ operation, body });
    if (pending.current?.fingerprint !== fingerprint)
      pending.current = { fingerprint, key: crypto.randomUUID() };
    try {
      const result = await api<Proposal>(`${path}/${operation}`, {
        method: "POST",
        body,
        key: pending.current.key,
      });
      onChange(result);
      pending.current = null;
      setNotice(
        operation === "retry"
          ? "Generation restarted. You can leave this page while it runs."
          : "Answer saved. The proposal is being updated.",
      );
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        try {
          onChange(await api<Proposal>(scoped(path)));
        } catch {
          /* Preserve the original conflict; polling will retry the read. */
        }
        setError(
          "The proposal changed while you were reviewing it. Review the latest version before trying again.",
        );
      } else setError(message(e));
    } finally {
      locked.current = false;
      setBusy(false);
      requestAnimationFrame(() => feedback.current?.focus());
    }
  }
  const candidate = proposal.result?.candidate;
  const generating =
    proposal.status === "evaluating" &&
    !!proposal.generation &&
    !proposal.generation.completedAt;
  const status = generating
    ? "Generating proposal"
    : {
        review: "Ready to review",
        blocked: question ? "Needs your input" : "Needs attention",
        failed: "Generation failed",
        ready: "Ready for application",
        applied: "Applied",
        awaiting_approval: "Awaiting approval",
        evaluating: "Evaluation in progress",
        rejected: "Rejected",
      }[proposal.status] || proposal.status;
  return (
    <section
      className="proposal proposal-review"
      aria-labelledby="proposal-heading"
    >
      <div className="section-heading">
        <h2 id="proposal-heading" tabIndex={-1}>
          {activating
            ? "Activation proposal"
            : candidate?.team
              ? "Team proposal"
              : "Agent proposal"}
        </h2>
        <span className="proposal-status">
          {generating && <LoaderCircle size={14} className="spin" />}
          {status}
        </span>
      </div>
      <p className="proposal-prompt">
        {activating && <strong>Original installation request: </strong>}
        {proposal.prompt}
      </p>
      {generating && (
        <p className="muted">
          OpenSeal is preparing your proposal. Progress is saved automatically.
        </p>
      )}
      {proposal.generation?.lastError && proposal.status === "failed" && (
        <p className="error-text" role="alert">
          {proposal.generation.lastError}
        </p>
      )}
      {proposal.status === "failed" &&
        proposal.generation?.failureCode === "provider_failed" && (
          <p className="muted">
            The full error is recorded in logs/daemon.log inside the OpenSeal
            workspace folder. It is kept out of this screen because provider
            errors can contain your API key.
          </p>
        )}
      {candidate?.team && (
        <TeamProposal candidate={candidate} activating={activating} />
      )}
      {candidate?.agents?.map((agent) => (
        <article className="proposal-agent" key={agent.id}>
          <h3>{agent.displayName}</h3>
          <p>{agent.purpose}</p>
          {!!agent.skillRequirements?.length && (
            <p className="muted">
              Skills: {agent.skillRequirements.map((s) => s.skillId).join(", ")}
            </p>
          )}
          <details>
            <summary>Instructions</summary>
            <p className="preserve-lines">{agent.systemPrompt}</p>
            {!!agent.operatingPrinciples?.length && (
              <ul>
                {agent.operatingPrinciples.map((p, i) => (
                  <li key={i}>{p}</li>
                ))}
              </ul>
            )}
          </details>
        </article>
      ))}
      {!!proposal.result?.assumptions?.length && (
        <div className="proposal-notes">
          <h3>
            {activating
              ? "Original installation assumptions"
              : "Assumptions to review"}
          </h3>
          {activating && (
            <p className="muted">
              These assumptions describe the original installation. This
              proposal changes its inactive intent to active.
            </p>
          )}
          <ul>
            {proposal.result.assumptions.map((v, i) => (
              <li key={i}>{v}</li>
            ))}
          </ul>
        </div>
      )}
      {!!proposal.result?.validation?.length && (
        <div className="proposal-notes">
          <h3>Before this can proceed</h3>
          <ul>
            {proposal.result.validation.map((v, i) => (
              <li key={i}>
                <strong>{v.message}</strong>
                <span className="muted"> · {v.path}</span>
                <div>How to fix: {validationIssueHelp(v)}</div>
              </li>
            ))}
          </ul>
        </div>
      )}
      {!!proposal.result?.missingRequirements?.length && (
        <div className="proposal-notes">
          <h3>Required capabilities</h3>
          {candidate?.activation === "active" && (
            <p className="muted">
              This proposal enables the {candidate.team ? "Team" : "Agent"}
              {candidate.team ? " and its workflows" : ""} when installed. To
              save it first, make a new request to create it inactive; these
              activation requirements will still need to be resolved before
              enabling it later.
            </p>
          )}
          <ul>
            {proposal.result.missingRequirements.map((v, i) => {
              const help = missingRequirementHelp(v);
              const skill = proposal.catalog?.skills?.[v.id];
              const installable =
                v.kind === "skill_installation" && clawHubReference(skill);
              const credentialParts = /^agent:([^/]+)\/skill:(.+)$/.exec(
                v.requiredBy,
              );
              const configureCredential =
                v.kind === "credential" &&
                credentialParts &&
                !proposal.result?.missingRequirements?.some(
                  (other) =>
                    other.kind === "skill_binding" &&
                    other.id === credentialParts[2] &&
                    other.requiredBy === `agent:${credentialParts[1]}`,
                );
              return (
                <li key={i}>
                  <strong>{help.blocker}</strong>
                  <span className="muted"> · Required by {v.requiredBy}</span>
                  <div>
                    How to fix:{" "}
                    {installable
                      ? "Review and install the exact Skill version below, then create a fresh proposal to check the updated workspace."
                      : help.fix}
                  </div>
                  {installable && skill && (
                    <SkillInstall
                      skill={skill}
                      capabilities={capabilities}
                      onInstalled={() => setInstalledSkill(true)}
                    />
                  )}
                  {(v.kind === "skill" ||
                    v.kind === "skill_unavailable" ||
                    (v.kind === "skill_installation" && !installable)) && (
                    <SkillDiscovery
                      query={v.id}
                      capabilities={capabilities}
                      onInstalled={() => setInstalledSkill(true)}
                    />
                  )}
                  {v.kind === "skill_binding" &&
                    v.requiredBy.startsWith("agent:") && (
                      <SkillConfiguration
                        proposal={proposal}
                        capability={capability}
                        skillId={v.id}
                        ownerId={v.requiredBy.slice("agent:".length)}
                        onChange={onChange}
                      />
                    )}
                  {configureCredential && (
                    <SkillConfiguration
                      proposal={proposal}
                      capability={capability}
                      skillId={credentialParts[2]}
                      ownerId={credentialParts[1]}
                      onChange={onChange}
                    />
                  )}
                </li>
              );
            })}
          </ul>
          {(installedSkill ||
            proposal.result.missingRequirements.some((v) =>
              [
                "skill",
                "skill_installation",
                "skill_unavailable",
                "skill_binding",
                "credential",
              ].includes(v.kind),
            )) && (
            <button
              type="button"
              className="button primary"
              onClick={() => onPrepareFreshProposal(proposal.prompt)}
            >
              {installedSkill
                ? "Use installed Skill in a fresh proposal"
                : "Create a fresh proposal using current Skills"}
            </button>
          )}
        </div>
      )}
      {question && (
        <Question
          key={`${question.id}:${JSON.stringify(question.answer)}`}
          question={question}
          disabled={busy || !can("refine")}
          saving={busy}
          onAnswer={(value) => mutate("refinements", value)}
        />
      )}
      {question && capability && !can("refine") && (
        <p className="muted">
          Answering this question is not currently authorized for this
          workspace.
        </p>
      )}
      {capabilities && !capability && (
        <div className="proposal-notes">
          <p className="muted">
            Actions are unavailable for this proposal revision.
          </p>
          <button className="button" onClick={() => setCheck((v) => v + 1)}>
            Check actions again
          </button>
        </div>
      )}
      {capError && (
        <div role="alert" className="proposal-notes">
          <p>Could not check available actions. {capError}</p>
          <button className="button" onClick={() => setCheck((v) => v + 1)}>
            Check actions again
          </button>
        </div>
      )}
      {(error || notice) && (
        <div
          className={`proposal-feedback ${error ? "error-text" : "muted"}`}
          role={error ? "alert" : "status"}
          ref={feedback}
          tabIndex={-1}
        >
          {error || notice}
        </div>
      )}
      {can("retry") && (
        <button
          className="button"
          disabled={busy}
          onClick={() => void mutate("retry")}
        >
          <RefreshCw size={14} />
          {busy ? "Restarting…" : "Retry generation"}
        </button>
      )}
      <InstallationReview
        proposal={proposal}
        capability={capability}
        onChange={onChange}
        onDerived={onDerived}
        onInstalled={onInstalled}
        onViewAgents={onViewAgents}
        onViewTeams={onViewTeams}
      />
      {!!candidate && (
        <details className="proposal-technical">
          <summary>Technical details</summary>
          <pre>{JSON.stringify(candidate, null, 2)}</pre>
        </details>
      )}
    </section>
  );
}
