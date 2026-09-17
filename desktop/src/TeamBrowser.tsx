import { useEffect, useRef, useState } from "react";
import { Search, Workflow } from "lucide-react";
import {
  api,
  message,
  scoped,
  type Agent,
  type Capabilities,
  type Proposal,
  type Run,
  type RunInspection,
} from "./api";
import TeamControls from "./TeamControls";
import TeamRoster from "./TeamRoster";
import TeamWork, { type ChannelWorkDraft } from "./TeamWork";
import TeamChannels from "./TeamChannels";

export type Team = {
  deployment: {
    id: string;
    activeVersion: string;
    status: string;
    revision: number;
    definitionId?: string;
    scope?: { kind: string; id: string };
    activation?: { changeSetId: string };
    roster: {
      id: string;
      roleId: string;
      agentDeploymentId: string;
      displayName?: string;
    }[];
  };
  definition: {
    displayName: string;
    purpose: string;
    operatingPrinciples?: string[];
    roles: {
      id: string;
      displayName: string;
      purpose: string;
      minimumMembers?: number;
      maximumMembers?: number;
      requiredSkillIds?: string[];
      requiredDefinitionIds?: string[];
    }[];
  };
};

export default function TeamBrowser({
  available,
  refreshToken,
  agents,
  onOpenAgent,
  capabilities,
  onOpenProposal,
  onOpenRun,
}: {
  available: boolean;
  refreshToken: number;
  agents: Agent[];
  onOpenAgent: (agent: Agent) => void;
  capabilities: Capabilities | null;
  onOpenProposal: (proposal: Proposal) => void;
  onOpenRun: (run: Run, detail?: RunInspection) => void;
}) {
  const [teams, setTeams] = useState<Team[]>([]);
  const [query, setQuery] = useState("");
  const [filter, setFilter] = useState("all");
  const [loaded, setLoaded] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);
  const [selected, setSelected] = useState("");
  const [channelDraft, setChannelDraft] = useState<ChannelWorkDraft | null>(
    null,
  );
  useEffect(() => {
    setChannelDraft(null);
  }, [selected]);
  const feedback = useRef<HTMLParagraphElement>(null);
  const focusAfterLoad = useRef(false);
  useEffect(() => {
    if (!available) {
      setTeams([]);
      setLoaded(false);
      setSelected("");
      return;
    }
    let canceled = false;
    setBusy(true);
    setError("");
    api<{ items: Team[] | null }>(scoped("/team-deployments"))
      .then((result) => {
        if (canceled) return;
        if (!result || (result.items !== null && !Array.isArray(result.items)))
          throw new Error("OpenSeal returned an unreadable team list.");
        const items = result.items || [];
        setTeams((previous) =>
          items
            .map((item) => {
              const known = previous.find(
                (team) => team.deployment.id === item.deployment.id,
              );
              return known &&
                known.deployment.revision > item.deployment.revision
                ? known
                : item;
            })
            .slice()
            .sort(
              (a, b) =>
                a.definition.displayName.localeCompare(
                  b.definition.displayName,
                ) || a.deployment.id.localeCompare(b.deployment.id),
            ),
        );
        setSelected((id) =>
          items.some((team) => team.deployment.id === id) ? id : "",
        );
        setLoaded(true);
      })
      .catch((e) => {
        if (!canceled) setError(message(e));
      })
      .finally(() => {
        if (canceled) return;
        setBusy(false);
        if (focusAfterLoad.current) {
          focusAfterLoad.current = false;
          requestAnimationFrame(() => feedback.current?.focus());
        }
      });
    return () => {
      canceled = true;
    };
  }, [available, refreshToken, retry]);
  const visible = teams.filter(
    ({ deployment, definition }) =>
      deployment.id === selected ||
      ((filter === "all" || deployment.status === filter) &&
        [
          definition.displayName,
          definition.purpose,
          ...definition.roles.map((role) => role.displayName),
        ]
          .join(" ")
          .toLocaleLowerCase()
          .includes(query.trim().toLocaleLowerCase())),
  );
  if (!available)
    return (
      <p className="inline-help">
        Team browsing is unavailable in this workspace.
      </p>
    );
  return (
    <section aria-label="Workspace teams">
      <div className="list-toolbar">
        <label className="filter-search">
          <Search size={16} aria-hidden="true" />
          <span className="sr-only">Search teams</span>
          <input
            type="search"
            placeholder="Search names, purposes, or roles…"
            value={query}
            onChange={(e) => {
              setQuery(e.target.value);
              setSelected("");
            }}
          />
        </label>
        <label className="filter-select">
          <span className="sr-only">Team status</span>
          <select
            value={filter}
            onChange={(e) => {
              setFilter(e.target.value);
              setSelected("");
            }}
          >
            <option value="all">All teams</option>
            <option value="active">Active</option>
            <option value="draft">Draft</option>
            <option value="paused">Paused</option>
            <option value="archived">Archived</option>
          </select>
        </label>
        <button
          className="button"
          disabled={busy}
          onClick={() => {
            focusAfterLoad.current = true;
            setRetry((value) => value + 1);
          }}
        >
          Refresh teams
        </button>
      </div>
      {error && (
        <p className="error-text" role="alert">
          Could not refresh teams. {error}{" "}
          {loaded
            ? "Showing the last loaded team details."
            : "Use Refresh teams to try again."}
        </p>
      )}
      {!loaded && !error ? (
        <div
          className="skeleton-list"
          role="group"
          aria-busy="true"
          aria-label="Loading teams"
        >
          <span />
          <span />
          <span />
        </div>
      ) : loaded && !visible.length ? (
        <div className="empty">
          <Workflow size={27} strokeWidth={1.5} aria-hidden="true" />
          <h2>
            {query || filter !== "all"
              ? "No matching teams"
              : "Your teams will appear here"}
          </h2>
          <p>
            {query || filter !== "all"
              ? "Try another search or show all teams."
              : "Create a team to bring agents together around a shared goal. Review its roles and permissions before installing it."}
          </p>
          {(query || filter !== "all") && (
            <button
              className="button"
              onClick={() => {
                setQuery("");
                setFilter("all");
              }}
            >
              Clear filters
            </button>
          )}
        </div>
      ) : (
        <div className="record-list team-list">
          {visible.map(({ deployment, definition }) => (
            <div key={deployment.id}>
              <button
                className={`record-row ${selected === deployment.id ? "selected" : ""}`}
                aria-expanded={selected === deployment.id}
                aria-controls={`team-${deployment.id}`}
                onClick={() =>
                  setSelected(selected === deployment.id ? "" : deployment.id)
                }
              >
                <span className="avatar" aria-hidden="true">
                  <Workflow size={19} />
                </span>
                <span className="record-main">
                  <strong>{definition.displayName}</strong>
                  <span>{definition.purpose}</span>
                </span>
                <span className="status-pill">{deployment.status}</span>
              </button>
              {selected === deployment.id && (
                <section
                  id={`team-${deployment.id}`}
                  className="team-details"
                  aria-label={`${definition.displayName} details`}
                >
                  <p className="preserve-lines">{definition.purpose}</p>
                  <p className="muted">
                    Version {deployment.activeVersion} ·{" "}
                    {deployment.roster.length}{" "}
                    {deployment.roster.length === 1 ? "member" : "members"} ·
                    Revision {deployment.revision}
                  </p>
                  <p className="inline-help">
                    Team status describes its deployment. Each member’s
                    availability is shown separately.
                  </p>
                  {filter !== "all" && filter !== deployment.status && (
                    <p className="inline-help">
                      This selected team remains visible after its status
                      changed.
                    </p>
                  )}
                  <TeamControls
                    key={`controls:${deployment.id}`}
                    team={{ deployment, definition }}
                    capabilities={capabilities}
                    onOpenProposal={onOpenProposal}
                    onChange={(next) =>
                      setTeams((current) =>
                        current.map((item) =>
                          item.deployment.id === next.deployment.id &&
                          next.deployment.revision >= item.deployment.revision
                            ? next
                            : item,
                        ),
                      )
                    }
                  />
                  <TeamWork
                    key={`work:${deployment.id}`}
                    team={{ deployment, definition }}
                    agents={agents}
                    capabilities={capabilities}
                    onOpen={onOpenRun}
                    channelDraft={
                      channelDraft?.teamId === deployment.id
                        ? channelDraft
                        : null
                    }
                    onChannelDraftHandled={() => setChannelDraft(null)}
                  />
                  <TeamChannels
                    key={`channels:${deployment.id}`}
                    team={{ deployment, definition }}
                    agents={agents}
                    capabilities={capabilities}
                    onWorkDraft={setChannelDraft}
                    onOpenRun={onOpenRun}
                  />
                  <h2>Roles and members</h2>
                  <TeamRoster
                    key={`roster:${deployment.id}`}
                    team={{ deployment, definition }}
                    agents={agents}
                    capabilities={capabilities}
                    onChange={(next) =>
                      setTeams((current) =>
                        current.map((item) =>
                          item.deployment.id === next.deployment.id &&
                          next.deployment.revision >= item.deployment.revision
                            ? next
                            : item,
                        ),
                      )
                    }
                  />
                  <h3>Saved role assignments</h3>
                  {definition.roles.map((role) => {
                    const members = deployment.roster.filter(
                      (member) => member.roleId === role.id,
                    );
                    return (
                      <section
                        className="team-role"
                        key={role.id}
                        aria-label={role.displayName}
                      >
                        <h3>{role.displayName}</h3>
                        <p>{role.purpose}</p>
                        <p className="muted">
                          Minimum {role.minimumMembers || 0} ·{" "}
                          {role.maximumMembers
                            ? `Maximum ${role.maximumMembers}`
                            : "No maximum specified"}
                        </p>
                        {!!role.requiredSkillIds?.length && (
                          <p>
                            Required skills: {role.requiredSkillIds.join(", ")}
                          </p>
                        )}
                        {!members.length ? (
                          <p className="muted">No member assigned.</p>
                        ) : (
                          <ul className="team-members">
                            {members.map((member) => {
                              const agent = agents.find(
                                (item) =>
                                  item.deployment.id ===
                                  member.agentDeploymentId,
                              );
                              const name =
                                member.displayName ||
                                agent?.deployment.displayName ||
                                agent?.definition.displayName ||
                                member.agentDeploymentId;
                              return (
                                <li key={member.id}>
                                  {agent ? (
                                    <>
                                      <button
                                        className="button"
                                        onClick={() => onOpenAgent(agent)}
                                      >
                                        View {name}
                                      </button>
                                      <span className="muted">
                                        {agent.deployment.rolloutStatus}
                                      </span>
                                    </>
                                  ) : (
                                    <>
                                      <span>{name}</span>
                                      <span className="muted">
                                        Agent details unavailable
                                      </span>
                                    </>
                                  )}
                                </li>
                              );
                            })}
                          </ul>
                        )}
                      </section>
                    );
                  })}
                  {!!definition.operatingPrinciples?.length && (
                    <>
                      <h2>Operating principles</h2>
                      <ul>
                        {definition.operatingPrinciples.map(
                          (principle, index) => (
                            <li key={index}>{principle}</li>
                          ),
                        )}
                      </ul>
                    </>
                  )}
                </section>
              )}
            </div>
          ))}
        </div>
      )}
      <p className="muted" ref={feedback} tabIndex={-1} role="status">
        {busy
          ? "Refreshing teams…"
          : error
            ? "Team details may be out of date."
            : `${visible.length} ${visible.length === 1 ? "team" : "teams"}`}
      </p>
    </section>
  );
}
