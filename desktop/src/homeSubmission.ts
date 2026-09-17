import { scope } from "./api";
export type HomeDraft = {
  version: 1;
  prompt: string;
  mode: "agent" | "team" | "work";
  agentID: string;
};
export type HomeSubmission = {
  version: 1;
  key: string;
  draft: HomeDraft;
  path: string;
  body: Record<string, unknown>;
};
export const homeSubmissionKey = "openseal.home-submission";
function request(draft: HomeDraft) {
  return draft.mode === "work"
    ? {
        path: "/agent-runs",
        body: {
          scope,
          kind: "agent_work",
          owner: { type: "agent", id: draft.agentID },
          assignedAgentId: draft.agentID,
          goal: draft.prompt.trim(),
          source: "manual",
          actor: { type: "user", id: "local-operator" },
          visibility: "scope",
        },
      }
    : {
        path: "/authoring/workforce/change-sets",
        body: {
          scope,
          prompt:
            draft.mode === "team"
              ? `Create one team.\n\n${draft.prompt.trim()}`
              : draft.prompt.trim(),
          catalog: {},
          actor: { type: "user", id: "local-operator" },
        },
      };
}
export function newHomeSubmission(draft: HomeDraft): HomeSubmission {
  return { version: 1, key: crypto.randomUUID(), draft, ...request(draft) };
}
export function readHomeSubmission(): {
  pending: HomeSubmission | null;
  error: string;
} {
  try {
    const raw = localStorage.getItem(homeSubmissionKey);
    if (!raw) return { pending: null, error: "" };
    const value = JSON.parse(raw),
      draft = value?.draft;
    if (
      value?.version !== 1 ||
      typeof value.key !== "string" ||
      !/^[\da-f]{8}(-[\da-f]{4}){3}-[\da-f]{12}$/i.test(value.key) ||
      draft?.version !== 1 ||
      !["agent", "team", "work"].includes(draft.mode) ||
      typeof draft.prompt !== "string" ||
      !draft.prompt.trim() ||
      draft.prompt.length > 16000 ||
      typeof draft.agentID !== "string" ||
      (draft.mode === "work" && !draft.agentID.trim())
    )
      throw new Error("Invalid saved request");
    const expected = request(draft);
    if (
      value.path !== expected.path ||
      JSON.stringify(value.body) !== JSON.stringify(expected.body)
    )
      throw new Error("Saved request does not match this workspace");
    return { pending: value, error: "" };
  } catch {
    return {
      pending: null,
      error:
        "Saved request recovery could not be read. New requests are paused to avoid losing an earlier submission. Restore local storage access, then retry recovery.",
    };
  }
}
export function persistHomeSubmission(pending: HomeSubmission) {
  const previous = localStorage.getItem(homeSubmissionKey);
  if (previous && previous !== JSON.stringify(pending))
    throw new Error(
      "Another saved request needs recovery. Reload the app to inspect it before sending.",
    );
  localStorage.setItem(homeSubmissionKey, JSON.stringify(pending));
}
export function finishHomeSubmission(
  pending: HomeSubmission,
  proposalId?: string,
) {
  if (localStorage.getItem(homeSubmissionKey) !== JSON.stringify(pending))
    throw new Error(
      "The saved request changed. Reload the app to inspect recovery.",
    );
  if (proposalId) localStorage.setItem("openseal.proposal", proposalId);
  // Clear the editor durably before releasing the request identity. If storage
  // fails at any step, the same idempotent request remains recoverable.
  localStorage.setItem(
    "openseal.draft",
    JSON.stringify({ ...pending.draft, prompt: "" }),
  );
  localStorage.removeItem("openseal.prompt");
  localStorage.removeItem(homeSubmissionKey);
}
