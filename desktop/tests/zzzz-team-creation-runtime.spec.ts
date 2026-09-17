import { test, expect } from "@playwright/test";
import { createServer } from "node:http";
import { execFileSync } from "node:child_process";
import { join } from "node:path";
import AxeBuilder from "@axe-core/playwright";

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

    const provider = createServer(async (req, res) => {
      let body = "";
      for await (const chunk of req) body += chunk;
      const input = JSON.parse(body);
      if (input.tools[0].function.name === "submit_agent_turn") {
        const turn = JSON.parse(input.messages[1].content);
        let form: any = {
          schemaVersion: "openseal.hosted-turn-form/v1",
          nextRunStatus: "completed",
          outputSummary: "Evidence checked",
          runOutput: {
            reply: "Team evidence review completed with uncertainty noted.",
          },
        };
        if (turn.inputContext?.agentRequestInbox) {
          const requestID = turn.inputContext.agentRequestInbox.requestId;
          const seen = intakeSeen.has(requestID);
          intakeSeen.add(requestID);
          form.runOutput = {
            agentRequestDecision: {
              decision: seen ? "accept" : "request_clarification",
              message: seen
                ? "Reporting period is clear."
                : "Which reporting period should I use?",
            },
          };
        } else if (turn.inputContext?.agentRequestCompletionReview)
          form.runOutput = {
            agentRequestCompletionReviewDecision: {
              decision: "approve",
              message: "The result addresses the delegated goal.",
            },
          };
        else if (
          turn.goal === "Coordinate evidence review." &&
          !turn.continuationCheckpoint?.delegated
        ) {
          expect(turn.eligibleAgents).toHaveLength(1);
          form = {
            ...form,
            nextRunStatus: "running",
            runOutput: {},
            continuationCheckpoint: { delegated: true },
            proposedDelegation: {
              stepId: "review-evidence",
              assignedAgentId: turn.eligibleAgents[0].id,
              goal: "Independently check the evidence.",
              checkpoint: {},
              budget: {
                ...turn.budget.minimumChild,
                maxTurns: 6,
                maxAttempts: 8,
                maxTotalTokens: Math.max(
                  64000,
                  turn.budget.minimumChild.maxTotalTokens,
                ),
              },
            },
          };
        }
        if (
          turn.goal === "Coordinate evidence review." &&
          turn.continuationCheckpoint?.delegated &&
          !turn.continuationCheckpoint?.clarified
        ) {
          waitingForGuidance = true;
          await clarificationGate;
          if (turn.pendingInterventions?.length) {
            expect(turn.pendingInterventions[0].instruction).toContain(
              "July reporting period",
            );
            form = {
              ...form,
              nextRunStatus: "running",
              runOutput: {},
              continuationCheckpoint: { delegated: true, clarified: true },
              proposedDelegation: {
                stepId: "review-evidence",
                assignedAgentId: turn.eligibleAgents[0].id,
                goal: "Independently check the evidence.",
                clarification: "Use the July reporting period.",
                checkpoint: {},
                budget: { ...turn.budget.minimumChild },
              },
            };
          }
        }
        res.setHeader("Content-Type", "application/json");
        res.end(
          JSON.stringify({
            choices: [
              {
                finish_reason: "tool_calls",
                message: {
                  tool_calls: [
                    {
                      id: "team-work-fixture",
                      type: "function",
                      function: {
                        name: "submit_agent_turn",
                        arguments: JSON.stringify(form),
                      },
                    },
                  ],
                },
              },
            ],
          }),
        );
        return;
      }
      expect(input.tools[0].function.name).toBe("submit_authoring_intent");
      expect(JSON.parse(input.messages[1].content).prompt).toContain(
        "Create one team.",
      );
      generationCalls++;
      res.setHeader("Content-Type", "application/json");
      res.end(
        JSON.stringify({
          choices: [
            {
              finish_reason: "tool_calls",
              message: {
                tool_calls: [
                  {
                    id: "team-fixture",
                    type: "function",
                    function: {
                      name: "submit_authoring_intent",
                      arguments: JSON.stringify(intent),
                    },
                  },
                ],
              },
            },
          ],
        }),
      );
    });
    await new Promise<void>((resolve) =>
      provider.listen(0, "127.0.0.1", resolve),
    );
    try {
      const address = provider.address() as { port: number };
      expect(
        (
          await request.post("/__desktop/provider", {
            data: {
              baseUrl: `http://127.0.0.1:${address.port}/v1`,
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
          .fill("Coordinate evidence review.");
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
          page.getByText("Which reporting period should I use?", {
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
              const root = rows.find(
                (r: any) => r.goal === "Coordinate evidence review.",
              );
              return root?.status === "failed" ? root.error : root?.status;
            },
            { timeout: 30000 },
          )
          .toBe("completed");
        const rows = await (await request.get(workPath)).json();
        const root = rows.find(
          (r: any) => r.goal === "Coordinate evidence review.",
        );
        expect(root.output.reply).toContain("uncertainty noted");
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
      await new Promise<void>((resolve, reject) => {
        provider.close((error) => (error ? reject(error) : resolve()));
        provider.closeAllConnections();
      });
    }
  });
}
