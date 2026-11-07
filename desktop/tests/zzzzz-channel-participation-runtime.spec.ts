import { test, expect } from "@playwright/test";
import { execFileSync } from "node:child_process";
import { join } from "node:path";
import {
  FINAL_TOOL,
  ROUTE_TOOL,
  hasTool,
  lastUserText,
  startLoopProvider,
  tool,
} from "./fixtures/loop-provider";

const REPLY = "Synthetic channel reply: uncertainty remains explicit.";

test("real daemon replies only to new opted-in messages and persists the choice", async ({
  page,
  request,
}) => {
  test.setTimeout(60000);
  // The newest channel message each member step answered.
  const calls: string[] = [];
  const provider = await startLoopProvider((r) => {
    // The desktop Team router picks the member; any member will do.
    if (hasTool(r, ROUTE_TOOL)) {
      const members = tool(r, ROUTE_TOOL)!.function.parameters.properties
        .agentId.enum;
      return {
        toolCalls: [
          {
            name: ROUTE_TOOL,
            args: { agentId: members[0], reason: "reviews evidence" },
          },
        ],
      };
    }
    // A member's model step: plain text is its final answer (D5).
    expect(hasTool(r, FINAL_TOOL)).toBe(true);
    calls.push(lastUserText(r));
    return { text: REPLY };
  });
  try {
    const baseUrl = provider.baseUrl;
    expect(
      (
        await request.post("/__desktop/provider", {
          data: {
            baseUrl,
            model: "synthetic-channel",
            apiKey: "synthetic-channel-key",
          },
        })
      ).ok(),
    ).toBe(true);
    execFileSync(
      "go",
      [
        "run",
        "tests/seed_participation.go",
        "-db",
        join(process.env.OPENSEAL_UI_TEST_WORKSPACE!, "data/openseal.db"),
      ],
      { timeout: 30000 },
    );
    await page.goto("/");
    await expect(
      page.getByText("Connected locally", { exact: true }),
    ).toBeVisible();
    await page.keyboard.press("Control+5");
    await page
      .getByRole("button", { name: /Channel reply team.*active/ })
      .click();
    await page.getByRole("button", { name: "Channels", exact: true }).click();
    await page
      .getByRole("button", { name: "New channel", exact: true })
      .click();
    await page
      .getByLabel("Channel name", { exact: true })
      .fill("Live channel review");
    await page
      .getByRole("button", { name: "Create channel", exact: true })
      .click();
    const thread = page.getByRole("region", {
      name: "Channel conversation",
      exact: true,
    });
    const post = async (text: string) => {
      await page
        .getByLabel("Message to this channel", { exact: true })
        .fill(text);
      await page
        .getByRole("button", { name: "Post message", exact: true })
        .click();
      await expect(thread.getByText(text, { exact: true })).toBeVisible();
    };
    await post("Before opt-in: keep this as context.");
    await thread
      .getByRole("button", { name: "Channel settings", exact: true })
      .click();
    await thread
      .getByRole("button", { name: "Enable team replies", exact: true })
      .click();
    await expect(
      thread.getByText("Team replies are on.", { exact: true }),
    ).toBeVisible();
    await post("After opt-in: review this evidence.");
    await expect(thread.getByText(REPLY, { exact: true })).toBeVisible({
      timeout: 15000,
    });
    // One member step, for the opted-in message. The chat step renders the
    // message with its header line, so match its text, not the whole entry.
    expect(calls).toHaveLength(1);
    expect(calls[0]).toContain("After opt-in: review this evidence.");
    const channels = await (
      await request.get(
        "/api/v1/conversations?scopeKind=local&scopeId=default&ownerType=team&ownerId=ui-participation-team",
      )
    ).json();
    const channel = channels.find(
      (c: any) => c.title === "Live channel review",
    );
    expect(channel.participation).toEqual({ enabled: true, afterSequence: 1 });
    const runs = await (
      await request.get(
        "/api/v1/agent-runs?scopeKind=local&scopeId=default&ownerType=team&ownerId=ui-participation-team",
      )
    ).json();
    // The replying Run is the one with a model step (a routing Run's steps
    // are deterministic); its step carries the fake provider's usage.
    let run: any;
    for (const candidate of runs.filter(
      (r: any) => r.context?.conversationId === channel.id,
    )) {
      const steps = (
        await (
          await request.get(
            `/api/v1/agent-turns?scopeKind=local&scopeId=default&runId=${candidate.id}`,
          )
        ).json()
      ).filter((step: any) => step.producer === "model");
      if (!steps.length) continue;
      expect(run).toBeUndefined();
      run = candidate;
      expect(steps).toHaveLength(1);
      expect(steps[0]).toMatchObject({
        status: "completed",
        toolCallCount: 0,
        final: true,
        usage: { inputTokens: 100, outputTokens: 80 },
      });
    }
    expect(run).toBeDefined();
    expect(run.status).toBe("completed");
    const activity = thread.getByRole("region", {
      name: "Reply activity",
      exact: true,
    });
    await activity.getByRole("button", { name: "Show reply activity" }).click();
    await expect(activity).toContainText("Completed — 1 message posted", {
      timeout: 10000,
    });
    await activity.getByRole("button", { name: "Triggering message" }).click();
    await expect(activity).toContainText("After opt-in: review this evidence.");
    await activity
      .getByRole("button", { name: "Close message", exact: true })
      .click();
    await activity.getByRole("button", { name: "Open reply details" }).click();
    await expect(page.locator(".inspector")).toContainText(run.goal);
    await page.locator(".inspector button[title]").click();
    await activity.getByRole("button", { name: "Hide reply activity" }).click();
    await thread
      .getByRole("button", { name: "Disable team replies", exact: true })
      .click();
    await expect(
      thread.getByText("Team replies are off.", { exact: true }),
    ).toBeVisible();
    await post("After opt-out: keep this as a note.");
    // Saving provider settings restarts the owned daemon with the same workspace.
    expect(
      (
        await request.post("/__desktop/provider", {
          data: { baseUrl, model: "synthetic-channel", apiKey: "" },
        })
      ).ok(),
    ).toBe(true);
    await page.reload();
    await page.keyboard.press("Control+5");
    await page
      .getByRole("button", { name: /Channel reply team.*active/ })
      .click();
    await page.getByRole("button", { name: "Channels", exact: true }).click();
    await page.getByRole("button", { name: /^Live channel review/ }).click();
    await expect(
      page.getByText("Team replies are off.", { exact: true }),
    ).toBeVisible();
    await expect(
      page.getByText("After opt-out: keep this as a note.", { exact: true }),
    ).toBeVisible();
    const saved = await (
      await request.get(
        `/api/v1/conversations/${channel.id}?scopeKind=local&scopeId=default`,
      )
    ).json();
    expect(saved.participation.enabled).toBe(false);
    expect(calls).toHaveLength(1);
    expect(provider.errors).toEqual([]);
  } finally {
    await provider.close();
  }
});
