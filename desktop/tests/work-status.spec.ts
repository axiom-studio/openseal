import { test, expect } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";

test("work status explains provider retry and budget limits without treating reserved capacity as spent", async ({
  page,
}) => {
  const runs = [
    {
      id: "retrying",
      kind: "agent_work",
      goal: "Review provider evidence",
      status: "sleeping",
      revision: 2,
      wakeCondition: {
        type: "timer",
        reference: "hosted-turn-retry",
        wakeAt: "2026-09-17T12:00:00Z",
      },
    },
    {
      id: "limited",
      kind: "agent_work",
      goal: "Summarize a long report",
      status: "paused",
      revision: 4,
      budgetState: "exhausted",
      budgetAdmission: {
        dimension: "total_tokens",
        required: 1600,
        remaining: 800,
      },
    },
  ];
  await page.route("**/api/v1/capabilities", (route) =>
    route.fulfill({
      json: {
        capabilities: [
          {
            id: "agent-runs",
            available: true,
            operations: ["list", "get", "pause", "resume", "cancel"],
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/agent-runs?*", (route) =>
    route.fulfill({ json: runs }),
  );
  await page.goto("/");
  await page.getByRole("button", { name: /Review provider evidence/ }).click();
  const inspector = page.getByRole("complementary", { name: "Work details" });
  await expect(
    inspector.getByText(/The model provider was unavailable/),
  ).toBeVisible();
  await expect(inspector.locator("time")).toHaveAttribute(
    "datetime",
    "2026-09-17T12:00:00.000Z",
  );
  await inspector
    .getByRole("button", { name: "Open provider settings", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "Model provider", exact: true }),
  ).toBeVisible();
  await page
    .getByRole("navigation")
    .getByRole("button", { name: /Work/ })
    .click();
  await page.getByRole("button", { name: /Summarize a long report/ }).click();
  await expect(inspector.getByText(/At the last budget check/)).toBeVisible();
  await expect(
    inspector.getByText("The next step needs 1,600 total tokens; 800 remain.", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(
    inspector.getByRole("button", { name: "Resume", exact: true }),
  ).toBeEnabled();
  await expect(
    inspector.getByRole("button", { name: "Resume", exact: true }),
  ).toHaveAccessibleDescription(/Work is paused/);
  await page.evaluate(() => {
    (document.activeElement as HTMLElement)?.blur();
    window.scrollTo(0, 0);
  });
  await page.screenshot({
    path: "../.impeccable/review/work-status-desktop.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({
    path: "../.impeccable/review/work-status-mobile.png",
    fullPage: true,
  });
  expect(
    (
      await new AxeBuilder({ page })
        .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
        .analyze()
    ).violations,
  ).toEqual([]);
});

test("a new attempt is a focused draft and preserves the original failed run", async ({
  page,
}) => {
  const run = {
    id: "failed-work",
    kind: "agent_work",
    goal: "Compare the supplied notes",
    status: "failed",
    revision: 3,
    assignedAgentId: "analyst",
    owner: { type: "agent", id: "analyst" },
    error: "Task provider returned HTTP 401.",
  };
  let created = 0;
  await page.route("**/api/v1/capabilities", (route) =>
    route.fulfill({
      json: {
        capabilities: [
          {
            id: "agent-runs",
            available: true,
            operations: ["list", "get", "create"],
          },
          { id: "agent-definitions", available: true, operations: ["list"] },
        ],
      },
    }),
  );
  await page.route("**/api/v1/agent-runs?*", (route) =>
    route.fulfill({ json: [run] }),
  );
  await page.route("**/api/v1/agent-deployments?*", (route) =>
    route.fulfill({
      json: {
        items: [
          {
            deployment: {
              id: "analyst",
              activeVersion: "1",
              rolloutStatus: "active",
            },
            definition: { displayName: "Analyst" },
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/agent-runs", (route) => {
    created++;
    return route.fulfill({ status: 500, json: { error: "should not run" } });
  });
  await page.addInitScript(() =>
    localStorage.setItem("openseal.prompt", "Keep my unfinished draft"),
  );
  await page.goto("/");
  await page
    .getByRole("button", { name: /Compare the supplied notes/ })
    .click();
  await page
    .getByRole("button", { name: "Review a new attempt", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "Replace draft and review", exact: true }),
  ).toBeFocused();
  await expect(
    page.getByRole("button", { name: "Replace draft and review", exact: true }),
  ).toHaveAccessibleDescription(/Replace your unfinished draft/);
  await page.evaluate(() => {
    (document.activeElement as HTMLElement)?.blur();
    window.scrollTo(0, 0);
  });
  await page.screenshot({
    path: "../.impeccable/review/work-retry-draft-desktop.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({
    path: "../.impeccable/review/work-retry-draft-mobile.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.getByRole("button", { name: "Keep draft", exact: true }).click();
  expect(
    await page.evaluate(() => localStorage.getItem("openseal.prompt")),
  ).toBe("Keep my unfinished draft");
  expect(created).toBe(0);
  await page
    .getByRole("button", { name: "Review a new attempt", exact: true })
    .click();
  await page
    .getByRole("button", { name: "Replace draft and review", exact: true })
    .click();
  const composer = page.getByRole("textbox", { name: "Describe the work" });
  await expect(composer).toHaveValue(run.goal);
  await expect(composer).toBeFocused();
  await expect(page.getByRole("combobox", { name: "Assign to" })).toHaveValue(
    "analyst",
  );
  expect(created).toBe(0);
  await expect(
    page
      .getByRole("status")
      .filter({ hasText: "Review it before starting a new run." }),
  ).toBeVisible();
  await page
    .getByRole("navigation")
    .getByRole("button", { name: /Work/ })
    .click();
  await page
    .getByRole("button", { name: /Compare the supplied notes/ })
    .click();
  await expect(
    page
      .getByRole("complementary", { name: "Work details" })
      .getByText("Failed", { exact: true }),
  ).toBeVisible();
  expect(created).toBe(0);
});
