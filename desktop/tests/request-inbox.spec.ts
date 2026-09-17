import { test, expect, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
const run = {
  id: "older-lead",
  goal: "Prepare the quarterly evidence review",
  status: "paused",
  revision: 2,
  owner: { type: "team", id: "research" },
};
const request = {
  id: "older-question",
  sourceRunId: run.id,
  goal: "Confirm the reporting period",
  kind: "request",
  status: "clarification_requested",
  revision: 2,
  requester: { type: "team", id: "research" },
  recipient: { type: "agent", id: "analyst" },
  clarification: "Should I include July or the entire quarter?",
};
async function setup(page: Page, inspect = true) {
  await page.route("**/api/v1/capabilities", (r) =>
    r.fulfill({
      json: {
        capabilities: [
          { id: "team-definitions", available: true, operations: ["list"] },
          { id: "agent-definitions", available: true, operations: ["list"] },
          {
            id: "agent-runs",
            available: true,
            operations: [
              "list",
              ...(inspect ? ["get", "intervene", "create"] : []),
            ],
          },
          {
            id: "agent-requests",
            available: true,
            operations: ["list", ...(inspect ? ["get"] : [])],
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/agent-runs?*", (r) => r.fulfill({ json: [] }));
  await page.route("**/api/v1/agent-runs/older-lead?*", (r) =>
    r.fulfill({ json: run }),
  );
  await page.route("**/api/v1/agent-requests/older-question?*", (r) =>
    r.fulfill({ json: request }),
  );
  await page.route("**/api/v1/team-deployments?*", (r) =>
    r.fulfill({
      json: {
        items: [
          {
            deployment: { id: "research" },
            definition: { displayName: "Research team" },
          },
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
              id: "analyst",
              rolloutStatus: "active",
              activeVersion: "1",
            },
            definition: { displayName: "Analyst" },
          },
        ],
      },
    }),
  );
  await page.goto("/");
  await page.keyboard.press("Control+3");
  await page.getByRole("button", { name: "Requests", exact: true }).click();
}
test("workspace questions open their exact older request and preserve list context", async ({
  page,
}) => {
  await page.route("**/api/v1/agent-requests?*", (r) => {
    const params = new URL(r.request().url()).searchParams;
    expect(params.get("scopeKind")).toBe("local");
    expect(params.get("scopeId")).toBe("default");
    if (params.has("sourceRunId")) return r.fulfill({ json: [] }); // selected request is beyond the first run page
    expect(params.get("status")).toBe("clarification_requested");
    return r.fulfill({
      json: [
        request,
        {
          ...request,
          id: "second",
          goal: "Check sources for the launch brief",
          recipient: { type: "agent", id: "editor" },
          clarification: "Which source should take precedence?",
        },
      ],
    });
  });
  await setup(page);
  const row = page.getByRole("button", {
    name: /Confirm the reporting period/,
  });
  await expect(row).toContainText("Research team");
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: "../.impeccable/review/request-inbox-desktop.png",
  });
  await page.setViewportSize({ width: 390, height: 844 });
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await page.screenshot({
    path: "../.impeccable/review/request-inbox-mobile.png",
  });
  await page.setViewportSize({ width: 1440, height: 1000 });
  await row.click();
  await expect(
    page
      .locator(".inspector")
      .getByText(request.clarification, { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Show all requests for this task" }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Guide the lead", exact: true })
    .click();
  await expect(
    page.getByLabel("Guidance for this task", { exact: true }),
  ).toBeFocused();
  await page
    .getByRole("button", { name: "Close details", exact: true })
    .click();
  await expect(row).toBeFocused();
  await expect(page.getByLabel("Request status")).toHaveValue(
    "clarification_requested",
  );
});
test("pagination and status filters use the server; failed refresh retains the last page", async ({
  page,
}) => {
  let offline = false;
  await page.route("**/api/v1/agent-requests?*", (r) => {
    if (offline) return r.fulfill({ status: 503, json: { error: "offline" } });
    const p = new URL(r.request().url()).searchParams;
    expect(p.get("limit")).toBe("21");
    return r.fulfill({
      json:
        p.get("status") === "completed"
          ? []
          : p.get("offset") === "20"
            ? [request]
            : Array.from({ length: 21 }, (_, i) => ({
                ...request,
                id: `r${i}`,
                goal: `Evidence request ${i}`,
              })),
    });
  });
  await setup(page);
  await page.getByRole("button", { name: "Next page", exact: true }).click();
  await expect(
    page.getByRole("button", { name: /Confirm the reporting period/ }),
  ).toBeVisible();
  offline = true;
  await page
    .getByRole("button", { name: "Refresh requests", exact: true })
    .click();
  await expect(
    page.getByRole("alert").filter({ hasText: "Previously loaded" }),
  ).toBeFocused();
  await expect(
    page.getByRole("button", { name: /Confirm the reporting period/ }),
  ).toBeVisible();
  offline = false;
  await page.getByLabel("Request status").selectOption("completed");
  await expect(
    page.getByRole("heading", { name: "No requests in this view" }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Previous page", exact: true }),
  ).toBeDisabled();
});
test("read-only capability stays honest and delayed opening cannot interrupt navigation", async ({
  page,
}) => {
  await page.route("**/api/v1/agent-requests?*", (r) =>
    r.fulfill({ json: [request] }),
  );
  await setup(page, false);
  await expect(
    page.getByRole("button", { name: /Confirm the reporting period/ }),
  ).toBeDisabled();
  await setup(page, true);
  let release!: () => void;
  const gate = new Promise<void>((resolve) => (release = resolve));
  await page.route("**/api/v1/agent-runs/older-lead?*", async (r) => {
    await gate;
    await r.fulfill({ json: run });
  });
  await page
    .getByRole("button", { name: /Confirm the reporting period/ })
    .click();
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  release();
  await expect(
    page.getByRole("heading", { name: "Settings", exact: true }),
  ).toBeVisible();
  await expect(page.locator(".inspector")).toHaveCount(0);
});

test("task entry points select Tasks after visiting Requests", async ({
  page,
}) => {
  await page.route("**/api/v1/agent-requests?*", (r) =>
    r.fulfill({ json: [] }),
  );
  await setup(page);
  await page.keyboard.press("Control+1");
  await page
    .locator("section")
    .filter({
      has: page.getByRole("heading", { name: "Recent work", exact: true }),
    })
    .getByRole("button", { name: "View all", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "Tasks", exact: true }),
  ).toHaveAttribute("aria-pressed", "true");
  await expect(page.getByLabel("Search work")).toBeVisible();
  await page.getByRole("button", { name: "Requests", exact: true }).click();
  await page.keyboard.press("Control+1");
  let createdRun: any;
  await page.route("**/api/v1/agent-runs", (r) => {
    createdRun = { ...run, ...r.request().postDataJSON(), id: "new-task" };
    return r.fulfill({ json: { run: createdRun } });
  });
  await page.route("**/api/v1/agent-runs/new-task?*", (r) =>
    r.fulfill({ json: createdRun }),
  );
  await page
    .getByRole("button", { name: "Start work", exact: true })
    .first()
    .click();
  await page.getByLabel("Describe the work").fill(run.goal);
  await page.getByLabel("Assign to").selectOption("analyst");
  await page.locator('form button[type="submit"]').click();
  await expect(
    page.getByRole("button", { name: "Tasks", exact: true }),
  ).toHaveAttribute("aria-pressed", "true");
  await expect(
    page.locator(".inspector").getByRole("heading", { name: run.goal }),
  ).toBeVisible();
});
