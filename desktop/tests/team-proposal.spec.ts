import { test, expect } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";

test("team permissions stay readable and unmatched assignments prevent approval", async ({
  page,
}) => {
  const proposal = {
    id: "team-review",
    prompt: "Create one team for evidence review.",
    status: "evaluated",
    revision: 3,
    candidateDigest: "team-digest",
    evaluations: [
      { id: "evaluation", allowed: true, candidateDigest: "team-digest" },
    ],
    result: {
      valid: true,
      candidate: {
        activation: "inactive",
        agents: [
          {
            id: "researcher",
            displayName: "Researcher",
            purpose: "Find evidence",
            systemPrompt: "Cite sources.",
          },
        ],
        assignments: [
          {
            id: "assignment",
            roleId: "review",
            agentDefinitionId: "missing-agent",
          },
        ],
        team: {
          id: "team",
          displayName: "Evidence team",
          purpose: "Review evidence together.",
          roles: [
            {
              id: "review",
              displayName: "Review",
              purpose: "Check uncertainty.",
              minimumMembers: 1,
              channelParticipation: "observe_only",
              skillGrants: [
                {
                  skillId: "evidence-reader",
                  skillVersion: "1",
                  maximumRisk: "read",
                  allowedActions: ["search"],
                  enablePrompt: true,
                },
              ],
            },
          ],
          approvals: {
            maximumRisk: "read",
            approverRoleIds: ["review"],
            approverPrincipals: ["user:owner"],
          },
          delegation: {
            allowPeerDelegation: true,
            requireAcceptance: true,
            requireCompletionReview: true,
            completionReviewQuorum: 2,
            maximumConcurrent: 3,
            maximumDepth: 2,
            escalateOnDisagreement: true,
          },
          sharedContext: {
            allowMemberRead: true,
            allowMemberWrite: false,
            retention: 60000000000,
            maximumBytes: 4096,
          },
          objectiveTemplates: [
            {
              id: "report",
              title: "Evidence report",
              goal: "Explain the evidence.",
              successCriteria: { citations: true },
              constraints: { external_writes: false },
            },
          ],
        },
      },
    },
  };
  let writes = 0;
  await page.addInitScript(() =>
    localStorage.setItem("openseal.proposal", "team-review"),
  );
  await page.route("**/api/v1/capabilities**", (route) =>
    route.fulfill({
      json: {
        capabilities: [
          {
            id: "workforce-authoring",
            available: true,
            operations: ["get", "approve", "apply"],
            context: {
              changeSetId: proposal.id,
              revision: proposal.revision,
              eligibleApprovalRequirements: [
                {
                  evaluationId: "evaluation",
                  policyId: "policy",
                  role: "operator",
                },
              ],
            },
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/authoring/workforce/change-sets/**", (route) => {
    if (route.request().method() === "POST") writes++;
    return route.fulfill({ json: proposal });
  });
  await page.goto("/");
  await expect(
    page.getByRole("heading", { name: "Team proposal", exact: true }),
  ).toBeVisible();
  await expect(page.getByRole("alert")).toContainText(
    "Some team assignments do not match",
  );
  await page
    .getByRole("checkbox", { name: "I reviewed the proposed resources" })
    .check();
  await expect(
    page.getByRole("button", { name: "Approve installation", exact: true }),
  ).toBeDisabled();
  await expect(
    page.getByRole("button", {
      name: "Install without activating",
      exact: true,
    }),
  ).toBeDisabled();
  expect(writes).toBe(0);
  const team = page.getByRole("region", { name: "Proposed team", exact: true });
  await team
    .getByText("Delegation and shared context", { exact: true })
    .click();
  await team.getByText("Role skill permissions", { exact: true }).click();
  await expect(team.getByText("60 seconds", { exact: true })).toBeVisible();
  await expect(team.getByText("4,096 bytes", { exact: true })).toBeVisible();
  await expect(
    team.getByText("Allowed actions: search", { exact: true }),
  ).toBeVisible();
  await expect(
    team.getByText("Channel participation: observe only", { exact: true }),
  ).toBeVisible();
  await expect(
    team.getByText("Approver roles: Review", { exact: true }),
  ).toBeVisible();
  await expect(
    team.getByText("Explain the evidence.", { exact: true }),
  ).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});

test("blocked team proposal names each requirement and its repair", async ({
  page,
}) => {
  const proposal = {
    id: "blocked-team-review",
    prompt: "Create a team with an outbound workflow.",
    status: "blocked",
    revision: 2,
    candidateDigest: "blocked-team-digest",
    result: {
      valid: false,
      validation: [
        {
          path: "agents.sender.runbook.steps.publish",
          code: "runbook_activation_action_approval_unreachable",
          message: "Action publish requires approval but no route is reachable",
        },
      ],
      missingRequirements: [
        {
          kind: "skill_binding",
          id: "delivery",
          requiredBy: "agent:sender",
        },
        {
          kind: "credential",
          id: "connection",
          requiredBy: "agent:sender/skill:delivery",
        },
      ],
      candidate: {
        activation: "active",
        agents: [
          {
            id: "sender",
            displayName: "Sender",
            purpose: "Send reviewed updates.",
            systemPrompt: "Wait for approval.",
          },
        ],
        assignments: [
          {
            id: "sender-assignment",
            roleId: "sender",
            agentDefinitionId: "sender",
          },
        ],
        team: {
          id: "outbound-team",
          displayName: "Outbound team",
          purpose: "Coordinate outbound updates.",
          roles: [
            {
              id: "sender",
              displayName: "Sender",
              purpose: "Send approved updates.",
              minimumMembers: 1,
              channelParticipation: "active",
            },
          ],
          approvals: {
            maximumRisk: "external",
            approverPrincipals: ["user:owner"],
          },
        },
      },
    },
  };
  await page.addInitScript(() =>
    localStorage.setItem("openseal.proposal", "blocked-team-review"),
  );
  await page.route("**/api/v1/capabilities**", (route) =>
    route.fulfill({
      json: {
        capabilities: [
          {
            id: "workforce-authoring",
            available: true,
            operations: ["get"],
            context: { changeSetId: proposal.id, revision: proposal.revision },
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/authoring/workforce/change-sets/**", (route) =>
    route.fulfill({ json: proposal }),
  );
  await page.goto("/");
  await expect(
    page.getByRole("heading", { name: "Team proposal", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText(/make a new request to create it inactive/),
  ).toBeVisible();
  await expect(
    page.getByText("Skill delivery has no ready binding."),
  ).toBeVisible();
  await expect(
    page.getByText(/Select the Skill settings and credential connection below/),
  ).toBeVisible();
  await expect(
    page.getByText("Required credential connection is not bound."),
  ).toBeVisible();
  await expect(
    page.getByText(
      /Add a private local connection or select an existing one below/,
    ),
  ).toBeVisible();
  await expect(
    page.getByText(/Configure a reviewed approval destination/),
  ).toBeVisible();
});

test("blocked Skill can be verified, installed, and configured from proposal review", async ({
  page,
}) => {
  const proposal = {
    id: "skill-team-review",
    prompt: "Create one team with a research Skill.",
    status: "blocked",
    revision: 2,
    catalog: {
      skills: {
        research: {
          id: "research",
          name: "Research",
          version: "1.2.3",
          sourceIdentity: "https://clawhub.ai::@acme/research",
          readiness: "needs_installation",
        },
      },
    },
    placement: {},
    requiredCredentialBindings: {
      researcher: [{ key: "RESEARCH_API_KEY", kind: "environment-secret" }],
    },
    result: {
      valid: false,
      missingRequirements: [
        {
          kind: "skill_installation",
          id: "research",
          requiredBy: "agent:researcher",
        },
        {
          kind: "skill_binding",
          id: "research",
          requiredBy: "agent:researcher",
        },
      ],
      candidate: {
        activation: "active",
        agents: [
          {
            id: "researcher",
            displayName: "Researcher",
            purpose: "Find evidence",
            systemPrompt: "Cite sources.",
            skillRequirements: [{ skillId: "research" }],
          },
        ],
        team: {
          id: "team",
          displayName: "Research team",
          purpose: "Find evidence",
          roles: [],
          approvals: { maximumRisk: "read" },
        },
      },
    },
  };
  await page.addInitScript(() =>
    localStorage.setItem("openseal.proposal", "skill-team-review"),
  );
  await page.route("**/api/v1/capabilities**", (route) =>
    route.fulfill({
      json: {
        capabilities: [
          {
            id: "workforce-authoring",
            available: true,
            operations: ["get", "patch"],
            context: {
              changeSetId: proposal.id,
              revision: proposal.revision,
              credentialBindings: [
                {
                  reference: {
                    kind: "environment-secret",
                    id: "credential://research",
                  },
                  displayName: "Research connection",
                  bindingKeys: ["RESEARCH_API_KEY"],
                },
              ],
              bindingConfigurationFields: [
                {
                  catalogSkillId: "research",
                  key: "region",
                  type: "string",
                  required: true,
                  prompt: "Region",
                  options: [{ label: "US", value: { string: "us" } }],
                },
              ],
            },
          },
          {
            id: "clawhub-lifecycle",
            available: true,
            operations: ["verify", "install", "inspect_catalog"],
          },
        ],
      },
    }),
  );
  let placement: any;
  await page.route(
    "**/api/v1/authoring/workforce/change-sets/skill-team-review**",
    (route) => route.fulfill({ json: proposal }),
  );
  await page.route(
    "**/api/v1/authoring/workforce/change-sets/skill-team-review/placement",
    async (route) => {
      placement = route.request().postDataJSON().placement;
      await route.fulfill({
        json: {
          ...proposal,
          placement,
          revision: 3,
          result: {
            ...proposal.result,
            missingRequirements: [proposal.result.missingRequirements[0]],
          },
        },
      });
    },
  );
  let installed = false;
  await page.route("**/api/v1/clawhub/catalog/**/verify", (route) =>
    route.fulfill({
      json: {
        ok: true,
        decision: "pass",
        displayName: "Research",
        publisherHandle: "acme",
        version: "1.2.3",
      },
    }),
  );
  await page.route("**/api/v1/clawhub/catalog/**/install", (route) => {
    installed = true;
    return route.fulfill({
      json: {
        sourceIdentity: "https://clawhub.ai::@acme/research",
        version: "1.2.3",
        outcome: "installed",
      },
    });
  });
  await page.goto("/");
  await page
    .getByRole("button", { name: "Review Skill for installation" })
    .click();
  await expect(page.getByText("Verification: pass")).toBeVisible();
  await page
    .getByRole("button", { name: "Install @acme/research version 1.2.3" })
    .click();
  await expect(
    page.getByText(/Skill installed\. Create a fresh proposal/),
  ).toBeVisible();
  expect(installed).toBe(true);
  await page.getByLabel("Region").selectOption("0");
  await page.getByLabel("RESEARCH_API_KEY credential").selectOption("0");
  await page.getByRole("button", { name: "Save Skill configuration" }).click();
  await expect
    .poll(() => placement?.bindingConfigs?.researcher?.research?.region)
    .toBe("us");
  expect(placement.credentialReferences.researcher.RESEARCH_API_KEY).toEqual({
    kind: "environment-secret",
    id: "credential://research",
  });
});

test("missing Skill can be found in ClawHub and installed after verification", async ({
  page,
}) => {
  const proposal = {
    id: "missing-skill",
    prompt: "Create a research agent",
    status: "blocked",
    revision: 1,
    result: {
      valid: false,
      missingRequirements: [
        { kind: "skill", id: "research", requiredBy: "agent:researcher" },
      ],
    },
  };
  await page.addInitScript(() =>
    localStorage.setItem("openseal.proposal", "missing-skill"),
  );
  await page.route("**/api/v1/capabilities**", (route) =>
    route.fulfill({
      json: {
        capabilities: [
          {
            id: "workforce-authoring",
            available: true,
            operations: ["get"],
            context: { changeSetId: proposal.id, revision: 1 },
          },
          {
            id: "clawhub-lifecycle",
            available: true,
            operations: ["inspect_catalog", "verify", "install"],
          },
        ],
      },
    }),
  );
  await page.route(
    "**/api/v1/authoring/workforce/change-sets/missing-skill**",
    (route) => route.fulfill({ json: proposal }),
  );
  await page.route("**/api/v1/clawhub/search?query=research", (route) =>
    route.fulfill({
      json: { items: [{ slug: "research", name: "Research Skill" }] },
    }),
  );
  await page.route("**/api/v1/clawhub/catalog/research", (route) =>
    route.fulfill({
      json: {
        slug: "research",
        name: "Research Skill",
        owner: "acme",
        version: "2.0.0",
      },
    }),
  );
  await page.route("**/api/v1/clawhub/catalog/**/verify", (route) =>
    route.fulfill({
      json: {
        ok: true,
        decision: "pass",
        displayName: "Research Skill",
        publisherHandle: "acme",
        version: "2.0.0",
      },
    }),
  );
  let installed = false;
  await page.route("**/api/v1/clawhub/catalog/**/install", (route) => {
    installed = true;
    return route.fulfill({
      json: {
        sourceIdentity: "https://clawhub.ai::@acme/research",
        version: "2.0.0",
        outcome: "installed",
      },
    });
  });
  await page.goto("/");
  await page.getByRole("button", { name: "Find Skill research" }).click();
  await page.getByRole("button", { name: "Review this Skill" }).click();
  await page
    .getByRole("button", { name: "Review Skill for installation" })
    .click();
  await page
    .getByRole("button", { name: "Install @acme/research version 2.0.0" })
    .click();
  await expect(
    page.getByText(/Skill installed\. Create a fresh proposal/),
  ).toBeVisible();
  expect(installed).toBe(true);
});

test("blocked Skill can add a private local credential connection", async ({
  page,
}) => {
  const proposal = {
    id: "needs-skill-connection",
    prompt: "Create a research agent",
    status: "blocked",
    revision: 1,
    placement: {},
    requiredCredentialBindings: {
      researcher: [{ key: "RESEARCH_API_KEY", kind: "environment-secret" }],
    },
    result: {
      valid: false,
      missingRequirements: [
        {
          kind: "skill_binding",
          id: "research",
          requiredBy: "agent:researcher",
        },
      ],
    },
  };
  await page.addInitScript(() =>
    localStorage.setItem("openseal.proposal", "needs-skill-connection"),
  );
  await page.route("**/api/v1/capabilities**", (route) =>
    route.fulfill({
      json: {
        capabilities: [
          {
            id: "workforce-authoring",
            available: true,
            operations: ["get", "patch"],
            context: { changeSetId: proposal.id, revision: 1 },
          },
        ],
      },
    }),
  );
  await page.route(
    "**/api/v1/authoring/workforce/change-sets/needs-skill-connection**",
    (route) => route.fulfill({ json: proposal }),
  );
  let saved: any;
  await page.route("**/__desktop/skill-credential", (route) => {
    saved = route.request().postDataJSON();
    return route.fulfill({
      json: {
        credential: {
          kind: "environment-secret",
          id: "desktop-skill-1",
          displayName: "Research account",
        },
        reconnected: false,
        message: "Connection saved; reopen workspace.",
      },
    });
  });
  await page.goto("/");
  await page
    .getByRole("button", { name: "Add RESEARCH_API_KEY connection" })
    .click();
  await page.getByLabel("Connection name").fill("Research account");
  await page.getByLabel("RESEARCH_API_KEY secret").fill("private-value");
  await page.getByRole("button", { name: "Save connection" }).click();
  await expect(
    page.getByText("Connection saved; reopen workspace."),
  ).toBeVisible();
  expect(saved).toEqual({
    kind: "environment-secret",
    bindingKey: "RESEARCH_API_KEY",
    displayName: "Research account",
    secret: "private-value",
  });
  await expect(page.getByLabel("RESEARCH_API_KEY secret")).toHaveValue("");
});
