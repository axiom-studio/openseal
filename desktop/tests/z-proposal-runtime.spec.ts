import { readFileSync } from "node:fs";
import { test, expect } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";

test("real daemon generation failure can be retried from its saved proposal", async ({
  page,
  request,
}) => {
  test.setTimeout(60000);
  const configured = await request.post("/__desktop/provider", {
    data: {
      baseUrl: "http://127.0.0.1:19999/v1",
      model: "synthetic-test-model",
      apiKey: "synthetic-test-key",
    },
  });
  expect(configured.ok()).toBe(true);
  const creates: { key: string; body: unknown; id: string }[] = [];
  await page.route(
    "**/api/v1/authoring/workforce/change-sets",
    async (route) => {
      const response = await route.fetch();
      const saved = await response.json();
      creates.push({
        key: route.request().headers()["idempotency-key"],
        body: route.request().postDataJSON(),
        id: saved.id,
      });
      if (creates.length === 1) return route.abort("failed");
      return route.fulfill({ response });
    },
  );
  await page.goto("/");
  await expect(
    page.getByText("Connected locally", { exact: true }),
  ).toBeVisible();
  await page
    .getByRole("textbox", { name: "Describe your agent" })
    .fill("Create a research reviewer for a local integration test.");
  await page
    .getByRole("button", { name: "Create proposal", exact: true })
    .click();
  await expect(
    page.getByRole("region", { name: "Saved request recovery" }),
  ).toContainText("Could not confirm");
  await page.reload();
  await page
    .getByRole("button", { name: "Retry saved request", exact: true })
    .click();
  await expect
    .poll(() =>
      page.evaluate(() => localStorage.getItem("openseal.home-submission")),
    )
    .toBeNull();
  expect(creates).toHaveLength(2);
  expect(creates[1]).toEqual(creates[0]);
  await expect(
    page.getByText("Generation failed", { exact: true }),
  ).toBeVisible({ timeout: 30000 });
  const id = await page.evaluate(() =>
    localStorage.getItem("openseal.proposal"),
  );
  expect(id).toBeTruthy();
  const path = `/api/v1/authoring/workforce/change-sets/${id}?scopeKind=local&scopeId=default`;
  const before = await (await request.get(path)).json();
  expect(before.generation.lastError).toBeTruthy();
  await expect(
    page.getByText(before.generation.lastError, { exact: true }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Retry generation", exact: true })
    .click();
  await expect(
    page.getByText(
      "Generation restarted. You can leave this page while it runs.",
    ),
  ).toBeVisible();
  await expect
    .poll(
      async () => (await (await request.get(path)).json()).generation.attempt,
      { timeout: 30000 },
    )
    .toBeGreaterThan(before.generation.attempt);
  await page.reload();
  await expect(
    page.getByRole("heading", { name: "Agent proposal", exact: true }),
  ).toBeVisible();
  expect(
    await page.evaluate(() => localStorage.getItem("openseal.proposal")),
  ).toBe(id);
});

for (const inactive of [false, true]) {
  test(`real daemon installs a reviewed ${inactive ? "inactive" : "active"} agent`, async ({
    page,
    request,
  }) => {
    test.setTimeout(60000);
    const { createServer } = await import("node:http");
    const agentName = inactive ? "Draft analyst" : "Evidence analyst";
    const intent = {
      schemaVersion: "openseal.authoring-intent/v4",
      kind: "agent",
      name: agentName,
      purpose: "Analyze supplied evidence",
      agents: [
        {
          key: inactive ? "draft-analyst" : "evidence-analyst",
          name: agentName,
          purpose: "Analyze supplied evidence",
          behavior: "Analyze supplied evidence accurately. Report uncertainty.",
        },
      ],
    };
    let generationCalls = 0;
    let releaseHeldTurn: (() => void) | undefined;
    let heldTurnStarted = false;
    let guidanceStarted = false;
    let releaseGuidance: (() => void) | undefined;
    let guidanceTurns = 0;
    const provider = createServer(async (req, res) => {
      generationCalls += 1;
      let body = "";
      for await (const chunk of req) body += chunk;
      const payload = JSON.parse(body);
      if (payload.tools[0].function.name === "submit_agent_turn") {
        const taskInput = JSON.parse(payload.messages[1].content);
        const guided = taskInput.goal === "Wait for task guidance";
        if (guided) {
          guidanceTurns++;
          if (!taskInput.pendingInterventions?.length) {
            guidanceStarted = true;
            await new Promise<void>((resolve) => {
              releaseGuidance = resolve;
            });
          } else {
            expect(taskInput.pendingInterventions[0].instruction).toBe(
              "Explain the uncertainty before concluding.",
            );
          }
        }
        if (
          JSON.parse(payload.messages[1].content).goal ===
          "Wait for provider recovery"
        ) {
          res.writeHead(429, { "Content-Type": "application/json" });
          res.end(JSON.stringify({ error: "Synthetic provider rate limit" }));
          return;
        }

        if (
          JSON.parse(payload.messages[1].content).goal ===
          "Wait while I review the task"
        ) {
          heldTurnStarted = true;
          await new Promise<void>((resolve) => {
            releaseHeldTurn = resolve;
          });
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
                      id: "fixture-turn",
                      type: "function",
                      function: {
                        name: "submit_agent_turn",
                        arguments: JSON.stringify({
                          schemaVersion: "openseal.hosted-turn-form/v1",
                          skillSelections: [],
                          decisions: [],
                          outputSummary: "Evidence reviewed",
                          continuationCheckpoint: {},
                          nextRunStatus: "completed",
                          runOutput: {
                            reply:
                              guided && taskInput.pendingInterventions?.length
                                ? "Guidance considered: uncertainty explained before concluding."
                                : "The supplied evidence supports further investigation, with uncertainty noted.",
                            generatedFiles: [
                              {
                                name: "analysis.md",
                                mediaType: "text/markdown",
                                text: "# Analysis\nThe supplied evidence supports further investigation.\nUncertainty remains explicit.\n",
                              },
                            ],
                          },
                          runError: "",
                          completionEvidenceRefs: [],
                          evidenceClaims: [],
                        }),
                      },
                    },
                  ],
                },
              },
            ],
            usage: { prompt_tokens: 100, completion_tokens: 80 },
          }),
        );
        return;
      }
      expect(payload.tools[0].function.name).toBe("submit_authoring_intent");
      res.setHeader("Content-Type", "application/json");
      res.end(
        JSON.stringify({
          choices: [
            {
              finish_reason: "tool_calls",
              message: {
                tool_calls: [
                  {
                    id: "fixture-intent",
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
              model: "synthetic-installation-model",
              apiKey: "synthetic-installation-key",
            },
          })
        ).ok(),
      ).toBe(true);
      await page.goto("/");
      await expect(
        page.getByText("Connected locally", { exact: true }),
      ).toBeVisible();
      await page
        .getByRole("textbox", { name: "Describe your agent" })
        .fill(
          inactive
            ? `Create an inactive ${agentName} agent.`
            : `Create an ${agentName} agent.`,
        );
      await page
        .getByRole("button", { name: "Create proposal", exact: true })
        .click();
      await expect(
        page.getByRole("heading", { name: agentName, exact: true }),
      ).toBeVisible({ timeout: 30000 });
      await page
        .getByRole("button", { name: "Check installation", exact: true })
        .click();
      await expect(
        page
          .getByRole("status")
          .filter({ hasText: "Installation checks finished" }),
      ).toBeFocused();
      const approve = page.getByRole("button", {
        name: "Approve installation",
        exact: true,
      });
      await expect(approve).toBeDisabled();
      await page
        .getByRole("checkbox", { name: "I reviewed the proposed resources" })
        .check();
      await expect(approve).toBeEnabled();
      if (!inactive) {
        const toast = page.getByRole("button", {
          name: "Dismiss notification",
          exact: true,
        });
        if (await toast.count()) await toast.click();
        expect(
          (
            await new AxeBuilder({ page })
              .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
              .analyze()
          ).violations,
        ).toEqual([]);
        await page.evaluate(() => {
          (document.activeElement as HTMLElement)?.blur();
          window.scrollTo(0, 0);
        });
        await page.screenshot({
          path: "../.impeccable/review/installation-review.png",
          fullPage: true,
        });
      }
      await approve.click();
      const install = page.getByRole("button", {
        name: inactive ? "Install without activating" : "Install and activate",
        exact: true,
      });
      await expect(install).toBeVisible();
      await expect(
        page.getByRole("status").filter({ hasText: "Approval saved" }),
      ).toBeFocused();
      await install.click();
      await expect(
        page.getByRole("heading", { name: "Installed", exact: true }),
      ).toBeVisible();
      await expect(
        page.getByRole("heading", { name: "Installed", exact: true }),
      ).toBeFocused();
      const id = await page.evaluate(() =>
        localStorage.getItem("openseal.proposal"),
      );
      const result = await (
        await request.get(
          `/api/v1/authoring/workforce/change-sets/${id}?scopeKind=local&scopeId=default`,
        )
      ).json();
      expect(result.status).toBe("applied");
      expect(result.applyReceipt.activation).toBe(
        inactive ? "inactive" : "active",
      );
      expect(result.approvalDecisions[0].actor.id).toBe("local-operator");
      expect(result.evaluations[0].actor.id).toBe("local-desktop-policy");
      await page
        .getByRole("button", { name: "View agents", exact: true })
        .click();
      await expect(
        page.getByRole("button").filter({ hasText: agentName }).first(),
      ).toBeVisible();
      if (!inactive) {
        await page
          .getByRole("button")
          .filter({ hasText: agentName })
          .first()
          .click();
        await page
          .getByRole("complementary", { name: "Agent details" })
          .getByRole("button", { name: "Start work", exact: true })
          .click();
        await page
          .getByRole("textbox", { name: "Describe the work" })
          .fill("Summarize the supplied evidence and note uncertainty.");
        await page.screenshot({
          path: "../.impeccable/review/generated-files-composer-desktop.png",
          fullPage: true,
        });
        await page.setViewportSize({ width: 390, height: 844 });
        await page.screenshot({
          path: "../.impeccable/review/generated-files-composer-mobile.png",
          fullPage: true,
        });
        await page.setViewportSize({ width: 1440, height: 1000 });
        await page.locator('form button[type="submit"]').click();
        const inspector = page.getByRole("complementary", {
          name: "Work details",
        });
        await expect(
          inspector.getByText(
            "The supplied evidence supports further investigation, with uncertainty noted.",
            { exact: true },
          ),
        ).toBeVisible({ timeout: 30000 });
        await expect(
          inspector.getByText("Completed", { exact: true }),
        ).toBeVisible();
        await expect(
          inspector.getByRole("button", { name: "Copy result", exact: true }),
        ).toBeVisible();
        await inspector.getByText("Artifacts", { exact: true }).click();
        const artifactRow = inspector
          .locator(".artifact-list li")
          .filter({ hasText: "analysis.md" });
        await expect(artifactRow).toHaveCount(1);
        await artifactRow.getByRole("button", { name: "Preview text" }).click();
        await expect(
          artifactRow.getByLabel("Text preview of analysis.md"),
        ).toContainText("Uncertainty remains explicit.");
        await expect(
          artifactRow.getByLabel("Text preview of analysis.md"),
        ).toBeFocused();
        const generatedDownload = page.waitForEvent("download");
        await artifactRow.getByRole("button", { name: "Save as…" }).click();
        const generatedFile = await generatedDownload;
        expect(generatedFile.suggestedFilename()).toBe("analysis.md");
        expect(readFileSync((await generatedFile.path())!, "utf8")).toBe(
          "# Analysis\nThe supplied evidence supports further investigation.\nUncertainty remains explicit.\n",
        );
        await inspector.getByText("Artifacts", { exact: true }).click();
        await page.evaluate(() => {
          (document.activeElement as HTMLElement)?.blur();
          window.scrollTo(0, 0);
        });
        await page.screenshot({
          path: "../.impeccable/review/work-result-desktop.png",
          fullPage: true,
        });
        await page.setViewportSize({ width: 390, height: 844 });
        await page.screenshot({
          path: "../.impeccable/review/work-result-mobile.png",
          fullPage: true,
        });
        expect(
          (
            await new AxeBuilder({ page })
              .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
              .analyze()
          ).violations,
        ).toEqual([]);
        await page.setViewportSize({ width: 1440, height: 1000 });
        const historyResponse = page.waitForResponse(
          (response) =>
            response.url().includes("/activity?") &&
            response.request().method() === "GET",
        );
        await inspector.getByText("Activity history", { exact: true }).click();
        const history = await (await historyResponse).json();
        expect(history.items.length).toBeGreaterThan(0);
        const recorded = inspector.getByRole("list", {
          name: "Recorded activity",
        });
        await expect(recorded.getByRole("listitem")).toHaveCount(
          history.items.length,
        );
        for (const event of history.items)
          await expect(recorded).toContainText(event.summary);
        await page
          .getByRole("searchbox", { name: "Search work" })
          .fill("no such recorded task");
        await expect(
          page.getByRole("heading", { name: "No work matches this view" }),
        ).toBeVisible();
        await page
          .getByRole("searchbox", { name: "Search work" })
          .fill("supplied evidence");
        await page
          .getByRole("combobox", { name: "Work status" })
          .selectOption("completed");
        await expect(
          page.getByRole("button", {
            name: /Summarize the supplied evidence and note uncertainty/,
          }),
        ).toBeVisible();

        await page
          .getByRole("navigation")
          .getByRole("button", { name: /Home/ })
          .click();
        await page
          .getByRole("textbox", { name: "Describe the work" })
          .fill("Wait for task guidance");
        await page.locator('form button[type="submit"]').click();
        await expect.poll(() => guidanceStarted).toBe(true);
        await inspector.getByRole("button", { name: "Add guidance" }).click();
        await inspector
          .getByLabel("Guidance for this task")
          .fill("Explain the uncertainty before concluding.");
        await inspector
          .getByRole("button", { name: "Save guidance", exact: true })
          .click();
        await expect(inspector.getByText(/Guidance saved/)).toBeFocused();
        releaseGuidance?.();
        await expect(
          inspector.getByText(
            "Guidance considered: uncertainty explained before concluding.",
            { exact: true },
          ),
        ).toBeVisible({ timeout: 30000 });
        expect(guidanceTurns).toBe(2);
        await inspector.getByText("Artifacts", { exact: true }).click();
        await expect(inspector.locator(".artifact-list li")).toHaveCount(1);
        await page
          .getByRole("navigation")
          .getByRole("button", { name: /Home/ })
          .click();
        await page
          .getByRole("textbox", { name: "Describe the work" })
          .fill("Wait while I review the task");
        const created = page.waitForResponse(
          (response) =>
            response.request().method() === "POST" &&
            response.url().endsWith("/api/v1/agent-runs"),
        );
        await page.locator('form button[type="submit"]').click();
        const heldRun = (await (await created).json()).run;
        await expect.poll(() => heldTurnStarted).toBe(true);
        await inspector
          .getByRole("button", { name: "Cancel work", exact: true })
          .click();
        await expect(
          inspector.getByRole("button", {
            name: "Confirm cancellation",
            exact: true,
          }),
        ).toBeFocused();
        await page.evaluate(() => {
          (document.activeElement as HTMLElement)?.blur();
          window.scrollTo(0, 0);
        });
        await page.screenshot({
          path: "../.impeccable/review/work-cancel-desktop.png",
          fullPage: true,
        });
        await page.setViewportSize({ width: 390, height: 844 });
        await page.screenshot({
          path: "../.impeccable/review/work-cancel-mobile.png",
          fullPage: true,
        });
        expect(
          (
            await new AxeBuilder({ page })
              .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
              .analyze()
          ).violations,
        ).toEqual([]);
        await inspector
          .getByRole("button", { name: "Confirm cancellation", exact: true })
          .click();
        await expect(
          inspector.getByRole("status").filter({
            hasText: "Work canceled. Saved results remain available.",
          }),
        ).toBeFocused();
        releaseHeldTurn?.();
        await expect
          .poll(
            async () => {
              const turns = await (
                await request.get(
                  `/api/v1/agent-turns?scopeKind=local&scopeId=default&runId=${heldRun.id}`,
                )
              ).json();
              return (
                turns.length > 0 &&
                turns.every(
                  (turn: { status: string }) => turn.status !== "running",
                )
              );
            },
            { timeout: 30000 },
          )
          .toBe(true);
        expect(
          (
            await (
              await request.get(
                `/api/v1/agent-runs/${heldRun.id}?scopeKind=local&scopeId=default`,
              )
            ).json()
          ).status,
        ).toBe("canceled");
        expect(
          await (
            await request.get(
              `/api/v1/artifacts?scopeKind=local&scopeId=default&producerRunId=${heldRun.id}`,
            )
          ).json(),
        ).toEqual([]);
        await page.setViewportSize({ width: 1440, height: 1000 });
        await page
          .getByRole("navigation")
          .getByRole("button", { name: /Home/ })
          .click();
        await page
          .getByRole("textbox", { name: "Describe the work" })
          .fill("Wait for provider recovery");
        await page.locator('form button[type="submit"]').click();
        await expect(
          inspector.getByText(/The model provider was unavailable/),
        ).toBeVisible({ timeout: 30000 });
        await expect(
          inspector.getByRole("button", {
            name: "Open provider settings",
            exact: true,
          }),
        ).toBeVisible();
        await expect(inspector.locator("time")).toBeVisible();
        await inspector
          .getByRole("button", { name: "Pause work", exact: true })
          .click();
        await expect(
          inspector.getByRole("status").filter({ hasText: "Work paused." }),
        ).toBeFocused();
        await expect(
          inspector.getByText(/Resume returns it to its previous state/),
        ).toBeVisible();
      }
      await page.reload();
      await expect(
        page.getByRole("heading", { name: "Installed", exact: true }),
      ).toBeVisible();
      if (inactive) {
        await page.setViewportSize({ width: 390, height: 844 });
        await page.evaluate(() => {
          (document.activeElement as HTMLElement)?.blur();
          window.scrollTo(0, 0);
        });
        await page.screenshot({
          path: "../.impeccable/review/installation-mobile.png",
          fullPage: true,
        });
        expect(
          await page.evaluate(
            () => document.documentElement.scrollWidth <= innerWidth,
          ),
        ).toBe(true);
        const callsBeforeActivation = generationCalls;
        const activationKeys: (string | undefined)[] = [];
        await page.route("**/change-sets/*/activation", async (route) => {
          activationKeys.push(route.request().headers()["idempotency-key"]);
          const response = await route.fetch();
          if (activationKeys.length === 1) await route.abort("failed");
          else await route.fulfill({ response });
        });
        await page
          .getByRole("button", { name: "Review activation", exact: true })
          .click();
        await expect(
          page.getByRole("alert").filter({ hasText: /fetch/i }),
        ).toBeVisible();
        await page
          .getByRole("button", { name: "Review activation", exact: true })
          .click();
        await expect(
          page.getByRole("heading", {
            name: "Activate installed resources",
            exact: true,
          }),
        ).toBeVisible();
        await expect(
          page.getByRole("heading", {
            name: "Activation proposal",
            exact: true,
          }),
        ).toBeFocused();
        const childID = await page.evaluate(() =>
          localStorage.getItem("openseal.proposal"),
        );
        expect(childID).not.toBe(id);
        expect(activationKeys).toHaveLength(2);
        expect(activationKeys[0]).toBeTruthy();
        expect(activationKeys[1]).toBe(activationKeys[0]);
        expect(generationCalls).toBe(callsBeforeActivation);
        const childPath = `/api/v1/authoring/workforce/change-sets/${childID}?scopeKind=local&scopeId=default`;
        const child = await (await request.get(childPath)).json();
        expect(child.parentId).toBe(id);
        expect(child.applyReceipt).toBeFalsy();
        expect(
          child.result.candidate.agents.map((a: { id: string }) => a.id),
        ).toEqual(
          result.result.candidate.agents.map((a: { id: string }) => a.id),
        );
        await page.reload();
        await page
          .getByRole("button", { name: "Check activation", exact: true })
          .click();
        const approveActivation = page.getByRole("button", {
          name: "Approve activation",
          exact: true,
        });
        await expect(approveActivation).toBeDisabled();
        await page.evaluate(() => {
          (document.activeElement as HTMLElement)?.blur();
          window.scrollTo(0, 0);
        });
        await page.screenshot({
          path: "../.impeccable/review/activation-approval-mobile.png",
          fullPage: true,
        });
        await page
          .getByRole("checkbox", { name: "I reviewed the proposed resources" })
          .check();
        await approveActivation.click();
        await page
          .getByRole("button", { name: "Activate resources", exact: true })
          .click();
        await expect(
          page.getByRole("heading", { name: "Activated", exact: true }),
        ).toBeFocused();
        const activated = await (await request.get(childPath)).json();
        expect(activated.applyReceipt.activation).toBe("active");
        expect(
          activated.applyReceipt.resources
            .map((r: { id: string }) => r.id)
            .sort(),
        ).toEqual(
          result.applyReceipt.resources.map((r: { id: string }) => r.id).sort(),
        );
        expect(
          (
            await (
              await request.get(
                `/api/v1/authoring/workforce/change-sets/${id}?scopeKind=local&scopeId=default`,
              )
            ).json()
          ).applyReceipt.activation,
        ).toBe("inactive");
        expect(
          (
            await new AxeBuilder({ page })
              .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
              .analyze()
          ).violations,
        ).toEqual([]);
        await page.evaluate(() => {
          (document.activeElement as HTMLElement)?.blur();
          window.scrollTo(0, 0);
        });
        await page.screenshot({
          path: "../.impeccable/review/activation-mobile.png",
          fullPage: true,
        });
        await page.setViewportSize({ width: 1440, height: 1000 });
        await page.evaluate(() => {
          (document.activeElement as HTMLElement)?.blur();
          window.scrollTo(0, 0);
        });
        await page.screenshot({
          path: "../.impeccable/review/activation-desktop.png",
          fullPage: true,
        });
      }
    } finally {
      releaseHeldTurn?.();
      releaseGuidance?.();
      provider.closeAllConnections();
      await new Promise<void>((resolve, reject) =>
        provider.close((error) => (error ? reject(error) : resolve())),
      );
    }
  });
}
