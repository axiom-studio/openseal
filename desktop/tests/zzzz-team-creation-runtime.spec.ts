import { test, expect } from "@playwright/test";
import { execFileSync } from "node:child_process";
import { join } from "node:path";
import AxeBuilder from "@axe-core/playwright";
import {
  AUTHORING_TOOL,
  COMPLETION_REVIEW_TOOL,
  DELEGATE_TOOL,
  FINAL_TOOL,
  REQUEST_DECISION_TOOL,
  ROUTE_TOOL,
  type ChatRequest,
  firstUserText,
  hasTool,
  isAuthoring,
  startLoopProvider,
  tool,
  userTexts,
} from "./fixtures/loop-provider";

// The coordinator answers the recipient's clarification question with this
// kernel tool (pkg/runtime/control_delegate.go).
const DELEGATE_REPLY_TOOL = "agent_delegate_reply";
const COORDINATOR_GOAL = "Coordinate evidence review.";
const DELEGATION_NARRATION = "Asking the reviewer to check the evidence.";
const QUESTION = "Which reporting period should I use?";
const GUIDANCE_MARK = "July reporting period";
const CLARIFICATION_ANSWER = "Use the July reporting period.";
const TEAM_ANSWER = "Team evidence review completed with uncertainty noted.";

type DelegationResult = {
  requestId: string;
  status: string;
  question?: string;
  answer?: string;
};

// latestDelegation is the newest agent_delegate or agent_delegate_reply
// result in the step's session: the delegated request's state as the
// coordinator last saw it.
function latestDelegation(r: ChatRequest): DelegationResult | undefined {
  const ids = new Set<string>();
  let latest: DelegationResult | undefined;
  for (const message of r.messages) {
    for (const call of message.tool_calls ?? [])
      if (
        call.function.name === DELEGATE_TOOL ||
        call.function.name === DELEGATE_REPLY_TOOL
      )
        ids.add(call.id);
    if (
      message.role === "tool" &&
      message.tool_call_id &&
      ids.has(message.tool_call_id)
    )
      try {
        latest = JSON.parse(message.content ?? "");
      } catch {
        // A refused call renders as "Error: ..." text, not a JSON result.
        latest = {
          requestId: "",
          status: "error",
          answer: message.content ?? "",
        };
      }
  }
  return latest;
}

