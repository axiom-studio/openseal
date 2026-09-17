import { test, expect, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import type { Team } from "../src/TeamBrowser";
const team: Team = {
  deployment: {
    id: "evidence-team",
    activeVersion: "1",
    status: "active",
    revision: 1,
    roster: [
      {
        id: "member-1",
        roleId: "research",
        agentDeploymentId: "researcher",
        displayName: "Evidence lead",
      },
      { id: "member-2", roleId: "review", agentDeploymentId: "missing-agent" },
    ],
  },
  definition: {
    displayName: "Evidence team",
    purpose: "Investigate claims and explain what the evidence supports.",
    roles: [
      {
        id: "research",
        displayName: "Research",
        purpose: "Find and compare sources.",
        minimumMembers: 1,
        maximumMembers: 2,
      },
      {
        id: "review",
        displayName: "Review",
        purpose: "Challenge conclusions and identify uncertainty.",
      },
      {
        id: "editor",
        displayName: "Editing",
        purpose: "Make the final report clear.",
      },
    ],
    operatingPrinciples: ["Keep uncertainty explicit."],
  },
};
async function setup(page: Page, available = true) {
  await page.route("**/api/v1/capabilities", (r) =>
    r.fulfill({
      json: {
        capabilities: [
          { id: "team-definitions", available, operations: ["list"] },
          { id: "agent-definitions", available: true, operations: ["list"] },
        ],
      },
    }),
  );
  await page.route("**/api/v1/agent-deployments?*", (r) =>
    r.fulfill({
      json: {
        items: [
          {
            deployment: {
              id: "researcher",
              activeVersion: "1",
              rolloutStatus: "inactive",
              revision: 1,
            },
            definition: {
              displayName: "Researcher",
              purpose: "Find evidence",
              systemPrompt: "Research carefully.",
            },
          },
        ],
      },
    }),
  );
  await page.goto("/");
  await expect(
    page.getByText("Connected locally", { exact: true }),
  ).toBeVisible();
  await page.keyboard.press("Control+5");
  await expect(
    page.getByRole("heading", { name: "Teams", exact: true }),
  ).toBeVisible();
}
test("teams expose roles and member availability, search, keyboard details and narrow layout", async ({
  page,
}) => {
  await page.route("**/api/v1/team-deployments?*", (r) => {
    const url = new URL(r.request().url());
    expect(url.searchParams.get("scopeKind")).toBe("local");
    expect(url.searchParams.get("scopeId")).toBe("default");
    return r.fulfill({ json: { items: [team] } });
  });
  await setup(page);
  const row = page.getByRole("button", { name: /Evidence team.*active/ });
  await row.focus();
  await page.keyboard.press("Enter");
  await expect(row).toHaveAttribute("aria-expanded", "true");
  const details = page.getByRole("region", { name: "Evidence team details" });
  await expect(details.getByText("inactive", { exact: true })).toBeVisible();
  await expect(details.getByText("Agent details unavailable")).toBeVisible();
  await expect(details.getByText("No member assigned.")).toBeVisible();
  await expect(details.getByText("Keep uncertainty explicit.")).toBeVisible();
  await page.screenshot({
    path: "../.impeccable/review/teams-desktop.png",
    fullPage: true,
  });
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await details.getByRole("button", { name: "View Evidence lead" }).click();
  await expect(
    page.getByRole("complementary", { name: "Details" }),
  ).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(
    page.getByRole("button", { name: "View Evidence lead" }),
  ).toBeFocused();
  await page.getByRole("searchbox", { name: "Search teams" }).fill("Editing");
  await expect(row).toBeVisible();
  await page
    .getByRole("combobox", { name: "Team status" })
    .selectOption("paused");
  await expect(
    page.getByRole("heading", { name: "No matching teams" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Clear filters" }).click();
  await row.click();
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({
    path: "../.impeccable/review/teams-mobile.png",
    fullPage: true,
  });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});
test("team refresh preserves inspected context on failure and reconciles changed membership", async ({
  page,
}) => {
  let failed = false;
  let current = structuredClone(team);
  await page.route("**/api/v1/team-deployments?*", (r) =>
    failed
      ? r.fulfill({ status: 503, json: { error: "Storage unavailable" } })
      : r.fulfill({ json: { items: [current] } }),
  );
  await setup(page);
  await page.getByRole("button", { name: /Evidence team.*active/ }).click();
  failed = true;
  await page.getByRole("button", { name: "Refresh teams" }).click();
  await expect(page.getByRole("alert")).toContainText(
    "Showing the last loaded team details.",
  );
  await expect(
    page.getByRole("region", { name: "Evidence team details" }),
  ).toBeVisible();
  await expect(
    page
      .getByRole("status")
      .filter({ hasText: "Team details may be out of date." }),
  ).toBeFocused();
  failed = false;
  current.deployment.roster = [];
  current.deployment.revision = 2;
  await page.getByRole("button", { name: "Refresh teams" }).click();
  await expect(
    page.getByText("Version 1 · 0 members · Revision 2"),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "View Evidence lead" }),
  ).toHaveCount(0);
});
test("team capability prevents unavailable requests and empty workspace is honest", async ({
  page,
}) => {
  let calls = 0;
  await page.route("**/api/v1/team-deployments?*", (r) => {
    calls++;
    return r.fulfill({ json: { items: [] } });
  });
  await setup(page, false);
  await expect(
    page.getByText("Team browsing is unavailable in this workspace."),
  ).toBeVisible();
  expect(calls).toBe(0);
  await setup(page);
  await expect(
    page.getByRole("heading", { name: "Your teams will appear here" }),
  ).toBeVisible();
  await expect(
    page.getByText(/Review its roles and permissions before installing it/),
  ).toBeVisible();
});
test("initial team load failure has explicit retry and unavailable data cannot look empty", async ({
  page,
}) => {
  let malformed = true;
  await page.route("**/api/v1/team-deployments?*", (r) =>
    r.fulfill({ json: malformed ? {} : { items: [team] } }),
  );
  await setup(page);
  await expect(page.getByRole("alert")).toContainText("unreadable team list");
  await expect(
    page.getByRole("heading", { name: "Your teams will appear here" }),
  ).toHaveCount(0);
  malformed = false;
  await page.getByRole("button", { name: "Refresh teams" }).click();
  await expect(
    page.getByRole("button", { name: /Evidence team.*active/ }),
  ).toBeVisible();
});

test("loading teams has valid accessible semantics before a slow response arrives", async ({
  page,
}) => {
  let release!: () => void;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route("**/api/v1/team-deployments?*", async (route) => {
    await held;
    await route.fulfill({ json: { items: [team] } });
  });
  await setup(page);
  const loading = page.getByRole("group", { name: "Loading teams" });
  await expect(loading).toHaveAttribute("aria-busy", "true");
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: "../.impeccable/review/teams-loading.png",
    fullPage: true,
  });
  release();
  await expect(
    page.getByRole("button", { name: /Evidence team.*active/ }),
  ).toBeVisible();
  await expect(loading).toHaveCount(0);
});
