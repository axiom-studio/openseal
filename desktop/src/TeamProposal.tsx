import type { Proposal } from "./api";

type Candidate = NonNullable<NonNullable<Proposal["result"]>["candidate"]>;
const words = (value?: string) =>
  value ? value.replaceAll("_", " ") : "Not specified";
const limit = (value?: number) => (value ? String(value) : "Not specified");

export function teamReviewIssue(candidate?: Candidate) {
  const team = candidate?.team;
  if (!team) return "";
  if (!team.roles?.length || !team.approvals?.maximumRisk)
    return "The team proposal is missing roles or approval boundaries. Review a complete proposal before approving installation.";
  if (
    candidate.assignments?.some(
      (assignment) =>
        !team.roles.some((role) => role.id === assignment.roleId) ||
        !candidate.agents?.some(
          (agent) => agent.id === assignment.agentDefinitionId,
        ),
    )
  )
    return "Some team assignments do not match the proposed roles and agents. Resolve the proposal before approving installation.";
  return "";
}

export default function TeamProposal({
  candidate,
  activating = false,
}: {
  candidate: Candidate;
  activating?: boolean;
}) {
  const team = candidate.team!;
  const assignments = candidate.assignments || [];
  const roleName = (id: string) =>
    team.roles?.find((role) => role.id === id)?.displayName || id;
  const delegation = team.delegation;
  const context = team.sharedContext;
  const coordination = team.coordination;
  return (
    <section className="proposal-team" aria-label="Proposed team">
      <h3>{team.displayName || "Proposed team"}</h3>
      <p className="preserve-lines">{team.purpose}</p>
      <p className="muted">
        {activating
          ? "This proposal activates the existing team and its agents. Review their roles, permissions, and instructions before enabling them."
          : "This proposal creates the team and its agents together. Review each agent’s instructions below."}
      </p>
      <h4>Roles and assignments</h4>
      {(team.roles || []).map((role) => (
        <section
          className="team-role"
          key={role.id}
          aria-label={`Proposed role: ${role.displayName}`}
        >
          <h5>{role.displayName}</h5>
          <p>{role.purpose}</p>
          <p className="muted">
            Minimum {role.minimumMembers || 0} ·{" "}
            {role.maximumMembers
              ? `Maximum ${role.maximumMembers}`
              : "No maximum specified"}
          </p>
          <ul>
            {assignments
              .filter((assignment) => assignment.roleId === role.id)
              .map((assignment) => {
                const agent = candidate.agents?.find(
                  (item) => item.id === assignment.agentDefinitionId,
                );
                return (
                  <li key={assignment.id}>
                    {agent?.displayName || assignment.agentDefinitionId}
                    {assignment.displayName &&
                    assignment.displayName !== agent?.displayName
                      ? ` · ${assignment.displayName}`
                      : ""}
                  </li>
                );
              })}
          </ul>
          {!assignments.some((assignment) => assignment.roleId === role.id) && (
            <p className="muted">No proposed member.</p>
          )}
          {!!role.requiredSkillIds?.length && (
            <p>Required skills: {role.requiredSkillIds.join(", ")}</p>
          )}
          {!!role.requiredDefinitionIds?.length && (
            <p>
              Required agent definitions:{" "}
              {role.requiredDefinitionIds
                .map(
                  (id) =>
                    candidate.agents?.find((agent) => agent.id === id)
                      ?.displayName || id,
                )
                .join(", ")}
            </p>
          )}
          <p className="muted">
            Channel participation: {words(role.channelParticipation)}
          </p>
          {!!role.skillGrants?.length && (
            <details>
              <summary>Role skill permissions</summary>
              {role.skillGrants.map((grant, index) => (
                <div key={index} className="proposal-notes">
                  <strong>
                    {grant.skillId} · {grant.skillVersion}
                  </strong>
                  <p>Maximum risk: {words(grant.maximumRisk)}</p>
                  <p>
                    Allowed actions:{" "}
                    {grant.allowedActions?.length
                      ? grant.allowedActions.join(", ")
                      : "None listed"}
                  </p>
                  <p>
                    Skill instructions:{" "}
                    {grant.enablePrompt ? "Enabled" : "Disabled"}
                  </p>
                </div>
              ))}
            </details>
          )}
        </section>
      ))}
      <h4>Approval boundaries</h4>
      <p>Maximum team risk: {words(team.approvals?.maximumRisk)}</p>
      <p>
        Approver roles:{" "}
        {team.approvals?.approverRoleIds?.length
          ? team.approvals.approverRoleIds.map(roleName).join(", ")
          : "None listed"}
      </p>
      <p>
        Other approvers:{" "}
        {team.approvals?.approverPrincipals?.join(", ") || "None listed"}
      </p>
      <p className="muted">
        These are the team’s proposed boundaries. Workspace policies and each
        agent’s permissions also apply.
      </p>
      <details>
        <summary>Delegation and shared context</summary>
        <dl className="installation-summary team-policy">
          <div>
            <dt>Peer delegation</dt>
            <dd>
              {delegation?.allowPeerDelegation ? "Allowed" : "Not allowed"}
            </dd>
          </div>
          <div>
            <dt>Acceptance required</dt>
            <dd>{delegation?.requireAcceptance ? "Yes" : "No"}</dd>
          </div>
          <div>
            <dt>Completion review required</dt>
            <dd>{delegation?.requireCompletionReview ? "Yes" : "No"}</dd>
          </div>
          <div>
            <dt>Completion review quorum</dt>
            <dd>{limit(delegation?.completionReviewQuorum)}</dd>
          </div>
          <div>
            <dt>Escalate disagreements</dt>
            <dd>{delegation?.escalateOnDisagreement ? "Yes" : "No"}</dd>
          </div>
          <div>
            <dt>Maximum delegation depth</dt>
            <dd>{limit(delegation?.maximumDepth)}</dd>
          </div>
          <div>
            <dt>Maximum concurrent delegations</dt>
            <dd>{limit(delegation?.maximumConcurrent)}</dd>
          </div>
          <div>
            <dt>Members may read shared context</dt>
            <dd>{context?.allowMemberRead ? "Yes" : "No"}</dd>
          </div>
          <div>
            <dt>Members may write shared context</dt>
            <dd>{context?.allowMemberWrite ? "Yes" : "No"}</dd>
          </div>
          <div>
            <dt>Shared context size limit</dt>
            <dd>
              {context?.maximumBytes
                ? `${context.maximumBytes.toLocaleString()} bytes`
                : "Not specified"}
            </dd>
          </div>
          <div>
            <dt>Shared context retention</dt>
            <dd>
              {context?.retention
                ? `${context.retention / 1e9} seconds`
                : "Not specified"}
            </dd>
          </div>
        </dl>
      </details>
      <details>
        <summary>Channel coordination</summary>
        <dl className="installation-summary team-policy">
          <div>
            <dt>Maximum speakers per round</dt>
            <dd>{limit(coordination?.maximumSpeakersPerRound)}</dd>
          </div>
          <div>
            <dt>Quiet by default</dt>
            <dd>{coordination?.quietByDefault ? "Yes" : "No"}</dd>
          </div>
          <div>
            <dt>Require role relevance</dt>
            <dd>{coordination?.requireRoleRelevance ? "Yes" : "No"}</dd>
          </div>
          <div>
            <dt>Suppress duplicate content</dt>
            <dd>{coordination?.suppressDuplicateContent ? "Yes" : "No"}</dd>
          </div>
        </dl>
      </details>
      {!!team.operatingPrinciples?.length && (
        <>
          <h4>Operating principles</h4>
          <ul>
            {team.operatingPrinciples.map((principle, index) => (
              <li key={index}>{principle}</li>
            ))}
          </ul>
        </>
      )}
      {!!team.objectiveTemplates?.length && (
        <>
          <h4>Team objectives</h4>
          {team.objectiveTemplates.map((objective) => (
            <div key={objective.id}>
              <strong>{objective.title}</strong>
              <p>{objective.goal}</p>
              {Object.entries(objective.successCriteria || {}).map(
                ([key, value]) => (
                  <p key={key}>
                    {words(key)}:{" "}
                    {typeof value === "string" ? value : JSON.stringify(value)}
                  </p>
                ),
              )}
              {Object.entries(objective.constraints || {}).map(
                ([key, value]) => (
                  <p key={key}>
                    {words(key)}:{" "}
                    {typeof value === "string" ? value : JSON.stringify(value)}
                  </p>
                ),
              )}
            </div>
          ))}
        </>
      )}
    </section>
  );
}
