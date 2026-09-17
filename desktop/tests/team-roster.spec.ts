import { test, expect, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
const initial = {
  deployment: {
    id: "roster-team",
    definitionId: "team-def",
    scope: { kind: "local", id: "default" },
    activeVersion: "1",
    status: "paused",
    revision: 1,
    restrictions: { maximumConcurrency: 2 },
    roster: [
      {
        id: "a",
        roleId: "research",
        agentDeploymentId: "alpha",
        displayName: "Research lead",
      },
      {
        id: "b",
        roleId: "review",
        agentDeploymentId: "reviewer",
        displayName: "Saved reviewer",
      },
    ],
  },
  definition: {
    displayName: "Roster team",
    purpose: "Review evidence.",
    roles: [
      {
        id: "research",
        displayName: "Research",
        purpose: "Find sources.",
        minimumMembers: 1,
        maximumMembers: 2,
        requiredDefinitionIds: ["research-def"],
        requiredSkillIds: ["read"],
      },
      {
        id: "review",
        displayName: "Review",
        purpose: "Review findings.",
        minimumMembers: 1,
        maximumMembers: 1,
        requiredDefinitionIds: ["review-def"],
      },
    ],
  },
};
const agents = [
  ["alpha", "Alpha", "research-def", "active", true],
  ["beta", "Beta", "research-def", "inactive", true],
  ["reviewer", "Reviewer", "review-def", "active", false],
  ["unqualified", "Unqualified", "research-def", "active", false],
].map(([id, name, definitionId, status, read]) => ({
  deployment: {
    id,
    definitionId,
    rolloutStatus: status,
    activeVersion: "1",
    revision: 1,
  },
  definition: {
    id: definitionId,
    displayName: name,
    purpose: "Test purpose",
    systemPrompt: "Test instruction",
    skillRequirements: read ? [{ skillId: "read" }] : [],
  },
}));
async function setup(page: Page, get: () => typeof initial) {
  await page.route("**/api/v1/capabilities**", (route) =>
    route.fulfill({
      json: {
        capabilities: [
          {
            id: "team-definitions",
            available: true,
            operations: ["list", "get", "update"],
          },
          { id: "agent-definitions", available: true, operations: ["list"] },
        ],
      },
    }),
  );
  await page.route("**/api/v1/agent-deployments?*", (route) =>
    route.fulfill({ json: { items: agents } }),
  );
  await page.route("**/api/v1/team-deployments?*", (route) =>
    route.fulfill({ json: { items: [get()] } }),
  );
  await page.goto("/");
  await expect(
    page.getByText("Connected locally", { exact: true }),
  ).toBeVisible();
  await page.keyboard.press("Control+5");
  await page.getByRole("button", { name: /Roster team.*paused/ }).click();
  await page
    .getByRole("button", { name: /^(Edit roster|Continue roster draft)$/ })
    .click();
}

test("roster drafts survive reload, enforce role eligibility and require reviewed changes", async ({
  page,
}) => {
  let current = structuredClone(initial);
  let writes = 0;
  await page.route("**/api/v1/team-deployments/roster-team?*", (route) => {
    const body = route.request().postDataJSON();
    writes++;
    expect(body.expectedRevision).toBe(1);
    expect(body.reason).toBe("Balance research capacity.");
    expect(body.deployment.status).toBe("paused");
    expect(body.deployment.restrictions).toEqual(
      initial.deployment.restrictions,
    );
    current = { ...current, deployment: { ...body.deployment, revision: 2 } };
    return route.fulfill({ json: { deployment: current.deployment } });
  });
  await setup(page, () => current);
  const select = page.getByLabel("Agent for Research 1", { exact: true });
  await expect(select.locator("option")).toHaveText([
    "Alpha · active",
    "Beta · inactive",
  ]);
  await select.selectOption("beta");
  await page
    .getByLabel("Name in team for Research 1", { exact: true })
    .fill("Evidence lead");
  await page
    .getByLabel("Reason for roster change", { exact: true })
    .fill("Balance research capacity.");
  await page.getByRole("button", { name: "Close editor", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "Continue roster draft", exact: true }),
  ).toBeFocused();
  await setup(page, () => current);
  await expect(select).toHaveValue("beta");
  await expect(
    page.getByLabel("Name in team for Research 1", { exact: true }),
  ).toHaveValue("Evidence lead");
  await expect(
    page.getByRole("button", { name: "Save roster", exact: true }),
  ).toBeDisabled();
  await page
    .getByRole("button", { name: "Add member to Research", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "Add member to Research", exact: true }),
  ).toBeDisabled();
  await page.getByRole("button", { name: "Remove Alpha", exact: true }).click();
  await page
    .getByRole("checkbox", { name: "I reviewed the role assignments" })
    .check();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.evaluate(() => {
    (document.activeElement as HTMLElement)?.blur();
    window.scrollTo(0, 0);
  });
  await page.screenshot({
    path: "../.impeccable/review/team-roster-desktop.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.evaluate(() => window.scrollTo(0, 0));
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: "../.impeccable/review/team-roster-mobile.png",
    fullPage: true,
  });
  await page.getByRole("button", { name: "Save roster", exact: true }).click();
  await expect(
    page.getByRole("status").filter({ hasText: "Team roster saved." }),
  ).toBeFocused();
  expect(writes).toBe(1);
  expect(current.deployment.roster[0].agentDeploymentId).toBe("beta");
  await expect
    .poll(() =>
      page.evaluate(() => localStorage.getItem("openseal.roster.roster-team")),
    )
    .toBeNull();
});

