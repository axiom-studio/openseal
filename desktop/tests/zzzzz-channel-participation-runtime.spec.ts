import { test, expect } from "@playwright/test";
import { createServer } from "node:http";
import { execFileSync } from "node:child_process";
import { join } from "node:path";

test("real daemon replies only to new opted-in messages and persists the choice", async ({
  page,
  request,
}) => {
  test.setTimeout(60000);
  const calls: string[] = [];
  const provider = createServer(async (req, res) => {
    let raw = "";
    for await (const chunk of req) raw += chunk;
    const body = JSON.parse(raw),
      input = JSON.parse(body.messages[1].content);
    expect(body.tools[0].function.name).toBe("submit_channel_contribution");
    expect(body.max_tokens).toBe(2048);
    expect(input.role.id).toBe("speaker");
    calls.push(input.trigger.content);
    res.setHeader("Content-Type", "application/json");
    res.end(
      JSON.stringify({
        choices: [
          {
            finish_reason: "tool_calls",
            message: {
              tool_calls: [
                {
                  type: "function",
                  function: {
                    name: "submit_channel_contribution",
                    arguments: JSON.stringify({
                      wantsToSpeak: true,
                      content:
                        "Synthetic channel reply: uncertainty remains explicit.",
                      roleRelevant: true,
                      hasNewInformation: true,
                    }),
                  },
                },
              ],
            },
          },
        ],
        usage: { prompt_tokens: 40, completion_tokens: 20 },
      }),
    );
  });
  await new Promise<void>((resolve) =>
    provider.listen(0, "127.0.0.1", resolve),
  );
  try {
    const baseUrl = `http://127.0.0.1:${(provider.address() as { port: number }).port}/v1`;
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
    await expect(
      thread.getByText(
        "Synthetic channel reply: uncertainty remains explicit.",
        { exact: true },
      ),
    ).toBeVisible({ timeout: 15000 });
    expect(calls).toEqual(["After opt-in: review this evidence."]);
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
    const run = runs.find((r: any) => r.context?.conversationId === channel.id);
    expect(run.status).toBe("completed");
    expect(run.budgetUsage.inputTokens).toBe(40);
    expect(run.budgetUsage.outputTokens).toBe(20);
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
    expect(calls).toEqual(["After opt-in: review this evidence."]);
  } finally {
    provider.closeAllConnections();
    await new Promise<void>((resolve) => provider.close(() => resolve()));
  }
});
