import { useEffect, useId, useRef, useState } from "react";
import {
  api,
  ApiError,
  message,
  scoped,
  supports,
  type Agent,
  type Capabilities,
} from "./api";
import type { Team } from "./TeamBrowser";

type Assignment = Team["deployment"]["roster"][number];
type Draft = {
  version: 2;
  revision: number;
  base: Assignment[];
  roster: Assignment[];
  reason: string;
};
type Conflict = { id: string; mine?: Assignment; saved?: Assignment };
type Merge = {
  revision: number;
  base: Assignment[];
  roster: Assignment[];
  conflicts: Conflict[];
  choices: Record<string, "mine" | "saved">;
};
const fields = (a?: Assignment) =>
  a && [
    a.id,
    a.agentDeploymentId,
    a.displayName || "",
    a.purpose || "",
    a.channelParticipation || "",
    a.requiredSkillIds || [],
    !!a.approver,
    !!a.lead,
    a.owns || [],
  ];
const equal = (a?: Assignment, b?: Assignment) =>
  JSON.stringify(fields(a)) === JSON.stringify(fields(b));
const sameRoster = (a: Assignment[], b: Assignment[]) =>
  a.length === b.length &&
  a.every((item) =>
    equal(
      item,
      b.find((other) => other.id === item.id),
    ),
  );
function reconcile(
  base: Assignment[],
  mine: Assignment[],
  saved: Assignment[],
) {
  const result = new Map(saved.map((item) => [item.id, item]));
  const conflicts: Conflict[] = [];
  for (const id of new Set([...base, ...mine].map((item) => item.id))) {
    const before = base.find((item) => item.id === id);
    const proposed = mine.find((item) => item.id === id);
    if (equal(before, proposed)) continue;
    const current = result.get(id);
    if (!equal(current, before) && !equal(current, proposed))
      conflicts.push({ id, mine: proposed, saved: current });
    else if (proposed) result.set(id, proposed);
    else result.delete(id);
  }
  return { roster: [...result.values()], conflicts };
}
function fresh(team: Team): Draft {
  return {
    version: 2,
    revision: team.deployment.revision,
    base: team.deployment.roster,
    roster: team.deployment.roster,
    reason: "",
  };
}
function restore(key: string, team: Team): Draft {
  try {
    const value = JSON.parse(localStorage.getItem(key) || "null");
    const valid = (items: unknown) =>
      Array.isArray(items) &&
      items.length <= 10000 &&
      items.every(
        (item) =>
          typeof item?.id === "string" &&
          typeof item.agentDeploymentId === "string" &&
          (item.displayName === undefined ||
            typeof item.displayName === "string"),
      );
    if (
      value?.version === 2 &&
      Number.isSafeInteger(value.revision) &&
      value.revision > 0 &&
      typeof value.reason === "string" &&
      value.reason.length <= 1000 &&
      valid(value.base) &&
      valid(value.roster)
    )
      return value;
  } catch {
    /* An unreadable draft must not block the saved roster. */
  }
  return fresh(team);
}
function qualifies(agent: Agent, member?: Assignment) {
  return (member?.requiredSkillIds || []).every((id) =>
    agent.definition.skillRequirements?.some((skill) => skill.skillId === id),
  );
}

