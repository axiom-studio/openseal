import { test, expect, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
const channel = {
  id: "channel",
  title: "Evidence review",
  owner: { type: "team", id: "research" },
  status: "active",
  revision: 2,
  lastSequence: 8,
};
const run = {
  id: "reply-run",
  kind: "conversation",
  scope: { kind: "local", id: "default" },
  context: {
    conversationId: "channel",
    triggerMessageId: "trigger",
    triggerSequence: 8,
  },
  owner: channel.owner,
  goal: "Coordinate Team channel participation",
  status: "paused",
  budgetState: "exhausted",
  revision: 2,
  createdAt: "2026-09-17T10:00:00Z",
  updatedAt: "2026-09-17T10:00:00Z",
};
const trigger = {
  id: "trigger",
  conversationId: "channel",
  sequence: 8,
  sender: { type: "user", id: "local-operator" },
  content: "Compare the evidence and preserve uncertainty.",
  intent: "update",
  audience: { kind: "channel" },
  createdAt: "2026-09-17T10:00:00Z",
};
async function setup(page: Page, available = true) {
  await page.route("**/api/v1/capabilities", (r) =>
    r.fulfill({
      json: {
        capabilities: [
          { id: "team-definitions", available: true, operations: ["list"] },
          { id: "agent-runs", available: true, operations: ["get"] },
          {
            id: "channels",
            available: true,
            operations: [
              "list",
              "get",
              "read",
              "post",
              ...(available ? ["runs"] : []),
            ],
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/team-deployments?*", (r) =>
    r.fulfill({
      json: {
        items: [
          {
            deployment: {
              id: "research",
              activeVersion: "1",
              status: "active",
              revision: 1,
              roster: [],
            },
            definition: {
              displayName: "Evidence team",
              purpose: "Make decisions with traceable evidence.",
              roles: [],
            },
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/conversations?*", (r) =>
    r.fulfill({ json: [channel] }),
  );
  await page.route("**/api/v1/conversations/channel?*", (r) =>
    r.fulfill({ json: channel }),
  );
  await page.route("**/api/v1/conversations/channel/messages?*", (r) =>
    r.fulfill({ json: [trigger] }),
  );
  await page.route("**/api/v1/conversations/channel/messages/trigger?*", (r) =>
    r.fulfill({ json: trigger }),
  );
  await page.route("**/api/v1/agent-runs/reply-run?*", (r) =>
    r.fulfill({ json: run }),
  );
  await page.goto("/");
  await page.keyboard.press("Control+5");
  await page.getByRole("button", { name: /Evidence team.*active/ }).click();
  await page.getByRole("button", { name: "Channels", exact: true }).click();
  await page
    .getByRole("button", { name: "Evidence review", exact: true })
    .click();
  return page.getByRole("region", { name: "Reply activity", exact: true });
}

test("reply activity keeps the draft while opening exact message and current work; captures accessible layouts", async ({
  page,
}) => {
  await page.route("**/api/v1/conversations/channel/runs?*", (r) =>
    r.fulfill({ json: [run] }),
  );
  const activity = await setup(page);
  await expect(activity).toContainText(
    "Latest attempt: Paused — a budget limit needs attention.",
  );
  const draft = page.getByLabel("Message to this channel", { exact: true });
  await draft.fill("Keep my unfinished follow-up.");
  await activity.getByRole("button", { name: "Show reply activity" }).click();
  await activity.getByRole("button", { name: "Triggering message" }).click();
  await expect(activity).toContainText(trigger.content);
  await activity
    .getByRole("button", { name: "Close message", exact: true })
    .click();
  await expect(
    activity.getByRole("button", { name: "Triggering message" }),
  ).toBeFocused();
  await activity
    .getByRole("button", { name: "Open reply details", exact: true })
    .click();
  await expect(page.locator(".inspector")).toContainText(run.goal);
  await page.locator(".inspector button[title]").click();
  await expect(
    activity.getByRole("button", { name: "Open reply details", exact: true }),
  ).toBeFocused();
  await expect(draft).toHaveValue("Keep my unfinished follow-up.");
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await activity.scrollIntoViewIfNeeded();
  await page.screenshot({
    path: "../.impeccable/review/channel-reply-activity-desktop.png",
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await activity.scrollIntoViewIfNeeded();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth > innerWidth,
    ),
  ).toBe(false);
  await page.screenshot({
    path: "../.impeccable/review/channel-reply-activity-mobile.png",
  });
});

test("reply history paginates exact channel and recovers failed reads without losing the draft", async ({
  page,
}) => {
  const offsets: number[] = [];
  let fail = false;
  await page.route("**/api/v1/conversations/channel/runs?*", (r) => {
    const params = new URL(r.request().url()).searchParams;
    expect(params.get("scopeId")).toBe("default");
    expect(params.get("limit")).toBe("6");
    const offset = Number(params.get("offset"));
    offsets.push(offset);
    if (fail)
      return r.fulfill({ status: 503, json: { error: "Temporary outage" } });
    return r.fulfill({
      json:
        offset === 0
          ? Array.from({ length: 6 }, (_, i) => ({
              ...run,
              id: `attempt-${i}`,
              context: { ...run.context, triggerSequence: 8 - i },
            }))
          : [
              {
                ...run,
                id: "last",
                status: "completed",
                output: { speakerCount: 0 },
              },
            ],
    });
  });
  const activity = await setup(page);
  await activity.getByRole("button", { name: "Show reply activity" }).click();
  await expect(activity.getByRole("listitem")).toHaveCount(5);
  await activity.getByRole("button", { name: "Older attempts" }).click();
  await expect(activity).toContainText("Completed — no reply was offered");
  await expect(activity.getByRole("status")).toBeFocused();
  await expect(
    activity.getByRole("button", { name: "Older attempts" }),
  ).toBeDisabled();
  fail = true;
  await activity
    .getByRole("button", { name: "Refresh reply activity" })
    .click();
  await expect(activity.getByRole("alert")).toContainText(
    "Saved activity may be out of date",
  );
  await expect(activity).toContainText("Completed — no reply was offered");
  fail = false;
  await activity.getByRole("button", { name: "Retry reply activity" }).click();
  await expect(activity.getByRole("alert")).toHaveCount(0);
  await activity.getByRole("button", { name: "Newer attempts" }).click();
  await expect(activity.getByRole("listitem")).toHaveCount(5);
  expect(offsets.filter((offset) => offset > 0)).toEqual([5, 5, 5]);
  expect(offsets.at(-1)).toBe(0);
});

test("reply activity rejects foreign history and wrong work identity, and reports stopped attempts honestly", async ({
  page,
}) => {
  let valid = false;
  await page.route("**/api/v1/conversations/channel/runs?*", (r) =>
    r.fulfill({
      json: [
        {
          ...run,
          scope: { kind: "local", id: valid ? "default" : "foreign" },
          status: "completed",
          output: { participationSkipped: true },
        },
      ],
    }),
  );
  const activity = await setup(page);
  await expect(activity.getByRole("alert")).toContainText(
    "does not match this channel",
  );
  valid = true;
  await activity.getByRole("button", { name: "Retry reply activity" }).click();
  await expect(activity).toContainText(
    "Stopped — replies were disabled for this message",
  );
  await activity.getByRole("button", { name: "Show reply activity" }).click();
  await page.route("**/api/v1/agent-runs/reply-run?*", (r) =>
    r.fulfill({ json: { ...run, id: "wrong" } }),
  );
  await activity.getByRole("button", { name: "Open reply details" }).click();
  await expect(activity.getByRole("alert")).toContainText(
    "does not match this reply attempt",
  );
  await expect(activity.getByRole("alert")).toBeFocused();
  await expect(page.locator(".inspector")).toHaveCount(0);
});

test("older hosts do not request unavailable reply activity", async ({
  page,
}) => {
  let reads = 0;
  await page.route("**/api/v1/conversations/channel/runs?*", (r) => {
    reads++;
    return r.fulfill({ json: [] });
  });
  const activity = await setup(page, false);
  await expect(activity).toHaveCount(0);
  expect(reads).toBe(0);
});

test("late inspector responses are ignored after collapsing reply activity", async ({
  page,
}) => {
  await page.route("**/api/v1/conversations/channel/runs?*", (r) =>
    r.fulfill({ json: [run] }),
  );
  const activity = await setup(page);
  await activity.getByRole("button", { name: "Show reply activity" }).click();
  let release!: () => void;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  let started!: () => void;
  const requested = new Promise<void>((resolve) => {
    started = resolve;
  });
  let finished!: () => void;
  const responded = new Promise<void>((resolve) => {
    finished = resolve;
  });
  await page.route("**/api/v1/agent-runs/reply-run?*", async (r) => {
    started();
    await gate;
    await r.fulfill({ json: run });
    finished();
  });
  await activity.getByRole("button", { name: "Open reply details" }).click();
  await requested;
  await activity.getByRole("button", { name: "Hide reply activity" }).click();
  release();
  await responded;
  await activity.getByRole("button", { name: "Show reply activity" }).click();
  await expect(
    activity.getByRole("button", { name: "Open reply details" }),
  ).toBeVisible();
  await expect(page.locator(".inspector")).toHaveCount(0);
});