for (const inactive of [false, true]) {
  test(`real daemon creates a reviewed ${inactive ? "inactive" : "active"} team and its roster`, async ({
    page,
    request,
  }) => {
    test.setTimeout(60000);
    page.setDefaultTimeout(10000);
    const name = inactive ? "Draft evidence team" : "Evidence review team";
    const researcherKey = inactive
      ? "draft-team-researcher"
      : "team-researcher";
    const reviewerKey = inactive ? "draft-team-reviewer" : "team-reviewer";
    const intent = {
      schemaVersion: "openseal.authoring-intent/v4",
      kind: "team",
      name,
      purpose: "Review supplied evidence together.",
      agents: [
        {
          key: researcherKey,
          name: "Team researcher",
          purpose: "Find the evidence.",
          behavior: "Compare supplied evidence and cite sources.",
        },
        {
          key: reviewerKey,
          name: "Team reviewer",
          purpose: "Check uncertainty.",
          behavior: "Challenge conclusions and report uncertainty.",
        },
      ],
      team: {
        key: inactive ? "draft-evidence" : "evidence",
        name,
        purpose: "Review supplied evidence together.",
        operatingPrinciples: ["Keep uncertainty explicit."],
        roles: [
          {
            key: "research",
            name: "Research",
            purpose: "Gather evidence.",
            agentKeys: [researcherKey],
            canSpeakInChannels: true,
          },
          {
            key: "review",
            name: "Review",
            purpose: "Challenge findings.",
            agentKeys: [reviewerKey],
            canSpeakInChannels: false,
          },
        ],
      },
    };
    let generationCalls = 0;
    let releaseClarification!: () => void;
    const clarificationGate = new Promise<void>(
      (resolve) => (releaseClarification = resolve),
    );
    const intakeSeen = new Set<string>();
    let waitingForGuidance = false;

    const provider = await startLoopProvider(async (r) => {
      if (isAuthoring(r)) {
        expect(JSON.parse(r.messages[1].content ?? "{}").prompt).toContain(
          "Create one team.",
        );
        generationCalls++;
        return { toolCalls: [{ name: AUTHORING_TOOL, args: intent }] };
      }
      // T2: the recipient's intake decision. It asks once, and accepts once
      // the coordinator's answer is in its session.
      if (hasTool(r, REQUEST_DECISION_TOOL)) {
        const key = firstUserText(r);
        const answered =
          intakeSeen.has(key) ||
          userTexts(r).some((text) => text.includes(CLARIFICATION_ANSWER));
        intakeSeen.add(key);
        return {
          toolCalls: [
            {
              name: REQUEST_DECISION_TOOL,
              args: answered
                ? { decision: "accept", message: "Reporting period is clear." }
                : { decision: "request_clarification", message: QUESTION },
            },
          ],
        };
      }
      // T4: the coordinator's review of the delegated result.
      if (hasTool(r, COMPLETION_REVIEW_TOOL))
        return {
          toolCalls: [
            {
              name: COMPLETION_REVIEW_TOOL,
              args: {
                decision: "approve",
                message: "The result addresses the delegated goal.",
              },
            },
          ],
        };
      if (hasTool(r, ROUTE_TOOL))
        return {
          toolCalls: [
            {
              name: ROUTE_TOOL,
              args: {
                agentId: tool(r, ROUTE_TOOL)!.function.parameters.properties
                  .agentId.enum[0],
              },
            },
          ],
        };
      if (firstUserText(r).includes(COORDINATOR_GOAL)) {
        expect(hasTool(r, DELEGATE_TOOL)).toBe(true);
        const delegation = latestDelegation(r);
        // T1: delegate to the only other member, narrating the step.
        if (!delegation) {
          const targets = tool(r, DELEGATE_TOOL)!.function.parameters.properties
            .agentId.enum;
          expect(targets).toHaveLength(1);
          return {
            text: DELEGATION_NARRATION,
            toolCalls: [
              {
                name: DELEGATE_TOOL,
                args: {
                  agentId: targets[0],
                  goal: "Independently check the evidence.",
                },
              },
            ],
          };
        }
        if (delegation.status === "clarification_requested") {
          expect(delegation.question).toBe(QUESTION);
          // T3: hold until the test saves guidance. The kernel re-checks
          // interventions before it accepts this final answer (D8) and calls
          // again with the guidance in the session.
          if (!userTexts(r).some((text) => text.includes(GUIDANCE_MARK))) {
            waitingForGuidance = true;
            await clarificationGate;
            return {
              toolCalls: [
                {
                  name: FINAL_TOOL,
                  args: { answer: "Waiting for the reporting period." },
                },
              ],
            };
          }
          return {
            toolCalls: [
              {
                name: DELEGATE_REPLY_TOOL,
                args: {
                  requestId: delegation.requestId,
                  answer: CLARIFICATION_ANSWER,
                },
              },
            ],
          };
        }
        // T5: the accepted delegation came back completed.
        expect(delegation.status).toBe("completed");
        return {
          toolCalls: [{ name: FINAL_TOOL, args: { answer: TEAM_ANSWER } }],
        };
      }
      // Member work: a plain-text final answer.
      return { text: TEAM_ANSWER };
    });
    try {
      expect(
        (
          await request.post("/__desktop/provider", {
            data: {
              baseUrl: provider.baseUrl,
              model: "synthetic-team-model",
              apiKey: "synthetic-team-key",
            },
          })
        ).ok(),
      ).toBe(true);
      await page.goto("/");
      await expect(
        page.getByText("Connected locally", { exact: true }),
      ).toBeVisible();
      await page.keyboard.press("Control+5");
      await page
        .getByRole("button", { name: "Create team", exact: true })
        .click();
      const prompt = page.getByRole("textbox", { name: "Describe your team" });
      await expect(prompt).toBeFocused();
      await prompt.fill(
        `${inactive ? "Keep the team inactive. " : ""}Review supplied evidence with a researcher and reviewer.`,
      );
      await page.reload();
      await expect(prompt).toHaveValue(
        `${inactive ? "Keep the team inactive. " : ""}Review supplied evidence with a researcher and reviewer.`,
      );
      await expect(
        page.getByRole("button", { name: "Create a team", exact: true }),
      ).toHaveAttribute("aria-pressed", "true");
      await page
        .getByRole("button", { name: "Create proposal", exact: true })
        .click();
      await expect(
        page.getByRole("heading", { name: "Team proposal", exact: true }),
      ).toBeVisible({ timeout: 30000 });
      const team = page.getByRole("region", {
        name: "Proposed team",
        exact: true,
      });
      await expect(
        team.getByRole("region", { name: "Proposed role: Research" }),
      ).toContainText("Team researcher");
      await expect(
        team.getByRole("region", { name: "Proposed role: Review" }),
      ).toContainText("Team reviewer");
      await expect(team.getByText(/Maximum team risk:/)).toBeVisible();
      await team
        .getByText("Delegation and shared context", { exact: true })
        .click();
      await expect(
        team.getByText("Members may read shared context"),
      ).toBeVisible();
      await page
        .getByRole("button", { name: "Check installation", exact: true })
        .click();
      const approval = page.getByRole("button", {
        name: "Approve installation",
        exact: true,
      });
      await expect(approval).toBeDisabled();
      if (!inactive) {
        const dismiss = page.getByRole("button", {
          name: "Dismiss notification",
          exact: true,
        });
        if (await dismiss.count()) await dismiss.click();
        expect((await new AxeBuilder({ page }).analyze()).violations).toEqual(
          [],
        );
        await page.evaluate(() => {
          (document.activeElement as HTMLElement)?.blur();
          window.scrollTo(0, 0);
        });
        await page.screenshot({
          path: "../.impeccable/review/team-proposal-desktop.png",
          fullPage: true,
        });
        await page.setViewportSize({ width: 390, height: 844 });
        expect(
          await page.evaluate(
            () => document.documentElement.scrollWidth <= innerWidth,
          ),
        ).toBe(true);
        expect((await new AxeBuilder({ page }).analyze()).violations).toEqual(
          [],
        );
        await page.evaluate(() => {
          (document.activeElement as HTMLElement)?.blur();
          window.scrollTo(0, 0);
        });
        await page.screenshot({
          path: "../.impeccable/review/team-proposal-mobile.png",
          fullPage: true,
        });
        await page.setViewportSize({ width: 1440, height: 1000 });
      }
      await page
        .getByRole("checkbox", { name: "I reviewed the proposed resources" })
        .check();
      await approval.click();
      await page
        .getByRole("button", {
          name: inactive
            ? "Install without activating"
            : "Install and activate",
          exact: true,
        })
        .click();
      await expect(
        page.getByRole("heading", { name: "Installed", exact: true }),
      ).toBeFocused();
      const id = await page.evaluate(() =>
        localStorage.getItem("openseal.proposal"),
      );
      const readProposal = async (proposalID: string) =>
        (
          await request.get(
            `/api/v1/authoring/workforce/change-sets/${proposalID}?scopeKind=local&scopeId=default`,
          )
        ).json();
      const result = await readProposal(id!);
      expect(result.result.candidate.team.displayName).toBe(name);
      expect(result.applyReceipt.activation).toBe(
        inactive ? "inactive" : "active",
      );
      const teams = await (
        await request.get(
          "/api/v1/team-deployments?scopeKind=local&scopeId=default",
        )
      ).json();
      const installed = teams.items.find(
        (item: any) => item.definition.displayName === name,
      );
      expect(installed.deployment.roster).toHaveLength(2);
      expect(installed.deployment.status).toBe(inactive ? "draft" : "active");
      if (inactive) {
        await page
          .getByRole("button", { name: "View teams", exact: true })
          .click();
        await page.evaluate(() => localStorage.removeItem("openseal.proposal"));
        await page.reload();
        await expect(
          page.getByText("Connected locally", { exact: true }),
        ).toBeVisible();
        await page.keyboard.press("Control+5");
        await page
          .getByRole("button", { name: new RegExp(name + ".*draft") })
          .click();
        await expect(
          page.getByRole("button", { name: "Resume team", exact: true }),
        ).toHaveCount(0);
        await page
          .getByRole("button", { name: "Review team activation", exact: true })
          .click();
        const before = generationCalls;
        await page
          .getByRole("button", { name: "Review activation", exact: true })
          .click();
        await expect(team).toContainText(
          "activates the existing team and its agents",
        );
        await expect(team).not.toContainText(
          "creates the team and its agents together",
        );
        const dismiss = page.getByRole("button", {
          name: "Dismiss notification",
          exact: true,
        });
        if (await dismiss.count()) await dismiss.click();
        await page.evaluate(() => {
          (document.activeElement as HTMLElement)?.blur();
          window.scrollTo(0, 0);
        });
        await page.screenshot({
          path: "../.impeccable/review/team-activation-desktop.png",
          fullPage: true,
        });
        await page.setViewportSize({ width: 390, height: 844 });
        await page.evaluate(() => window.scrollTo(0, 0));
        await page.screenshot({
          path: "../.impeccable/review/team-activation-mobile.png",
          fullPage: true,
        });
        await page.setViewportSize({ width: 1440, height: 1000 });
        await page
          .getByRole("button", { name: "Check activation", exact: true })
          .click();
        await page
          .getByRole("checkbox", { name: "I reviewed the proposed resources" })
          .check();
        await page
          .getByRole("button", { name: "Approve activation", exact: true })
          .click();
        await page
          .getByRole("button", { name: "Activate resources", exact: true })
          .click();
        await expect(
          page.getByRole("heading", { name: "Activated", exact: true }),
        ).toBeFocused();
        expect(generationCalls).toBe(before);
        const activated = await (
          await request.get(
            "/api/v1/team-deployments?scopeKind=local&scopeId=default",
          )
        ).json();
        const next = activated.items.find(
          (item: any) => item.definition.displayName === name,
        );
        expect(next.deployment.id).toBe(installed.deployment.id);
        expect(next.deployment.status).toBe("active");
        expect(next.deployment.roster).toEqual(installed.deployment.roster);
      }
      await page
        .getByRole("button", { name: "View teams", exact: true })
        .click();
      await page
        .getByRole("button", { name: new RegExp(name + ".*active") })
        .click();
      await expect(
        page.getByRole("region", { name: `${name} details` }),
      ).toContainText("2 members");
      await page.reload();
      await expect(
        page.getByText("Connected locally", { exact: true }),
      ).toBeVisible();
      await page.keyboard.press("Control+5");
      await page
        .getByRole("button", { name: new RegExp(name + ".*active") })
        .click();
      await expect(
        page.getByRole("region", { name: `${name} details` }),
      ).toContainText("Keep uncertainty explicit.");
      if (!inactive) {
        await page
          .getByRole("button", { name: "Team work", exact: true })
          .click();
        await page
          .getByLabel("Lead agent", { exact: true })
          .selectOption(installed.deployment.roster[0].agentDeploymentId);
        await page
          .getByLabel("What should the team accomplish?", { exact: true })
          .fill(COORDINATOR_GOAL);
        const submittedWork = page.waitForRequest(
          (request) =>
            request.method() === "POST" &&
            new URL(request.url()).pathname === "/api/v1/agent-runs",
        );
        await page
          .getByRole("button", { name: "Start team work", exact: true })
          .click();
        const originalWorkRequest = await submittedWork;
        await expect
          .poll(() => waitingForGuidance, { timeout: 15000 })
          .toBe(true);
        await page.keyboard.press("Control+3");
        await page
          .getByRole("button", { name: "Requests", exact: true })
          .click();
        await page
          .locator('[aria-label="Workspace requests"] .record-row')
          .filter({ hasText: "Which reporting period should I use?" })
          .click();
        await expect(
          page.getByText(QUESTION, {
            exact: true,
          }),
        ).toBeVisible({ timeout: 10000 });
        await page
          .getByRole("button", { name: "Guide the lead", exact: true })
          .click();
        await page
          .getByLabel("Guidance for this task", { exact: true })
          .fill("Use the July reporting period for this request.");
        await page
          .getByRole("button", { name: "Save guidance", exact: true })
          .click();
        await expect(page.locator(".work-guidance")).toContainText(
          /Guidance saved in this task|This task changed/,
        );
        if (
          await page
            .getByText(/This task changed. Your draft is preserved/)
            .count()
        ) {
          await page
            .getByRole("button", { name: "Save guidance", exact: true })
            .click();
        }
        await expect(
          page.getByText(/Guidance saved in this task/),
        ).toBeVisible();
        releaseClarification();

        const workPath = `/api/v1/agent-runs?scopeKind=local&scopeId=default&ownerType=team&ownerId=${installed.deployment.id}&limit=100`;
        await expect
          .poll(
            async () => {
              const rows = await (await request.get(workPath)).json();
              const root = rows.find((r: any) => r.goal === COORDINATOR_GOAL);
              return root?.status === "failed" ? root.error : root?.status;
            },
            { timeout: 30000 },
          )
          .toBe("completed");
        const rows = await (await request.get(workPath)).json();
        const root = rows.find((r: any) => r.goal === COORDINATOR_GOAL);
        expect(root.output.summary).toBe(TEAM_ANSWER);
        expect(provider.errors).toEqual([]);
        // T1: the coordinator's first model step carries its narration (D11).
        const rootSteps = await (
          await request.get(
            `/api/v1/agent-turns?scopeKind=local&scopeId=default&runId=${root.id}`,
          )
        ).json();
        expect(
          rootSteps.find((step: any) => step.sequence === 1)?.outputSummary,
        ).toBe(DELEGATION_NARRATION);
        const collaborations = await (
          await request.get(
            `/api/v1/agent-requests?scopeKind=local&scopeId=default&sourceRunId=${root.id}`,
          )
        ).json();
        expect(collaborations).toHaveLength(1);
        expect(collaborations[0].status).toBe("completed");
        expect(collaborations[0].clarification).toBe(
          "Which reporting period should I use?",
        );

        expect(
          rows.some(
            (r: any) => r.parentRunId === root.id && r.status === "completed",
          ),
        ).toBe(true);
        await page.keyboard.press("Control+5");
        await page
          .getByRole("button", { name: new RegExp(name + ".*active") })
          .click();
        await page
          .getByRole("button", { name: "Team work", exact: true })
          .click();
        await expect(page.locator(".team-run-list")).toContainText(
          "Delegated run",
        );
        for (const [label, operation, state] of [
          ["Pause team", "pause", "paused"],
          ["Archive team", "archive", "archived"],
          ["Restore team", "restore", "paused"],
          ["Resume team", "resume", "active"],
        ]) {
          await page.getByRole("button", { name: label, exact: true }).click();
          await page
            .getByRole("textbox", { name: "Reason for this team change" })
            .fill(`Owner reviewed ${operation} for ${name}.`);
          await page
            .getByRole("button", { name: `Confirm ${operation}`, exact: true })
            .click();
          await expect(
            page.getByRole("button", { name: new RegExp(name + ".*" + state) }),
          ).toBeVisible();
          if (operation === "pause") {
            const replay = await request.post("/api/v1/agent-runs", {
              data: originalWorkRequest.postDataJSON(),
              headers: {
                "Idempotency-Key":
                  originalWorkRequest.headers()["idempotency-key"],
              },
            });
            expect(replay.status()).toBe(200);
            expect((await replay.json()).run.id).toBe(root.id);
            const blocked = await request.post("/api/v1/agent-runs", {
              data: originalWorkRequest.postDataJSON(),
              headers: { "Idempotency-Key": "new-task-while-paused" },
            });
            expect(blocked.ok()).toBe(false);
          }
        }
        await page.getByText("Team change history", { exact: true }).click();
        await expect(page.locator(".team-history li").first()).toContainText(
          `Owner reviewed resume for ${name}.`,
        );
        const updated = await (
          await request.get(
            `/api/v1/team-deployments/${installed.deployment.id}?scopeKind=local&scopeId=default`,
          )
        ).json();
        expect(updated.revision).toBe(installed.deployment.revision + 4);
        expect(updated.roster).toEqual(installed.deployment.roster);
        const changes = await (
          await request.get(
            `/api/v1/team-deployments/${installed.deployment.id}/activations?scopeKind=local&scopeId=default`,
          )
        ).json();
        const audit = changes.find(
          (item: any) => item.deploymentRevision === updated.revision,
        );
        expect(audit.reason).toBe(`Owner reviewed resume for ${name}.`);
        expect(audit.actorId).toBe("local-operator");
        const researchRole = installed.definition.roles.find(
          (role: any) => role.displayName === "Research",
        );
        const original = updated.roster.find(
          (member: any) => member.roleId === researchRole.id,
        );
        execFileSync(
          "go",
          [
            "run",
            "tests/seed_roster_agent.go",
            "-db",
            join(process.env.OPENSEAL_UI_TEST_WORKSPACE!, "data/openseal.db"),
            "-source",
            original.agentDeploymentId,
          ],
          { timeout: 30000 },
        );
        await page.reload();
        await expect(
          page.getByText("Connected locally", { exact: true }),
        ).toBeVisible();
        await page.keyboard.press("Control+5");
        await page
          .getByRole("button", { name: new RegExp(name + ".*active") })
          .click();
        await page
          .getByRole("button", { name: "Edit roster", exact: true })
          .click();
        await page
          .getByLabel("Agent for Research 1", { exact: true })
          .selectOption("ui-roster-substitute");
        await page
          .getByLabel("Name in team for Research 1", { exact: true })
          .fill("Evidence lead");
        await page
          .getByLabel("Reason for roster change", { exact: true })
          .fill("Assign the research substitute after owner review.");
        await page
          .getByRole("checkbox", { name: "I reviewed the role assignments" })
          .check();
        await page
          .getByRole("button", { name: "Save roster", exact: true })
          .click();
        await expect(
          page.getByRole("status").filter({ hasText: "Team roster saved." }),
        ).toBeFocused();
        const rosterSaved = await (
          await request.get(
            `/api/v1/team-deployments/${updated.id}?scopeKind=local&scopeId=default`,
          )
        ).json();
        expect(rosterSaved.revision).toBe(updated.revision + 1);
        expect(rosterSaved.status).toBe(updated.status);
        expect(rosterSaved.restrictions).toEqual(updated.restrictions);
        expect(rosterSaved.roster).toEqual(
          updated.roster.map((member: any) =>
            member.id === original.id
              ? {
                  ...member,
                  agentDeploymentId: "ui-roster-substitute",
                  displayName: "Evidence lead",
                }
              : member,
          ),
        );
        const catalog = await (
          await request.get(
            "/api/v1/agent-deployments?scopeKind=local&scopeId=default",
          )
        ).json();
        expect(
          catalog.items.find(
            (item: any) => item.deployment.id === "ui-roster-substitute",
          ).deployment.rolloutStatus,
        ).toBe("paused");
        expect(
          catalog.items.find(
            (item: any) => item.deployment.id === original.agentDeploymentId,
          ).deployment.rolloutStatus,
        ).toBe("active");
        const rosterHistory = await (
          await request.get(
            `/api/v1/team-deployments/${updated.id}/activations?scopeKind=local&scopeId=default`,
          )
        ).json();
        expect(
          rosterHistory.find(
            (item: any) => item.deploymentRevision === rosterSaved.revision,
          ),
        ).toMatchObject({
          reason: "Assign the research substitute after owner review.",
          actorId: "local-operator",
        });
        await page.reload();
        await expect(
          page.getByText("Connected locally", { exact: true }),
        ).toBeVisible();
        await page.keyboard.press("Control+5");
        await page
          .getByRole("button", { name: new RegExp(name + ".*active") })
          .click();
        await expect(
          page.getByRole("button", { name: "View Evidence lead", exact: true }),
        ).toBeVisible();
      }
    } finally {
      releaseClarification();
      await provider.close();
    }
  });
}
