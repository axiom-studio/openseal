import { invoke, isTauri } from "@tauri-apps/api/core";
export const scope = { kind: "local", id: "default" };
export const scoped = (path: string) =>
  `${path}${path.includes("?") ? "&" : "?"}scopeKind=local&scopeId=default`;
export type DesktopStatus = {
  state: "starting" | "ready" | "error";
  message: string;
  workspace: string;
};
export type Capability = {
  id: string;
  available: boolean;
  operations: string[];
  context?: {
    changeSetId: string;
    revision: number;
    credentialBindings?: {
      reference: { kind: string; id: string };
      displayName: string;
      bindingKeys?: string[];
    }[];
    bindingConfigurationFields?: {
      catalogSkillId: string;
      key: string;
      type: string;
      required?: boolean;
      prompt: string;
      options: {
        label: string;
        description?: string;
        value: {
          string?: string;
          integer?: number;
          number?: number;
          boolean?: boolean;
        };
      }[];
    }[];
    eligibleApprovalRequirements?: {
      evaluationId: string;
      policyId: string;
      role: string;
    }[];
  };
};
export type Capabilities = { capabilities: Capability[] };
export type Agent = {
  deployment: {
    id: string;
    definitionId?: string;
    displayName?: string;
    rolloutStatus: string;
    activeVersion: string;
    revision: number;
  };
  definition: {
    id?: string;
    displayName: string;
    purpose: string;
    systemPrompt: string;
    operatingPrinciples?: string[];
    skillRequirements?: { skillId: string }[];
  };
};
export type Run = {
  id: string;
  parentRunId?: string;
  kind?: string;
  goal: string;
  status: string;
  assignedAgentId?: string;
  owner: { type: string; id: string };
  updatedAt: string;
  createdAt: string;
  revision: number;
  error?: string;
  output?: Record<string, unknown>;
  objectiveId?: string;
  wakeCondition?: { type: string; reference?: string; wakeAt?: string };
  pausedFrom?: string;
  pendingInterventions?: {
    id: string;
    instruction: string;
    actor: { type: string; id: string };
    createdAt: string;
  }[];
  budgetState?: string;
  budgetAdmission?: { dimension: string; required: number; remaining: number };
};
export type RunInspection = { requestId?: string; approvalId?: string };
export type RefinementValue = {
  text?: string;
  items?: string[];
  optionIds?: string[];
  boolean?: boolean;
  skillIds?: string[];
};
export type RefinementQuestion = {
  id: string;
  prompt: string;
  whyNeeded: string;
  priority: number;
  answer: {
    kind: string;
    options?: { id: string; label: string; description?: string }[];
    minimum?: number;
    maximum?: number;
  };
  dependsOn?: { questionId: string; requiredOptionIds?: string[] }[];
};
export type TeamDefinition = {
  id: string;
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
    channelParticipation?: string;
    skillGrants?: {
      skillId: string;
      skillVersion: string;
      allowedActions?: string[];
      enablePrompt?: boolean;
      maximumRisk: string;
    }[];
  }[];
  approvals: {
    maximumRisk: string;
    approverRoleIds?: string[];
    approverPrincipals?: string[];
  };
  coordination?: {
    maximumSpeakersPerRound?: number;
    quietByDefault?: boolean;
    requireRoleRelevance?: boolean;
    suppressDuplicateContent?: boolean;
  };
  delegation?: {
    maximumDepth?: number;
    maximumConcurrent?: number;
    allowPeerDelegation?: boolean;
    requireAcceptance?: boolean;
    requireCompletionReview?: boolean;
    completionReviewQuorum?: number;
    escalateOnDisagreement?: boolean;
  };
  sharedContext?: {
    retention?: number;
    maximumBytes?: number;
    allowMemberRead?: boolean;
    allowMemberWrite?: boolean;
  };
  objectiveTemplates?: {
    id: string;
    title: string;
    goal: string;
    successCriteria?: Record<string, unknown>;
    constraints?: Record<string, unknown>;
  }[];
};
export type Proposal = {
  id: string;
  parentId?: string;
  prompt: string;
  status: string;
  revision: number;
  candidateDigest?: string;
  catalog?: {
    skills?: Record<
      string,
      {
        id: string;
        name?: string;
        version?: string;
        sourceIdentity?: string;
        readiness?: string;
      }
    >;
  };
  placement?: {
    credentialReferences?: Record<
      string,
      Record<string, { kind: string; id: string }>
    >;
    bindingConfigs?: Record<string, Record<string, Record<string, unknown>>>;
    [key: string]: unknown;
  };
  requiredCredentialBindings?: Record<string, { key: string; kind: string }[]>;
  generation?: {
    attempt: number;
    lastError?: string;
    failureCode?: string;
    completedAt?: string;
  };
  refinement?: {
    questions?: RefinementQuestion[];
    answers?: { questionId: string; value: RefinementValue }[];
  };
  applyReceipt?: {
    activation: string;
    resources?: { kind: string; id: string }[];
  };
  evaluations?: {
    id: string;
    candidateDigest: string;
    allowed: boolean;
    findings?: { message: string }[];
  }[];
  result?: {
    valid: boolean;
    diff?: { path: string }[];
    assumptions?: string[];
    validation?: { path: string; message: string; code: string }[];
    missingRequirements?: { kind: string; id: string; requiredBy: string }[];
    candidate?: {
      activation?: string;
      agents?: {
        id: string;
        displayName: string;
        purpose: string;
        systemPrompt: string;
        operatingPrinciples?: string[];
        authority?: {
          maximumRisk: string;
          requireApprovalAt?: string;
          maxConcurrentRuns?: number;
        };
        skillRequirements?: { skillId: string }[];
      }[];
      team?: TeamDefinition;
      assignments?: {
        id: string;
        roleId: string;
        agentDefinitionId: string;
        displayName?: string;
      }[];
      [key: string]: unknown;
    };
  };
};
export class ApiError extends Error {
  constructor(
    message: string,
    public status = 0,
  ) {
    super(message);
  }
}
export async function status(): Promise<DesktopStatus> {
  if (isTauri()) return invoke("desktop_status");
  const response = await fetch("/__desktop/status");
  if (
    !response.ok ||
    !response.headers.get("content-type")?.includes("application/json")
  )
    throw new Error(
      "Open this workspace in the OpenSeal desktop app or its development server.",
    );
  return response.json();
}
export async function api<T>(
  path: string,
  options: { method?: string; body?: unknown; key?: string } = {},
): Promise<T> {
  const { method = "GET", body, key } = options;
  let result: { status: number; body: unknown };
  if (isTauri()) {
    result = await invoke("api_request", {
      method,
      path: `/api/v1${path}`,
      body: body ?? null,
      idempotencyKey: key ?? null,
    });
  } else {
    const response = await fetch(`/api/v1${path}`, {
      method,
      headers: {
        "Content-Type": "application/json",
        ...(key ? { "Idempotency-Key": key } : {}),
      },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(35_000),
    });
    result = { status: response.status, body: await response.json() };
  }
  if (result.status < 200 || result.status >= 300)
    throw new ApiError(
      (result.body as { error?: string })?.error ||
        "OpenSeal could not complete the request.",
      result.status,
    );
  return result.body as T;
}
export const supports = (
  caps: Capabilities | null,
  id: string,
  operation: string,
) =>
  !!caps?.capabilities.some(
    (cap) =>
      cap.id === id && cap.available && cap.operations.includes(operation),
  );
