import { readFileSync } from "node:fs";
import { test, expect } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import {
  AUTHORING_TOOL,
  FINAL_TOOL,
  firstUserText,
  hasTool,
  isAuthoring,
  startLoopProvider,
  tool,
  userTexts,
  type ChatRequest,
  type Reply,
} from "./fixtures/loop-provider";

const SUMMARY_GOAL = "Summarize the supplied evidence and note uncertainty";
const GUIDANCE_GOAL = "Wait for task guidance";
const HELD_GOAL = "Wait while I review the task";
const RATE_LIMITED_GOAL = "Wait for provider recovery";
const GUIDANCE = "Explain the uncertainty before concluding.";
const SUMMARY_ANSWER =
  "The supplied evidence supports further investigation, with uncertainty noted.";
const GUIDED_ANSWER =
  "Guidance considered: uncertainty explained before concluding.";
const HOST_UNAVAILABLE =
  "The agent's step host is unavailable, so the run stopped after retrying.";
const ANALYSIS =
  "# Analysis\nThe supplied evidence supports further investigation.\nUncertainty remains explicit.\n";
const analysisFile = {
  name: "analysis.md",
  mediaType: "text/markdown",
  text: ANALYSIS,
};

function finalAnswer(answer: string, files?: unknown[]): Reply {
  return {
    toolCalls: [
      {
        name: FINAL_TOOL,
        args: files ? { answer, generatedFiles: files } : { answer },
      },
    ],
  };
}

