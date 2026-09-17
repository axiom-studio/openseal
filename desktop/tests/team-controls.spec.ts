import { test, expect, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
const initial = {
  deployment: {
    id: "managed-team",
    definitionId: "managed",
    scope: { kind: "local", id: "default" },
    activeVersion: "1",
    status: "active",
    revision: 1,
    roster: [],
    restrictions: { maximumConcurrency: 2 },
  },
  definition: {
    displayName: "Managed team",
    purpose: "Review work together.",
    roles: [
      { id: "review", displayName: "Review", purpose: "Review evidence." },
    ],
  },
};
async function setup(page: Page, get: () => typeof initial, update = true) {
  await page.route("**/api/v1/capabilities**", (route) =>
    route.fulfill({
      json: {
        capabilities: [
          {
            id: "team-definitions",
            available: true,
            operations: update ? ["list", "get", "update"] : ["list"],
          },
          { id: "workforce-authoring", available: true, operations: ["get"] },
        ],
      },
    }),
  );
  await page.route("**/api/v1/team-deployments?*", (route) =>
    route.fulfill({ json: { items: [get()] } }),
  );
  await page.goto("/");
  await expect(
    page.getByText("Connected locally", { exact: true }),
  ).toBeVisible();
  await page.keyboard.press("Control+5");
  await page.getByRole("button", { name: /Managed team/ }).click();
}

test("team changes require a reason, keep selection and preserve deployment fields", async ({
  page,
}) => {
  let current = structuredClone(initial);
  const history: any[] = [];
  await page.route("**/api/v1/team-deployments/managed-team?*", (route) => {
    const body = route.request().postDataJSON();
    expect(route.request().method()).toBe("PUT");
    expect(body.expectedRevision).toBe(current.deployment.revision);
    expect(body.deployment.restrictions).toEqual({ maximumConcurrency: 2 });
    expect(body.deployment.scope).toEqual({ kind: "local", id: "default" });
    expect(body.actorId).toBe("local-operator");
    current = {
      ...current,
      deployment: { ...body.deployment, revision: body.expectedRevision + 1 },
    };
    history.push({
      id: String(current.deployment.revision),
      deploymentRevision: current.deployment.revision,
      reason: body.reason,
      actorType: "user",
      actorId: "local-operator",
      createdAt: "2026-09-17T06:00:00Z",
      toVersion: "1",
    });
    return route.fulfill({ json: { deployment: current.deployment } });
  });
  await page.route(
    "**/api/v1/team-deployments/managed-team/activations?*",
    (route) => route.fulfill({ json: history }),
  );
  await setup(page, () => current);
  await page
    .getByRole("combobox", { name: "Team status" })
    .selectOption("active");
  await page.getByRole("button", { name: /Managed team/ }).click();
  await page.getByRole("button", { name: "Pause team", exact: true }).click();
  const reason = page.getByRole("textbox", {
    name: "Reason for this team change",
  });
  await expect(reason).toBeFocused();
  await expect(
    page.getByRole("button", { name: "Confirm pause" }),
  ).toBeDisabled();
  await reason.fill("Review the team’s responsibilities.");
  await page.getByRole("button", { name: "Keep current status" }).click();
  await expect(
    page.getByRole("button", { name: "Pause team", exact: true }),
  ).toBeFocused();
  await page.getByRole("button", { name: "Pause team", exact: true }).click();
  await expect(reason).toHaveValue("Review the team’s responsibilities.");
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.evaluate(() => {
    (document.activeElement as HTMLElement)?.blur();
    window.scrollTo(0, 0);
  });
  await page.screenshot({
    path: "../.impeccable/review/team-controls-desktop.png",
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
    path: "../.impeccable/review/team-controls-mobile.png",
    fullPage: true,
  });
  await page.getByRole("button", { name: "Confirm pause" }).click();
  await expect(
    page.getByRole("status").filter({ hasText: "Team paused." }),
  ).toBeFocused();
  await expect(
    page.getByText(
      "This selected team remains visible after its status changed.",
    ),
  ).toBeVisible();
  for (const [label, action, state] of [
    ["Archive team", "archive", "archived"],
    ["Restore team", "restore", "paused"],
    ["Resume team", "resume", "active"],
  ]) {
    await page.getByRole("button", { name: label, exact: true }).click();
    await reason.fill(`Reviewed ${action}.`);
    await page
      .getByRole("button", { name: `Confirm ${action}`, exact: true })
      .click();
    await expect.poll(() => current.deployment.status).toBe(state);
  }
  await page.getByText("Team change history", { exact: true }).click();
  await expect(page.locator(".team-history li")).toHaveCount(4);
  await expect(page.locator(".team-history li").first()).toContainText(
    "Reviewed resume.",
  );
});

test("conflicting team changes preserve the reason and require a current-state check when refresh fails", async ({
  page,
}) => {
  let current = structuredClone(initial);
  let writes = 0;
  await page.route("**/api/v1/team-deployments/managed-team?*", (route) => {
    writes++;
    current.deployment.revision = 2;
    current.deployment.status = "paused";
    return route.fulfill({ status: 409, json: { error: "Revision conflict" } });
  });
  await setup(page, () => current);
  let fail = true;
  await page.route("**/api/v1/team-deployments?*", (route) =>
    fail
      ? route.fulfill({ status: 503, json: { error: "Storage unavailable" } })
      : route.fulfill({ json: { items: [current] } }),
  );
  await page.getByRole("button", { name: "Pause team", exact: true }).click();
  await page
    .getByRole("textbox", { name: "Reason for this team change" })
    .fill("Review capacity.");
  await page.getByRole("button", { name: "Confirm pause" }).click();
  await expect(page.getByRole("alert")).toContainText(
    "current state could not be loaded",
  );
  await expect(
    page.getByRole("button", { name: "Pause team", exact: true }),
  ).toBeDisabled();
  fail = false;
  await page.getByRole("button", { name: "Check saved team state" }).click();
  await page.getByRole("button", { name: "Resume team", exact: true }).click();
  await expect(
    page.getByRole("textbox", { name: "Reason for this team change" }),
  ).toHaveValue("Review capacity.");
  expect(writes).toBe(1);
});

test("lost update response checks saved status without replay and read-only teams expose no mutation", async ({
  page,
}) => {
  let current = structuredClone(initial);
  let writes = 0;
  await page.route("**/api/v1/team-deployments/managed-team?*", (route) => {
    writes++;
    current.deployment.status = "paused";
    current.deployment.revision++;
    return route.abort();
  });
  await setup(page, () => current);
  await page.getByRole("button", { name: "Pause team", exact: true }).click();
  await page
    .getByRole("textbox", { name: "Reason for this team change" })
    .fill("Review readiness.");
  await page.getByRole("button", { name: "Confirm pause" }).click();
  await expect(page.getByRole("alert")).toContainText("could not be confirmed");
  await page.getByRole("button", { name: "Check saved team state" }).click();
  await expect(
    page.getByRole("button", { name: "Resume team", exact: true }),
  ).toBeEnabled();
  expect(writes).toBe(1);
  await setup(page, () => current, false);
  await expect(
    page.getByRole("button", { name: "Resume team", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Archive team", exact: true }),
  ).toHaveCount(0);
});

test("inactive team uses its saved proposal and a late lookup cannot navigate away from another page", async ({
  page,
}) => {
  const current = {
    ...structuredClone(initial),
    deployment: {
      ...initial.deployment,
      status: "draft",
      activation: { changeSetId: "original-team" },
    },
  };
  let release!: () => void;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  let started = false;
  await page.route(
    "**/api/v1/authoring/workforce/change-sets/original-team?*",
    async (route) => {
      started = true;
      await held;
      await route.fulfill({
        json: {
          id: "original-team",
          prompt: "Original team",
          revision: 1,
          status: "applied",
        },
      });
    },
  );
  await setup(page, () => current);
  await expect(
    page.getByRole("button", { name: "Resume team", exact: true }),
  ).toHaveCount(0);
  await page.getByRole("button", { name: "Review team activation" }).click();
  await expect.poll(() => started).toBe(true);
  await page.keyboard.press("Control+2");
  release();
  await expect(
    page.getByRole("heading", { name: "Agents", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "Agent proposal", exact: true }),
  ).toHaveCount(0);
});

test("team history retries failures and exposes older saved revisions", async ({
  page,
}) => {
  let fail = true;
  await page.route(
    "**/api/v1/team-deployments/managed-team/activations?*",
    (route) =>
      fail
        ? route.fulfill({ status: 503, json: { error: "History unavailable" } })
        : route.fulfill({
            json: Array.from({ length: 25 }, (_, index) => ({
              id: `change-${index}`,
              deploymentRevision: index + 1,
              reason: `Saved change ${index + 1}.`,
              actorType: "user",
              actorId: "local-operator",
              createdAt: "2026-09-17T06:00:00Z",
              toVersion: "1",
            })),
          }),
  );
  await setup(page, () => initial, false);
  await page.getByText("Team change history", { exact: true }).click();
  await expect(page.getByRole("alert")).toContainText(
    "Could not load team history.",
  );
  await expect(page.getByText("No team changes recorded.")).toHaveCount(0);
  fail = false;
  await page.getByRole("button", { name: "Retry team history" }).click();
  await expect(page.locator(".team-history li")).toHaveCount(20);
  await expect(page.locator(".team-history li").first()).toContainText(
    "Saved change 25.",
  );
  await page.getByRole("button", { name: "Show older changes" }).click();
  await expect(page.locator(".team-history li")).toHaveCount(25);
  await expect(page.locator(".team-history li").last()).toContainText(
    "Saved change 1.",
  );
});