export function message(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

export type ProviderSettings = {
  baseUrl: string;
  model: string;
  hasApiKey: boolean;
};
export type ProviderUpdate = { baseUrl: string; model: string; apiKey: string };
export type ProviderSaveResult = {
  settings: ProviderSettings;
  reconnected: boolean;
  message: string;
};
export type SkillCredentialUpdate = {
  kind: string;
  bindingKey: string;
  displayName: string;
  secret: string;
};
export type SkillCredentialSaveResult = {
  credential: { kind: string; id: string; displayName: string };
  reconnected: boolean;
  message: string;
};
export class NativeUpdateRequired extends Error {
  constructor() {
    super(
      "Close and reopen OpenSeal to finish updating provider settings. Reloading this page cannot update the running app.",
    );
  }
}
async function invokeProvider<T>(
  command: string,
  args?: Record<string, unknown>,
): Promise<T> {
  try {
    return await invoke<T>(command, args);
  } catch (error) {
    if (message(error) === `Command ${command} not found`) {
      throw new NativeUpdateRequired();
    }
    throw error;
  }
}
export async function loadProvider(): Promise<ProviderSettings> {
  if (isTauri()) return invokeProvider("load_provider_settings");
  const response = await fetch("/__desktop/provider");
  const result = await response.json();
  if (!response.ok)
    throw new Error(result.error || "Could not load provider settings.");
  return result;
}
export async function saveProvider(
  settings: ProviderUpdate,
): Promise<ProviderSaveResult> {
  if (isTauri()) return invokeProvider("save_provider_settings", { settings });
  const response = await fetch("/__desktop/provider", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(settings),
    signal: AbortSignal.timeout(45_000),
  });
  const result = await response.json();
  if (!response.ok)
    throw new Error(result.error || "Could not save provider settings.");
  return result;
}
export async function saveSkillCredential(
  credential: SkillCredentialUpdate,
): Promise<SkillCredentialSaveResult> {
  if (isTauri())
    return invokeProvider("save_skill_credential_settings", { credential });
  const response = await fetch("/__desktop/skill-credential", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(credential),
    signal: AbortSignal.timeout(45_000),
  });
  const result = await response.json();
  if (!response.ok)
    throw new Error(result.error || "Could not save Skill connection.");
  return result;
}

export function isActivationProposal(proposal: Proposal): boolean {
  return (
    !!proposal.parentId &&
    proposal.result?.candidate?.activation === "active" &&
    proposal.result?.diff?.length === 1 &&
    proposal.result.diff[0].path === "activation"
  );
}
