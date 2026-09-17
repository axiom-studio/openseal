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