test("a stale roster draft preserves unrelated saved changes and requires review again", async ({
  page,
}) => {
  let current = structuredClone(initial);
  let writes = 0;
  await page.route("**/api/v1/team-deployments/roster-team?*", (route) => {
    writes++;
    const body = route.request().postDataJSON();
    if (writes === 1) {
      current.deployment.revision = 2;
      current.deployment.roster[1].displayName = "Remote review lead";
      return route.fulfill({
        status: 409,
        json: { error: "Team revision changed" },
      });
    }
    expect(body.expectedRevision).toBe(2);
    expect(body.deployment.roster[0].displayName).toBe("My research lead");
    expect(body.deployment.roster[1].displayName).toBe("Remote review lead");
    current = { ...current, deployment: { ...body.deployment, revision: 3 } };
    return route.fulfill({ json: { deployment: current.deployment } });
  });
  await setup(page, () => current);
  await page
    .getByLabel("Name in team for Research 1", { exact: true })
    .fill("My research lead");
  await page
    .getByLabel("Reason for roster change", { exact: true })
    .fill("Clarify responsibilities.");
  await page
    .getByRole("checkbox", { name: "I reviewed the role assignments" })
    .check();
  await page.getByRole("button", { name: "Save roster", exact: true }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "could not be confirmed" }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Review latest team", exact: true })
    .click();
  await expect(
    page.getByLabel("Name in team for Review 1", { exact: true }),
  ).toHaveValue("Remote review lead");
  await expect(
    page.getByLabel("Name in team for Research 1", { exact: true }),
  ).toHaveValue("My research lead");
  await expect(
    page.getByRole("checkbox", { name: "I reviewed the role assignments" }),
  ).not.toBeChecked();
  expect(writes).toBe(1);
  await page
    .getByRole("checkbox", { name: "I reviewed the role assignments" })
    .check();
  await page.getByRole("button", { name: "Save roster", exact: true }).click();
  await expect.poll(() => writes).toBe(2);
});

test("conflicting assignment edits require a per-assignment choice before saving", async ({
  page,
}) => {
  let current = structuredClone(initial);
  let writes = 0;
  await page.route("**/api/v1/team-deployments/roster-team?*", (route) => {
    writes++;
    return route.fulfill({ json: { deployment: current.deployment } });
  });
  await setup(page, () => current);
  await page
    .getByLabel("Name in team for Research 1", { exact: true })
    .fill("My lead");
  await page
    .getByLabel("Reason for roster change", { exact: true })
    .fill("Clarify ownership.");
  current.deployment.revision = 2;
  current.deployment.roster[0].displayName = "Saved lead";
  await page
    .getByRole("button", { name: "Review latest team", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "Resolve changed assignments" }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Use these choices" }),
  ).toBeDisabled();
  await expect(
    page.getByRole("button", { name: "Save roster", exact: true }),
  ).toBeDisabled();
  await page
    .getByRole("combobox", { name: "Resolve assignment Alpha" })
    .selectOption("saved");
  await page.getByRole("button", { name: "Use these choices" }).click();
  await expect(
    page.getByLabel("Name in team for Research 1", { exact: true }),
  ).toHaveValue("Saved lead");
  await expect(
    page.getByRole("button", { name: "Save roster", exact: true }),
  ).toBeDisabled();
  expect(writes).toBe(0);
});

test("lost roster response reconciles saved membership without replaying the mutation", async ({
  page,
}) => {
  let current = structuredClone(initial);
  let writes = 0;
  await page.route("**/api/v1/team-deployments/roster-team?*", (route) => {
    writes++;
    current = {
      ...current,
      deployment: { ...route.request().postDataJSON().deployment, revision: 2 },
    };
    return route.abort();
  });
  await setup(page, () => current);
  await page
    .getByLabel("Agent for Research 1", { exact: true })
    .selectOption("beta");
  await page
    .getByLabel("Reason for roster change", { exact: true })
    .fill("Change the researcher.");
  await page
    .getByRole("checkbox", { name: "I reviewed the role assignments" })
    .check();
  await page.getByRole("button", { name: "Save roster", exact: true }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "could not be confirmed" }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Review latest team", exact: true })
    .click();
  await expect(
    page
      .getByRole("status")
      .filter({ hasText: "saved roster matches your draft" }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Save roster", exact: true }),
  ).toBeDisabled();
  expect(writes).toBe(1);
});