// The guidance step must see the guidance after the first step's final
// answer call and its tool reply: the kernel re-checks interventions before
// it accepts a final answer (D8).
function expectGuidanceAfterRejectedFinal(r: ChatRequest) {
  const finalAt = r.messages.findIndex(
    (m) =>
      m.role === "assistant" &&
      (m.tool_calls ?? []).some((call) => call.function.name === FINAL_TOOL),
  );
  const replyAt = r.messages.findIndex(
    (m, i) => i > finalAt && m.role === "tool",
  );
  const guidanceAt = r.messages.findIndex(
    (m, i) =>
      i > replyAt &&
      m.role === "user" &&
      typeof m.content === "string" &&
      m.content.includes(GUIDANCE),
  );
  expect(finalAt).toBeGreaterThan(0);
  expect(replyAt).toBeGreaterThan(finalAt);
  expect(guidanceAt).toBeGreaterThan(replyAt);
}

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
    // P4 waits out the step host retries (2 s + 5 s + 15 s).
    test.setTimeout(inactive ? 60000 : 120000);
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
    let releaseHeldTurn: (() => void) | undefined;
    let heldTurnStarted = false;
    let guidanceStarted = false;
    let releaseGuidance: (() => void) | undefined;
    const provider = await startLoopProvider(async (r) => {
      if (isAuthoring(r))
        return { toolCalls: [{ name: AUTHORING_TOOL, args: intent }] };
      // No step here is tool-free: host_unavailable failures are never
      // explained by the model.
      if (!r.tools?.length) throw new Error("unexpected tool-free model step");
      expect(hasTool(r, FINAL_TOOL)).toBe(true);
      const goal = firstUserText(r);
      if (goal.includes(RATE_LIMITED_GOAL)) return { status: 429 };
      if (goal.includes(HELD_GOAL)) {
        heldTurnStarted = true;
        await new Promise<void>((resolve) => {
          releaseHeldTurn = resolve;
        });
        return finalAnswer("Review finished.", [analysisFile]);
      }
      if (goal.includes(GUIDANCE_GOAL)) {
        if (!userTexts(r).some((text) => text.includes(GUIDANCE))) {
          guidanceStarted = true;
          await new Promise<void>((resolve) => {
            releaseGuidance = resolve;
          });
          return finalAnswer("Initial answer.");
        }
        expectGuidanceAfterRejectedFinal(r);
        return finalAnswer(GUIDED_ANSWER, [analysisFile]);
      }
      if (goal.includes(SUMMARY_GOAL))
        return finalAnswer(SUMMARY_ANSWER, [analysisFile]);
      throw new Error(`unexpected model step for goal ${JSON.stringify(goal)}`);
    });
    // Model steps of one task, by its goal (authoring and tool-free calls
    // excluded).
    const loopCalls = (goal: string) =>
      provider.calls.filter(
        (r) =>
          !isAuthoring(r) &&
          !!r.tools?.length &&
          firstUserText(r).includes(goal),
      );
    try {
      expect(
        (
          await request.post("/__desktop/provider", {
            data: {
              baseUrl: provider.baseUrl,
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
        const summaryCreated = page.waitForResponse(
          (response) =>
            response.request().method() === "POST" &&
            response.url().endsWith("/api/v1/agent-runs"),
        );
        await page.locator('form button[type="submit"]').click();
        const summaryRun = (await (await summaryCreated).json()).run;
        const inspector = page.getByRole("complementary", {
          name: "Work details",
        });
        await expect(
          inspector.getByText(SUMMARY_ANSWER, { exact: true }),
        ).toBeVisible({ timeout: 30000 });
        await expect(
          inspector.getByText("Completed", { exact: true }),
        ).toBeVisible();
        // P1: one model step that called the final answer tool with a file.
        const summaryCalls = loopCalls(SUMMARY_GOAL);
        expect(summaryCalls).toHaveLength(1);
        expect(
          tool(summaryCalls[0], FINAL_TOOL)?.function.parameters.properties,
        ).toHaveProperty("generatedFiles");
        const summarySteps = await (
          await request.get(
            `/api/v1/agent-turns?scopeKind=local&scopeId=default&runId=${summaryRun.id}`,
          )
        ).json();
        expect(summarySteps).toHaveLength(1);
        expect(summarySteps[0]).toMatchObject({
          toolCallCount: 1,
          status: "completed",
        });
        expect(summarySteps[0]).not.toHaveProperty("decisions");
        expect(summarySteps[0]).not.toHaveProperty("requestedActions");
        const summaryArtifacts = await (
          await request.get(
            `/api/v1/artifacts?scopeKind=local&scopeId=default&producerRunId=${summaryRun.id}`,
          )
        ).json();
        expect(summaryArtifacts).toHaveLength(1);
        expect(summaryArtifacts[0].provenance.turnId).toBe(summarySteps[0].id);
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
          ANALYSIS,
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
          .fill(GUIDANCE_GOAL);
        await page.locator('form button[type="submit"]').click();
        await expect.poll(() => guidanceStarted).toBe(true);
        await inspector.getByRole("button", { name: "Add guidance" }).click();
        await inspector.getByLabel("Guidance for this task").fill(GUIDANCE);
        await inspector
          .getByRole("button", { name: "Save guidance", exact: true })
          .click();
        await expect(inspector.getByText(/Guidance saved/)).toBeFocused();
        releaseGuidance?.();
        await expect(
          inspector.getByText(GUIDED_ANSWER, { exact: true }),
        ).toBeVisible({ timeout: 30000 });
        // P2: the first final answer was refused for the new guidance and
        // never shown; the second step saw the guidance.
        expect(loopCalls(GUIDANCE_GOAL)).toHaveLength(2);
        await expect(
          inspector.getByText("Initial answer.", { exact: true }),
        ).toHaveCount(0);
        await inspector.getByText("Artifacts", { exact: true }).click();
        await expect(inspector.locator(".artifact-list li")).toHaveCount(1);
        await page
          .getByRole("navigation")
          .getByRole("button", { name: /Home/ })
          .click();
        await page
          .getByRole("textbox", { name: "Describe the work" })
          .fill(HELD_GOAL);
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
          .fill(RATE_LIMITED_GOAL);
        const rateLimitedCreated = page.waitForResponse(
          (response) =>
            response.request().method() === "POST" &&
            response.url().endsWith("/api/v1/agent-runs"),
        );
        await page.locator('form button[type="submit"]').click();
        const rateLimitedRun = (await (await rateLimitedCreated).json()).run;
        // P4: every model step gets HTTP 429, which the step host reports as
        // unavailable. The kernel retries (2 s, 5 s, 15 s), then fails the Run
        // with host_unavailable and no model explanation.
        await expect(
          inspector.getByText(/This attempt ended with an error/),
        ).toBeVisible({ timeout: 40000 });
        await expect(
          inspector.getByText(HOST_UNAVAILABLE, { exact: true }),
        ).toBeVisible();
        const failedRun = await (
          await request.get(
            `/api/v1/agent-runs/${rateLimitedRun.id}?scopeKind=local&scopeId=default`,
          )
        ).json();
        expect(failedRun.status).toBe("failed");
        expect(failedRun.failureExplanation?.code).toBe("host_unavailable");
        expect(failedRun.failureExplanation?.explained).toBeFalsy();
        expect(failedRun.failureExplanation?.stepError).toContain("HTTP 429");
        // The first attempt plus three retries.
        expect(loopCalls(RATE_LIMITED_GOAL)).toHaveLength(4);
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
        const callsBeforeActivation = provider.calls.length;
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
        expect(provider.calls.length).toBe(callsBeforeActivation);
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
      expect(provider.errors).toEqual([]);
    } finally {
      releaseHeldTurn?.();
      releaseGuidance?.();
      await provider.close();
    }
  });
}
