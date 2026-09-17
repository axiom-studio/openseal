import { useEffect, useId, useRef, useState } from "react";
import {
  api,
  ApiError,
  message,
  scope,
  scoped,
  supports,
  type Agent,
  type Capabilities,
  type Run,
} from "./api";
import type { Team } from "./TeamBrowser";

type Draft = { goal: string; lead: string; pending?: string };
export type ChannelWorkDraft = {
  teamId: string;
  channel: string;
  sequence: number;
  content: string;
  returnToMessage: () => void;
};
function restore(key: string): Draft {
  try {
    const value = JSON.parse(localStorage.getItem(key) || "null");
    if (
      value &&
      typeof value.goal === "string" &&
      value.goal.length <= 16000 &&
      typeof value.lead === "string" &&
      (!value.pending || typeof value.pending === "string")
    )
      return value;
  } catch {
    /* A new draft remains usable when storage is unavailable. */
  }
  return { goal: "", lead: "" };
}
export default function TeamWork({
  team,
  agents,
  capabilities,
  onOpen,
  channelDraft,
  onChannelDraftHandled,
}: {
  team: Team;
  agents: Agent[];
  capabilities: Capabilities | null;
  onOpen: (run: Run) => void;
  channelDraft?: ChannelWorkDraft | null;
  onChannelDraftHandled?: () => void;
}) {
  const key = `openseal.team-work.${team.deployment.id}`;
  const [draft, setDraft] = useState(() => restore(key));
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [storageError, setStorageError] = useState(false);
  const [runs, setRuns] = useState<Run[]>([]);
  const [offset, setOffset] = useState(0);
  const [more, setMore] = useState(false);
  const [loading, setLoading] = useState(true);
  const [listError, setListError] = useState("");
  const [refresh, setRefresh] = useState(0);
  const alive = useRef(true),
    lock = useRef(false);
  const heading = useRef<HTMLHeadingElement>(null);
  const feedback = useRef<HTMLParagraphElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const id = useId();
  const handoffHeading = useRef<HTMLHeadingElement>(null);
  const goalInput = useRef<HTMLTextAreaElement>(null);
  const focusGoal = useRef(false);
  const canCreate = supports(capabilities, "agent-runs", "create-team");
  const canList = supports(capabilities, "agent-runs", "list");
  const members = team.deployment.roster.map((member) => ({
    member,
    agent: agents.find((a) => a.deployment.id === member.agentDeploymentId),
  }));
  const ready =
    team.deployment.status === "active" &&
    !team.deployment.activation &&
    members.length > 0 &&
    members.every(({ agent }) => agent?.deployment.rolloutStatus === "active");
  const leadAvailable = members.some(
    ({ agent }) =>
      agent?.deployment.id === draft.lead &&
      agent.deployment.rolloutStatus === "active",
  );
  useEffect(() => {
    if (channelDraft) setOpen(true);
  }, [channelDraft]);
  useEffect(() => {
    if (open && channelDraft) handoffHeading.current?.focus();
  }, [open, channelDraft]);
  useEffect(() => {
    if (focusGoal.current && !channelDraft) {
      focusGoal.current = false;
      goalInput.current?.focus();
    }
  }, [channelDraft]);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);
  useEffect(() => {
    try {
      draft.goal || draft.lead || draft.pending
        ? localStorage.setItem(key, JSON.stringify(draft))
        : localStorage.removeItem(key);
      setStorageError(false);
    } catch {
      setStorageError(true);
    }
  }, [key, draft]);
  useEffect(() => {
    if (!open || !canList) return;
    let canceled = false;
    let inflight = false;
    async function load() {
      if (inflight) return;
      inflight = true;
      try {
        const result = await api<Run[] | null>(
          scoped(
            `/agent-runs?ownerType=team&ownerId=${encodeURIComponent(team.deployment.id)}&limit=21&offset=${offset}&order=created_desc`,
          ),
        );
        if (canceled) return;
        if (result !== null && !Array.isArray(result))
          throw new Error("Unreadable team work list.");
        setRuns((result || []).slice(0, 20));
        setMore((result || []).length > 20);
        setListError("");
      } catch (e) {
        if (!canceled) setListError(message(e));
      } finally {
        inflight = false;
        if (!canceled) setLoading(false);
      }
    }
    setLoading(true);
    void load();
    const timer = setInterval(() => void load(), 3000);
    return () => {
      canceled = true;
      clearInterval(timer);
    };
  }, [open, canList, team.deployment.id, offset, refresh]);
  async function start() {
    if (
      lock.current ||
      !canCreate ||
      (!!channelDraft && !draft.pending) ||
      (!draft.pending && (!ready || !leadAvailable)) ||
      !draft.goal.trim() ||
      !draft.lead
    )
      return;
    const submitted = {
      ...draft,
      pending: draft.pending || crypto.randomUUID(),
    };
    // Persist delivery identity before sending, so reload can never create a second task.
    try {
      localStorage.setItem(key, JSON.stringify(submitted));
    } catch {
      setError(
        "This device could not save the work request. Nothing was sent. Keep your draft open and enable local storage before trying again.",
      );
      return;
    }
    lock.current = true;
    setBusy(true);
    setDraft(submitted);
    setError("");
    try {
      const result = await api<{ run: Run }>("/agent-runs", {
        method: "POST",
        key: submitted.pending,
        body: {
          scope,
          kind: "agent_work",
          owner: { type: "team", id: team.deployment.id },
          assignedAgentId: submitted.lead,
          goal: submitted.goal.trim(),
          source: "manual",
          actor: { type: "user", id: "local-operator" },
          visibility: "scope",
        },
      });
      if (!alive.current) return;
      if (
        !result.run?.id ||
        result.run.owner.type !== "team" ||
        result.run.owner.id !== team.deployment.id ||
        result.run.assignedAgentId !== submitted.lead ||
        result.run.goal !== submitted.goal.trim()
      )
        throw new Error("The response did not match this work request.");
      setDraft({ goal: "", lead: "" });
      setRefresh((value) => value + 1);
      setOffset(0);
      onOpen(result.run);
    } catch (e) {
      if (alive.current) {
        if (
          e instanceof ApiError &&
          [400, 401, 403, 404, 422].includes(e.status)
        ) {
          setDraft({ goal: submitted.goal, lead: submitted.lead });
          setError(`Work was not started. ${message(e)} Your draft is kept.`);
        } else
          setError(
            `Could not confirm that work started. ${message(e)} Your request is kept. Retry uses the same request and cannot create a duplicate.`,
          );
        requestAnimationFrame(() => feedback.current?.focus());
      }
    } finally {
      lock.current = false;
      if (alive.current) setBusy(false);
    }
  }
  if (!canCreate && !canList) return null;
  return (
    <section className="team-work" aria-label="Team work">
      <button
        className="button"
        ref={trigger}
        aria-expanded={open}
        onClick={() => {
          setOpen((value) => !value);
          if (!open) requestAnimationFrame(() => heading.current?.focus());
        }}
      >
        {" "}
        {open ? "Hide team work" : "Team work"}
      </button>
      {open && (
        <div>
          <h2 ref={heading} tabIndex={-1}>
            Work with this team
          </h2>
          <p>
            Choose a lead agent to begin. It can delegate to the team’s other
            agents under the team’s saved policies. Follow each run below.
          </p>
          {canCreate && (
            <form
              onSubmit={(event) => {
                event.preventDefault();
                void start();
              }}
            >
              {channelDraft && (
                <section
                  className="channel-work-handoff"
                  aria-label="Review channel message"
                >
                  <h3 ref={handoffHeading} tabIndex={-1}>
                    Use this message as team work
                  </h3>
                  <p className="muted">
                    {channelDraft.channel} · Message {channelDraft.sequence}
                  </p>
                  <p className="channel-message-content">
                    {channelDraft.content}
                  </p>
                  <p className="inline-help">
                    Only this message’s text is copied. Review the goal and
                    choose a lead before starting; the rest of the conversation
                    and its attachments are not included.
                  </p>
                  {draft.pending || busy ? (
                    <p className="inline-help">
                      Confirm the current work request before replacing its
                      draft. This message has not started any work.
                    </p>
                  ) : draft.goal.trim() &&
                    draft.goal !== channelDraft.content ? (
                    <p className="inline-help">
                      You already have an unfinished team-work draft. Replacing
                      it will keep your selected lead agent.
                    </p>
                  ) : null}
                  <div className="inspector-actions">
                    <button
                      type="button"
                      className="button"
                      disabled={
                        busy ||
                        !!draft.pending ||
                        channelDraft.content.length > 16000
                      }
                      onClick={() => {
                        if (lock.current || draft.pending) return;
                        setDraft({
                          goal: channelDraft.content,
                          lead: draft.lead,
                        });
                        setError("");
                        focusGoal.current = true;
                        onChannelDraftHandled?.();
                      }}
                    >
                      {draft.goal.trim() && draft.goal !== channelDraft.content
                        ? "Replace draft and review"
                        : "Use message in draft"}
                    </button>
                    {channelDraft.content.length > 16000 && (
                      <button
                        type="button"
                        className="button"
                        disabled={busy || !!draft.pending}
                        onClick={() => {
                          if (lock.current || draft.pending) return;
                          focusGoal.current = true;
                          onChannelDraftHandled?.();
                        }}
                      >
                        Write a shorter goal
                      </button>
                    )}
                    <button
                      type="button"
                      className="button"
                      onClick={() => {
                        channelDraft.returnToMessage();
                        onChannelDraftHandled?.();
                      }}
                    >
                      Keep draft and return to message
                    </button>
                  </div>
                  {channelDraft.content.length > 16000 && (
                    <p className="inline-help">
                      This message exceeds the 16,000-character work limit.
                      Choose Write a shorter goal to continue in the editor.
                      Your current draft and lead will be kept.
                    </p>
                  )}
                </section>
              )}
              {!ready && (
                <p className="inline-help">
                  Starting new work requires an active team and active agents in
                  every assigned role.
                </p>
              )}
              {storageError && (
                <p role="alert">
                  Your draft could not be saved on this device. Keep this editor
                  open.
                </p>
              )}
              {draft.pending && (
                <p className="inline-help">
                  This request is awaiting confirmation. Retry checks the same
                  saved request, including after reopening the app.
                </p>
              )}
              <fieldset disabled={busy || !!draft.pending}>
                <label htmlFor={`${id}-lead`}>Lead agent</label>
                <select
                  id={`${id}-lead`}
                  value={draft.lead}
                  onChange={(event) =>
                    setDraft({ ...draft, lead: event.target.value })
                  }
                >
                  <option value="">Choose a lead agent</option>
                  {!!draft.lead && !leadAvailable && (
                    <option value={draft.lead} disabled>
                      Previously selected agent (unavailable)
                    </option>
                  )}
                  {members
                    .filter(
                      ({ agent }) =>
                        agent?.deployment.rolloutStatus === "active",
                    )
                    .map(({ member, agent }) => (
                      <option key={member.id} value={agent!.deployment.id}>
                        {member.displayName ||
                          agent!.deployment.displayName ||
                          agent!.definition.displayName}
                      </option>
                    ))}
                </select>
                <label htmlFor={`${id}-goal`}>
                  What should the team accomplish?
                </label>
                <textarea
                  ref={goalInput}
                  id={`${id}-goal`}
                  rows={4}
                  maxLength={16000}
                  value={draft.goal}
                  onChange={(event) =>
                    setDraft({ ...draft, goal: event.target.value })
                  }
                />
              </fieldset>
              <p className="muted">
                Progress and results are saved. Each run has a bounded budget;
                delegation follows the team’s acceptance and review rules.
              </p>
              {error && (
                <p role="alert" tabIndex={-1} ref={feedback}>
                  {error}
                </p>
              )}
              <button
                className="button primary"
                disabled={
                  busy ||
                  (!!channelDraft && !draft.pending) ||
                  !draft.goal.trim() ||
                  !draft.lead ||
                  (!draft.pending && (!ready || !leadAvailable))
                }
              >
                {busy
                  ? "Starting work…"
                  : draft.pending
                    ? "Retry starting work"
                    : "Start team work"}
              </button>
            </form>
          )}
          <h3>Team runs</h3>
          {canList ? (
            <>
              {listError && (
                <p role="alert">
                  Could not refresh team work. {listError} Previously loaded
                  runs may be out of date.{" "}
                  <button
                    className="button"
                    onClick={() => setRefresh((value) => value + 1)}
                  >
                    Refresh team work
                  </button>
                </p>
              )}
              {loading ? (
                <p role="status">Loading team work…</p>
              ) : !runs.length && !listError ? (
                <p className="muted">No team work on this page.</p>
              ) : (
                <ul className="team-run-list">
                  {runs.map((run) => (
                    <li key={run.id}>
                      <button className="button" onClick={() => onOpen(run)}>
                        {run.goal}
                      </button>
                      <span className="muted">
                        {run.parentRunId ? "Delegated run · " : ""}
                        {run.status.replaceAll("_", " ")}
                      </span>
                    </li>
                  ))}
                </ul>
              )}
              <div className="team-work-pages">
                <button
                  className="button"
                  disabled={offset === 0 || loading}
                  onClick={() => setOffset((value) => Math.max(0, value - 20))}
                >
                  Newer team runs
                </button>
                <button
                  className="button"
                  disabled={!more || loading}
                  onClick={() => setOffset((value) => value + 20)}
                >
                  Older team runs
                </button>
              </div>
            </>
          ) : (
            <p className="inline-help">
              Work history is unavailable in this workspace.
            </p>
          )}
        </div>
      )}
    </section>
  );
}