export default function TeamRoster({
  team,
  agents,
  capabilities,
  onChange,
}: {
  team: Team;
  agents: Agent[];
  capabilities: Capabilities | null;
  onChange: (team: Team) => void;
}) {
  const key = `openseal.roster.${team.deployment.id}`;
  const [draft, setDraft] = useState(() => restore(key, team));
  const [open, setOpen] = useState(false);
  const [reviewed, setReviewed] = useState(false);
  const [merge, setMerge] = useState<Merge | null>(null);
  const [busy, setBusy] = useState(false);
  const [uncertain, setUncertain] = useState(false);
  const [notice, setNotice] = useState("");
  const [feedbackRevision, setFeedbackRevision] = useState(0);
  const [error, setError] = useState(false);
  const [storageIssue, setStorageIssue] = useState(false);
  const alive = useRef(true);
  const lock = useRef(false);
  const toggle = useRef<HTMLButtonElement>(null);
  const heading = useRef<HTMLHeadingElement>(null);
  const feedback = useRef<HTMLParagraphElement>(null);
  useEffect(() => {
    if (notice) feedback.current?.focus();
  }, [feedbackRevision]);
  const labelID = useId();
  const changed = !sameRoster(draft.base, draft.roster);
  const outdated = draft.revision !== team.deployment.revision;
  const allowed =
    supports(capabilities, "team-definitions", "update") &&
    supports(capabilities, "agent-definitions", "list") &&
    team.deployment.scope?.kind === "local" &&
    team.deployment.scope.id === "default" &&
    !!team.deployment.definitionId &&
    team.deployment.status !== "archived";
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);
  useEffect(() => {
    try {
      if (changed || draft.reason)
        localStorage.setItem(key, JSON.stringify(draft));
      else localStorage.removeItem(key);
      setStorageIssue(false);
    } catch {
      setStorageIssue(true);
    }
  }, [draft, key, changed]);
  const eligibility = JSON.stringify(
    agents.map((agent) => [
      agent.deployment.id,
      agent.deployment.definitionId,
      agent.definition.id,
      agent.definition.skillRequirements,
      agent.deployment.rolloutStatus,
    ]),
  );
  useEffect(() => {
    setReviewed(false);
  }, [team.deployment.revision, draft, eligibility]);
  function report(text: string, failed = false) {
    setNotice(text);
    setError(failed);
    setFeedbackRevision((value) => value + 1);
  }
  function changeRoster(roster: Assignment[]) {
    setDraft((current) => ({ ...current, roster }));
    setReviewed(false);
    setNotice("");
  }
  const agentName = (id: string) => {
    const agent = agents.find((item) => item.deployment.id === id);
    return agent?.deployment.displayName || agent?.definition.displayName || id;
  };
  const describe = (assignment?: Assignment) =>
    assignment
      ? `${agentName(assignment.agentDeploymentId)}${assignment.displayName ? ` (${assignment.displayName})` : ""}`
      : "No assignment";
  const problems: string[] = [];
  for (const member of draft.roster) {
    const agent = agents.find(
      (item) => item.deployment.id === member.agentDeploymentId,
    );
    if (!agent || !qualifies(agent, member))
      problems.push(
        `${agentName(member.agentDeploymentId)} no longer meets the requirements for ${member.displayName || "this member"}.`,
      );
  }
  if (
    new Set(draft.roster.map((item) => item.agentDeploymentId)).size !==
    draft.roster.length
  )
    problems.push("Each agent can be only one member of this team.");
  const available = agents.filter(
    (agent) =>
      !draft.roster.some(
        (item) => item.agentDeploymentId === agent.deployment.id,
      ),
  );
  const differences = [
    ...new Set([...draft.base, ...draft.roster].map((item) => item.id)),
  ].filter(
    (id) =>
      !equal(
        draft.base.find((item) => item.id === id),
        draft.roster.find((item) => item.id === id),
      ),
  );
  async function refresh() {
    if (lock.current) return;
    lock.current = true;
    setBusy(true);
    try {
      const result = await api<{ items: Team[] }>(scoped("/team-deployments"));
      if (!alive.current) return;
      const latest = result.items?.find(
        (item) => item.deployment.id === team.deployment.id,
      );
      if (!latest || latest.deployment.revision < team.deployment.revision)
        throw new Error(
          "The current team could not be verified. Refresh the team list.",
        );
      onChange(latest);
      setUncertain(false);
      setReviewed(false);
      if (changed && sameRoster(latest.deployment.roster, draft.roster)) {
        setDraft(fresh(latest));
        setMerge(null);
        report(
          "The saved roster matches your draft. Review team change history for the recorded change.",
        );
        return;
      }
      const merged = reconcile(
        draft.base,
        draft.roster,
        latest.deployment.roster,
      );
      if (merged.conflicts.length)
        setMerge({
          ...merged,
          revision: latest.deployment.revision,
          base: latest.deployment.roster,
          choices: {},
        });
      else {
        setDraft({
          ...draft,
          revision: latest.deployment.revision,
          base: latest.deployment.roster,
          roster: merged.roster,
        });
        setMerge(null);
      }
      report(
        merged.conflicts.length
          ? "Some assignments changed in both versions. Choose which to keep before reviewing the roster."
          : "Your draft now includes the latest saved changes. Review it before saving.",
      );
    } catch (failure) {
      if (alive.current)
        report(`Could not load the latest team. ${message(failure)}`, true);
    } finally {
      lock.current = false;
      if (alive.current) setBusy(false);
    }
  }
  async function save() {
    if (
      lock.current ||
      !allowed ||
      !changed ||
      outdated ||
      uncertain ||
      merge ||
      problems.length ||
      !reviewed ||
      !draft.reason.trim()
    )
      return;
    lock.current = true;
    setBusy(true);
    setNotice("");
    try {
      const result = await api<{ deployment: Team["deployment"] }>(
        scoped(`/team-deployments/${encodeURIComponent(team.deployment.id)}`),
        {
          method: "PUT",
          body: {
            deployment: { ...team.deployment, roster: draft.roster },
            expectedRevision: draft.revision,
            actorType: "user",
            actorId: "local-operator",
            reason: draft.reason.trim(),
          },
        },
      );
      if (!alive.current) return;
      if (
        result.deployment?.id !== team.deployment.id ||
        result.deployment.revision <= draft.revision ||
        result.deployment.activeVersion !== team.deployment.activeVersion ||
        !sameRoster(result.deployment.roster, draft.roster)
      )
        throw new Error("The update response did not match this roster.");
      const next = { ...team, deployment: result.deployment };
      onChange(next);
      setDraft(fresh(next));
      setReviewed(false);
      setOpen(false);
      report(
        "Team roster saved. Agent availability and running work are unchanged.",
      );
    } catch (failure) {
      if (!alive.current) return;
      setReviewed(false);
      const rejected =
        failure instanceof ApiError &&
        failure.status >= 400 &&
        failure.status < 500 &&
        failure.status !== 409;
      setUncertain(!rejected);
      report(
        rejected
          ? `The roster was not saved. ${message(failure)} Your draft is kept.`
          : `The roster change could not be confirmed. ${message(failure)} Check the latest team before trying again. Your draft is kept.`,
        true,
      );
    } finally {
      lock.current = false;
      if (alive.current) setBusy(false);
    }
  }
  if (!allowed && !changed && !draft.reason) return null;
  return (
    <section className="team-roster" aria-label="Edit team roster">
      <button
        ref={toggle}
        hidden={open}
        className="button"
        aria-expanded={open}
        onClick={() => {
          setOpen((current) => !current);
          if (!open) {
            if (!changed && !draft.reason) setDraft(fresh(team));
            requestAnimationFrame(() => heading.current?.focus());
          }
        }}
      >
        {changed || draft.reason ? "Continue roster draft" : "Edit roster"}
      </button>
      {notice && (
        <p
          ref={feedback}
          tabIndex={-1}
          role={error ? "alert" : "status"}
          className={error ? "error-text" : "muted"}
        >
          {notice}
        </p>
      )}
      {open && (
        <div>
          <h3 ref={heading} tabIndex={-1}>
            Edit members
          </h3>
          <p>
            Choose installed agents that meet each member’s requirements. Each
            agent can be one member. Changing membership does not activate
            agents or cancel running work.
          </p>
          <p className="muted">
            {storageIssue
              ? "This device could not save the draft. Keep this editor open to retain your changes."
              : "Unfinished roster changes are saved on this device."}
          </p>
          {!allowed && (
            <p role="alert">
              Roster editing is unavailable for this team. Your saved draft
              remains below.
            </p>
          )}
          {(outdated || uncertain) && (
            <p role="alert">
              The saved team may have changed. Load the latest team to review
              your draft against it before saving.
            </p>
          )}
          <button
            className="button"
            disabled={busy}
            onClick={() => void refresh()}
          >
            Review latest team
          </button>
          {merge && (
            <div className="roster-conflicts">
              <h4>Resolve changed assignments</h4>
              {merge.conflicts.map((conflict) => (
                <label key={conflict.id}>
                  Your draft: {describe(conflict.mine)}
                  <br />
                  Saved: {describe(conflict.saved)}
                  <select
                    aria-label={`Resolve assignment ${agentName(conflict.mine?.agentDeploymentId || conflict.saved?.agentDeploymentId || conflict.id)}`}
                    value={merge.choices[conflict.id] || ""}
                    onChange={(event) =>
                      setMerge({
                        ...merge,
                        choices: {
                          ...merge.choices,
                          [conflict.id]: event.target.value as "mine" | "saved",
                        },
                      })
                    }
                  >
                    <option value="">Choose a version</option>
                    <option value="mine">Keep my draft</option>
                    <option value="saved">Keep saved assignment</option>
                  </select>
                </label>
              ))}
              {merge.revision !== team.deployment.revision && (
                <p role="alert">
                  The team changed again. Review the latest team before
                  resolving these choices.
                </p>
              )}
              <button
                className="button"
                disabled={
                  busy ||
                  merge.revision !== team.deployment.revision ||
                  merge.conflicts.some((item) => !merge.choices[item.id])
                }
                onClick={() => {
                  const items = new Map(
                    merge.roster.map((item) => [item.id, item]),
                  );
                  for (const conflict of merge.conflicts) {
                    const chosen =
                      merge.choices[conflict.id] === "mine"
                        ? conflict.mine
                        : conflict.saved;
                    if (chosen) items.set(conflict.id, chosen);
                    else items.delete(conflict.id);
                  }
                  setDraft({
                    ...draft,
                    revision: merge.revision,
                    base: merge.base,
                    roster: [...items.values()],
                  });
                  setMerge(null);
                  setReviewed(false);
                  report(
                    "Conflicts resolved in your draft. Review the changes before saving.",
                  );
                }}
              >
                Use these choices
              </button>
            </div>
          )}
          <fieldset disabled={busy || !allowed || !!merge}>
            <legend className="sr-only">Draft members</legend>
            {draft.roster.map((member, index) => {
              const label = `member ${index + 1}`;
              const eligible = agents.filter((agent) =>
                qualifies(agent, member),
              );
              return (
                <section
                  className="team-role"
                  key={member.id}
                  aria-label={`Edit ${label}`}
                >
                  <h4>
                    {member.displayName || agentName(member.agentDeploymentId)}
                  </h4>
                  {member.purpose && <p>{member.purpose}</p>}
                  {!!member.requiredSkillIds?.length && (
                    <p>Required skills: {member.requiredSkillIds.join(", ")}</p>
                  )}
                  <div className="roster-assignment">
                    <label>
                      Agent for {label}
                      <select
                        aria-label={`Agent for ${label}`}
                        value={member.agentDeploymentId}
                        onChange={(event) =>
                          changeRoster(
                            draft.roster.map((item) =>
                              item.id === member.id
                                ? {
                                    ...item,
                                    agentDeploymentId: event.target.value,
                                  }
                                : item,
                            ),
                          )
                        }
                      >
                        {!eligible.some(
                          (agent) =>
                            agent.deployment.id === member.agentDeploymentId,
                        ) && (
                          <option value={member.agentDeploymentId}>
                            {agentName(member.agentDeploymentId)} (unavailable
                            or no longer eligible)
                          </option>
                        )}
                        {eligible
                          .filter(
                            (agent) =>
                              agent.deployment.id ===
                                member.agentDeploymentId ||
                              !draft.roster.some(
                                (item) =>
                                  item.agentDeploymentId ===
                                  agent.deployment.id,
                              ),
                          )
                          .map((agent) => (
                            <option
                              key={agent.deployment.id}
                              value={agent.deployment.id}
                            >
                              {agentName(agent.deployment.id)} ·{" "}
                              {agent.deployment.rolloutStatus}
                            </option>
                          ))}
                      </select>
                    </label>
                    <label>
                      Name in team for {label}
                      <input
                        value={member.displayName || ""}
                        maxLength={100}
                        placeholder="Optional name within this team"
                        onChange={(event) =>
                          changeRoster(
                            draft.roster.map((item) =>
                              item.id === member.id
                                ? { ...item, displayName: event.target.value }
                                : item,
                            ),
                          )
                        }
                      />
                    </label>
                    <button
                      className="button"
                      type="button"
                      onClick={() =>
                        changeRoster(
                          draft.roster.filter((item) => item.id !== member.id),
                        )
                      }
                    >
                      Remove {agentName(member.agentDeploymentId)}
                    </button>
                  </div>
                </section>
              );
            })}
            <button
              className="button"
              type="button"
              disabled={!available.length}
              onClick={() =>
                changeRoster([
                  ...draft.roster,
                  {
                    id: crypto.randomUUID(),
                    agentDeploymentId: available[0].deployment.id,
                  },
                ])
              }
            >
              Add member
            </button>
            {!available.length && (
              <p className="muted">
                No other eligible agents are available for this team.
              </p>
            )}
            {!!problems.length && (
              <ul className="error-text" aria-label="Roster requirements">
                {problems.map((problem) => (
                  <li key={problem}>{problem}</li>
                ))}
              </ul>
            )}
            <h4>Changes to review</h4>
            {differences.length ? (
              <ul>
                {differences.map((id) => (
                  <li key={id}>
                    <div>
                      Before:{" "}
                      {describe(draft.base.find((item) => item.id === id))}
                    </div>
                    <div>
                      After:{" "}
                      {describe(draft.roster.find((item) => item.id === id))}
                    </div>
                  </li>
                ))}
              </ul>
            ) : (
              <p className="muted">No membership changes yet.</p>
            )}
            <label htmlFor={labelID}>Reason for roster change</label>
            <textarea
              id={labelID}
              rows={3}
              maxLength={1000}
              value={draft.reason}
              onChange={(event) =>
                setDraft({ ...draft, reason: event.target.value })
              }
            />
            <label className="installation-consent">
              <input
                type="checkbox"
                checked={reviewed}
                onChange={(event) => setReviewed(event.target.checked)}
              />
              I reviewed the members and changes above.
            </label>
            <button
              className="button primary"
              disabled={
                !changed ||
                !draft.reason.trim() ||
                !reviewed ||
                outdated ||
                uncertain ||
                !!problems.length
              }
              onClick={() => void save()}
            >
              {busy ? "Saving roster…" : "Save roster"}
            </button>
          </fieldset>
          <button
            className="button roster-close"
            disabled={busy}
            onClick={() => {
              setOpen(false);
              requestAnimationFrame(() => toggle.current?.focus());
            }}
          >
            Close editor
          </button>
        </div>
      )}
    </section>
  );
}
