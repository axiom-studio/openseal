import { test, expect, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
const team = {
  deployment: {
    id: "team",
    definitionId: "definition",
    scope: { kind: "local", id: "default" },
    activeVersion: "1",
    status: "active",
    revision: 1,
    roster: [{ id: "a", roleId: "research", agentDeploymentId: "lead" }],
  },
  definition: {
    displayName: "Evidence team",
    purpose: "Review sources together.",
    roles: [
      { id: "research", displayName: "Research", purpose: "Find sources." },
    ],
  },
};
const run = {
  id: "work",
  kind: "agent_work",
  owner: { type: "team", id: "team" },
  assignedAgentId: "lead",
  goal: "Compare the supplied evidence.",
  status: "queued",
  revision: 1,
  createdAt: "2026-09-17T00:00:00Z",
  updatedAt: "2026-09-17T00:00:00Z",
};
async function setup(page: Page, create = true, status = "active") {
  await page.route("**/api/v1/capabilities**", (route) =>
    route.fulfill({
      json: {
        capabilities: [
          { id: "team-definitions", available: true, operations: ["list"] },
          { id: "agent-definitions", available: true, operations: ["list"] },
          {
            id: "channels",
            available: true,
            operations: ["list", "get", "read"],
          },
          {
            id: "agent-runs",
            available: true,
            operations: ["list", "get", ...(create ? ["create-team"] : [])],
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/team-deployments?*", (route) =>
    route.fulfill({
      json: {
        items: [{ ...team, deployment: { ...team.deployment, status } }],
      },
    }),
  );
  await page.route("**/api/v1/agent-deployments?*", (route) =>
    route.fulfill({
      json: {
        items: [
          {
            deployment: { id: "lead", rolloutStatus: "active" },
            definition: {
              displayName: "Research lead",
              purpose: "Find evidence.",
            },
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/agent-runs?*", (route) =>
    route.fulfill({ json: [] }),
  );
  await page.route("**/api/v1/agent-runs/work?*", (route) =>
    route.fulfill({ json: run }),
  );
  await page.goto("/");
  await expect(
    page.getByText("Connected locally", { exact: true }),
  ).toBeVisible();
  await page.keyboard.press("Control+5");
  await page
    .getByRole("button", { name: /Evidence team.*Review sources/ })
    .click();
  await page.getByRole("button", { name: "Team work", exact: true }).click();
}
test("team work preserves drafts and recovers uncertain delivery across reload without duplicate requests", async ({
  page,
}) => {
  await setup(page);
  await page.getByLabel("Lead agent", { exact: true }).selectOption("lead");
  await page
    .getByLabel("What should the team accomplish?", { exact: true })
    .fill(run.goal);
  await page.reload();
  await page.keyboard.press("Control+5");
  await page
    .getByRole("button", { name: /Evidence team.*Review sources/ })
    .click();
  await page.getByRole("button", { name: "Team work", exact: true }).click();
  await expect(page.getByLabel("Lead agent", { exact: true })).toHaveValue(
    "lead",
  );
  await expect(
    page.getByLabel("What should the team accomplish?", { exact: true }),
  ).toHaveValue(run.goal);
  const keys: string[] = [];
  await page.route("**/api/v1/agent-runs", (route) => {
    const body = route.request().postDataJSON();
    expect(body.owner).toEqual({ type: "team", id: "team" });
    expect(body.assignedAgentId).toBe("lead");
    keys.push(route.request().headers()["idempotency-key"]);
    return keys.length === 1 ? route.abort() : route.fulfill({ json: { run } });
  });
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.evaluate(() => {
    (document.activeElement as HTMLElement)?.blur();
    window.scrollTo(0, 0);
  });
  await page.screenshot({
    path: "../.impeccable/review/team-work-desktop.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: "../.impeccable/review/team-work-mobile.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page
    .getByRole("button", { name: "Start team work", exact: true })
    .click();
  await expect(page.getByRole("alert")).toContainText("Could not confirm");
  await expect(
    page.getByLabel("What should the team accomplish?", { exact: true }),
  ).toBeDisabled();
  await page.reload();
  await page.keyboard.press("Control+5");
  await page
    .getByRole("button", { name: /Evidence team.*Review sources/ })
    .click();
  await page.getByRole("button", { name: "Team work", exact: true }).click();
  expect(keys).toHaveLength(1);
  await page
    .getByRole("button", { name: "Retry starting work", exact: true })
    .click();
  await expect
    .poll(() =>
      page.evaluate(() => localStorage.getItem("openseal.team-work.team")),
    )
    .toBe(null);
  expect(keys).toHaveLength(2);
  expect(keys[1]).toBe(keys[0]);
});
test("team work enforces readiness and capability while keeping history available", async ({
  page,
}) => {
  await setup(page, true, "paused");
  await page.getByLabel("Lead agent", { exact: true }).selectOption("lead");
  await page
    .getByLabel("What should the team accomplish?", { exact: true })
    .fill("Analyze");
  await expect(
    page.getByRole("button", { name: "Start team work", exact: true }),
  ).toBeDisabled();
  await setup(page, false);
  await expect(
    page.getByRole("button", { name: "Start team work", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("heading", { name: "Team runs", exact: true }),
  ).toBeVisible();
});
test("team run pages are scoped and expose delegated work with explicit refresh failures", async ({
  page,
}) => {
  await setup(page);
  const requests: string[] = [];
  await page.route("**/api/v1/agent-runs?*", (route) => {
    const url = new URL(route.request().url());
    if (url.searchParams.get("ownerType") !== "team")
      return route.fulfill({ json: [] });
    requests.push(url.search);
    return route.fulfill({
      json:
        url.searchParams.get("offset") === "20"
          ? [
              {
                ...run,
                id: "older",
                goal: "Older team result",
                parentRunId: "root",
              },
            ]
          : Array.from({ length: 21 }, (_, i) => ({
              ...run,
              id: `r${i}`,
              goal: `Team result ${i}`,
            })),
    });
  });
  await expect(
    page.getByRole("button", { name: "Older team runs", exact: true }),
  ).toBeEnabled({ timeout: 10000 });
  await page
    .getByRole("button", { name: "Older team runs", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "Older team result", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("Delegated run · queued", { exact: true }),
  ).toBeVisible();
  expect(
    requests.every((q) => new URLSearchParams(q).get("ownerId") === "team"),
  ).toBe(true);
  await page.route("**/api/v1/agent-runs?*", (route) =>
    route.fulfill({ status: 503, json: { error: "temporarily offline" } }),
  );
  await expect(
    page.getByRole("alert").filter({ hasText: "Could not refresh team work" }),
  ).toBeVisible({ timeout: 10000 });
  await expect(
    page.getByRole("button", { name: "Older team result", exact: true }),
  ).toBeVisible();
});

test("rejected team work keeps an editable draft and leaving the team ignores a late start response", async ({
  page,
}) => {
  await setup(page);
  await page.getByLabel("Lead agent", { exact: true }).selectOption("lead");
  await page
    .getByLabel("What should the team accomplish?", { exact: true })
    .fill(run.goal);
  await page.route("**/api/v1/agent-runs", (route) =>
    route.fulfill({ status: 400, json: { error: "Team membership changed" } }),
  );
  await page
    .getByRole("button", { name: "Start team work", exact: true })
    .click();
  await expect(page.getByRole("alert")).toContainText("Work was not started");
  await expect(
    page.getByLabel("What should the team accomplish?", { exact: true }),
  ).toBeEnabled();
  let release!: () => void;
  const gate = new Promise<void>((resolve) => (release = resolve));
  await page.route("**/api/v1/agent-runs", async (route) => {
    await gate;
    await route.fulfill({ json: { run } });
  });
  await page
    .getByRole("button", { name: "Start team work", exact: true })
    .click();
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  release();
  await expect(
    page.getByRole("heading", { name: "Settings", exact: true }),
  ).toBeVisible();
  await expect(page.locator(".inspector")).toHaveCount(0);
});

async function discussion(
  page: Page,
  content = "Compare July’s evidence. Keep uncertainty explicit.",
) {
  const conversation = {
    id: "discussion",
    title: "July source review",
    owner: { type: "team", id: "team" },
    status: "archived",
    revision: 2,
    lastSequence: 1,
  };
  await page.route("**/api/v1/conversations?*", (r) =>
    r.fulfill({ json: [conversation] }),
  );
  await page.route("**/api/v1/conversations/discussion?*", (r) =>
    r.fulfill({ json: conversation }),
  );
  await page.route("**/api/v1/conversations/discussion/messages?*", (r) =>
    r.fulfill({
      json: [
        {
          id: "decision",
          conversationId: "discussion",
          sequence: 1,
          content,
          sender: { type: "user", id: "local-operator" },
          intent: "update",
          audience: { kind: "channel" },
          createdAt: "2026-09-17T00:00:00Z",
        },
      ],
    }),
  );
  await page.getByRole("button", { name: "Channels", exact: true }).click();
  await page.getByLabel("Channel status").selectOption("archived");
  await page
    .getByRole("button", { name: "July source review · Archived", exact: true })
    .click();
}

test("discussion handoff protects existing work drafts and restores message focus", async ({
  page,
}) => {
  await setup(page);
  const goal = page.getByLabel("What should the team accomplish?", {
    exact: true,
  });
  await goal.fill("Keep my separate research plan.");
  await page.getByLabel("Lead agent", { exact: true }).selectOption("lead");
  await discussion(page);
  const trigger = page.getByRole("button", {
    name: "Use as team work",
    exact: true,
  });
  await trigger.click();
  const handoff = page.getByRole("region", { name: "Review channel message" });
  await expect(handoff.getByRole("heading")).toBeFocused();
  await expect(goal).toHaveValue("Keep my separate research plan.");
  await expect(
    page.getByRole("button", { name: "Start team work", exact: true }),
  ).toBeDisabled();
  await handoff.scrollIntoViewIfNeeded();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: "../.impeccable/review/channel-work-desktop.png",
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await handoff.scrollIntoViewIfNeeded();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: "../.impeccable/review/channel-work-mobile.png",
  });
  await page
    .getByRole("button", { name: "Keep draft and return to message" })
    .click();
  await expect(trigger).toBeFocused();
  await expect(goal).toHaveValue("Keep my separate research plan.");
  await trigger.click();
  await page.getByRole("button", { name: "Replace draft and review" }).click();
  await expect(goal).toBeFocused();
  await expect(goal).toHaveValue(
    "Compare July’s evidence. Keep uncertainty explicit.",
  );
  await expect(page.getByLabel("Lead agent", { exact: true })).toHaveValue(
    "lead",
  );
  await page.reload();
  await page.keyboard.press("Control+5");
  await page
    .getByRole("button", { name: /Evidence team.*Review sources/ })
    .click();
  await page.getByRole("button", { name: "Team work", exact: true }).click();
  await expect(goal).toHaveValue(
    "Compare July’s evidence. Keep uncertainty explicit.",
  );
  await expect(handoff).toHaveCount(0);
});

test("discussion handoff only starts governed work after goal review and lead selection", async ({
  page,
}) => {
  await setup(page);
  let writes = 0;
  const goal = "Compare July’s evidence. Keep uncertainty explicit.";
  await page.route("**/api/v1/agent-runs", (r) => {
    writes++;
    const body = r.request().postDataJSON();
    expect(body.goal).toBe(goal);
    expect(body.owner).toEqual({ type: "team", id: "team" });
    expect(body.assignedAgentId).toBe("lead");
    expect(body.context).toBeUndefined();
    expect(r.request().headers()["idempotency-key"]).toBeTruthy();
    return r.fulfill({ json: { run: { ...run, goal } } });
  });
  await page.route("**/api/v1/agent-runs/work?*", (r) =>
    r.fulfill({ json: { ...run, goal } }),
  );
  await discussion(page);
  await page
    .getByRole("button", { name: "Use as team work", exact: true })
    .click();
  expect(writes).toBe(0);
  await page.getByRole("button", { name: "Use message in draft" }).click();
  await expect(
    page.getByLabel("What should the team accomplish?", { exact: true }),
  ).toBeFocused();
  const start = page.getByRole("button", {
    name: "Start team work",
    exact: true,
  });
  await expect(start).toBeDisabled();
  expect(writes).toBe(0);
  await page.getByLabel("Lead agent", { exact: true }).selectOption("lead");
  await start.click();
  await expect(page.locator(".inspector")).toContainText(goal);
  expect(writes).toBe(1);
});

test("pending work cannot be replaced by a message and unavailable creation hides the handoff", async ({
  page,
}) => {
  await page.addInitScript(() =>
    localStorage.setItem(
      "openseal.team-work.team",
      JSON.stringify({
        goal: "Keep the uncertain request.",
        lead: "lead",
        pending: "same-request",
      }),
    ),
  );
  await setup(page);
  await discussion(page);
  await page
    .getByRole("button", { name: "Use as team work", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "Replace draft and review" }),
  ).toBeDisabled();
  await expect(
    page.getByLabel("What should the team accomplish?", { exact: true }),
  ).toHaveValue("Keep the uncertain request.");
  await expect(
    page.getByLabel("What should the team accomplish?", { exact: true }),
  ).toBeDisabled();
  await page
    .getByRole("button", { name: "Keep draft and return to message" })
    .click();
  await setup(page, false);
  await discussion(page);
  await expect(
    page.getByRole("button", { name: "Use as team work", exact: true }),
  ).toHaveCount(0);
});

test("oversized channel messages offer a complete shorter-goal recovery without replacing the draft", async ({
  page,
}) => {
  await setup(page);
  const goal = page.getByLabel("What should the team accomplish?", {
    exact: true,
  });
  await goal.fill("Existing plan worth keeping.");
  await page.getByLabel("Lead agent", { exact: true }).selectOption("lead");
  await discussion(page, "Long evidence note. ".repeat(900));
  await page
    .getByRole("button", { name: "Use as team work", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "Replace draft and review" }),
  ).toBeDisabled();
  const shorter = page.getByRole("button", { name: "Write a shorter goal" });
  await shorter.scrollIntoViewIfNeeded();
  await page.screenshot({
    path: "../.impeccable/review/channel-work-desktop.png",
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await shorter.scrollIntoViewIfNeeded();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: "../.impeccable/review/channel-work-mobile.png",
  });
  await shorter.click();
  await expect(goal).toBeFocused();
  await expect(goal).toHaveValue("Existing plan worth keeping.");
  await expect(page.getByLabel("Lead agent", { exact: true })).toHaveValue(
    "lead",
  );
  await expect(
    page.getByRole("region", { name: "Review channel message" }),
  ).toHaveCount(0);
  await goal.fill("Summarize the supplied evidence.");
  let submitted = false;
  await page.route("**/api/v1/agent-runs", (r) => {
    const body = r.request().postDataJSON();
    expect(body.goal).toBe("Summarize the supplied evidence.");
    submitted = true;
    return r.fulfill({ json: { run: { ...run, goal: body.goal } } });
  });
  await page
    .getByRole("button", { name: "Start team work", exact: true })
    .click();
  await expect.poll(() => submitted).toBe(true);
});
