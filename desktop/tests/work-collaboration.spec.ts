import { test, expect, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
const run = {
  id: "lead-run",
  goal: "Coordinate source review",
  owner: { type: "team", id: "team" },
  status: "paused",
  revision: 2,
  pendingInterventions: [] as any[],
};
const request = {
  id: "request-1",
  sourceRunId: run.id,
  kind: "request",
  status: "clarification_requested",
  revision: 2,
  goal: "Check evidence independently",
  requester: { type: "team", id: "team" },
  recipient: { type: "agent", id: "reviewer" },
  clarification: "Which reporting period should I use?",
};
async function setup(page: Page, get = () => run, canGuide = true) {
  await page.route("**/api/v1/capabilities", (r) =>
    r.fulfill({
      json: {
        capabilities: [
          {
            id: "agent-runs",
            available: true,
            operations: ["list", "get", ...(canGuide ? ["intervene"] : [])],
          },
          {
            id: "agent-requests",
            available: true,
            operations: ["list", "get"],
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/agent-runs?*", (r) =>
    r.fulfill({ json: [get()] }),
  );
  await page.route("**/api/v1/agent-runs/lead-run?*", (r) =>
    r.fulfill({ json: get() }),
  );
  await page.goto("/");
  await page.getByRole("button", { name: /Coordinate source review/ }).click();
  await page
    .getByRole("button", { name: "Delegation and requests", exact: true })
    .click();
}
test("clarification is visible and guidance uses the lead run without impersonating a recipient", async ({
  page,
}) => {
  let current = { ...run };
  let writes = 0;
  await page.route("**/api/v1/agent-requests?*", (r) => {
    expect(new URL(r.request().url()).searchParams.get("sourceRunId")).toBe(
      run.id,
    );
    return r.fulfill({ json: [request] });
  });
  await page.route("**/api/v1/agent-requests/*/responses?*", () => {
    throw new Error("Desktop must not impersonate an agent");
  });
  await page.route("**/api/v1/agent-runs/lead-run/commands?*", (r) => {
    const body = r.request().postDataJSON();
    writes++;
    expect(body.kind).toBe("intervene");
    expect(body.actor).toEqual({ type: "user", id: "local-operator" });
    current = {
      ...current,
      revision: 3,
      pendingInterventions: [
        {
          id: body.interventionId,
          instruction: body.instruction,
          actor: body.actor,
          createdAt: "2026-09-17T10:00:00Z",
        },
      ],
    };
    return r.fulfill({ json: { run: current } });
  });
  await setup(page, () => current);
  await expect(
    page.getByText(request.clarification, { exact: true }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Guide the lead", exact: true })
    .click();
  const input = page.getByLabel("Guidance for this task", { exact: true });
  await expect(input).toBeFocused();
  await expect(input).toContainText(request.clarification);
  await input.fill(
    "For request-1, use the July reporting period and state that scope in the clarification.",
  );
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.locator(".inspector").evaluate((el) => (el.scrollTop = 0));
  await page.screenshot({
    path: "../.impeccable/review/work-collaboration-desktop.png",
  });
  await page.locator(".work-guidance").scrollIntoViewIfNeeded();
  await page.screenshot({
    path: "../.impeccable/review/work-collaboration-guidance-desktop.png",
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.locator(".inspector").evaluate((el) => (el.scrollTop = 0));
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: "../.impeccable/review/work-collaboration-mobile.png",
  });
  await page.locator(".work-guidance").scrollIntoViewIfNeeded();
  await page.screenshot({
    path: "../.impeccable/review/work-collaboration-guidance-mobile.png",
  });
  await page
    .getByRole("button", { name: "Save guidance", exact: true })
    .click();
  await expect(page.getByText(/Guidance saved in this task/)).toBeFocused();
  expect(writes).toBe(1);
  expect(current.status).toBe("paused");
});
test("opening clarification preserves an existing guidance draft and readonly requests remain inspectable", async ({
  page,
}) => {
  await page.addInitScript(() =>
    localStorage.setItem(
      "openseal.guidance.lead-run",
      JSON.stringify({ draft: "Preserve my existing note", pending: null }),
    ),
  );
  await page.route("**/api/v1/agent-requests?*", (r) =>
    r.fulfill({ json: [request] }),
  );
  await setup(page);
  await page
    .getByRole("button", { name: "Guide the lead", exact: true })
    .click();
  await expect(
    page.getByLabel("Guidance for this task", { exact: true }),
  ).toHaveValue("Preserve my existing note");
  await expect(
    page.getByText(/Your existing guidance draft is kept/),
  ).toBeVisible();
  await setup(page, () => run, false);
  await expect(
    page.getByRole("button", { name: "Guide the lead", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByText(request.clarification, { exact: true }),
  ).toBeVisible();
});
test("request pagination retains loaded details on failure and late related work cannot replace another screen", async ({
  page,
}) => {
  await page.route("**/api/v1/agent-requests?*", (r) =>
    r.fulfill({
      json:
        new URL(r.request().url()).searchParams.get("offset") === "20"
          ? [
              {
                ...request,
                id: "older",
                goal: "Older request",
                childRunId: "child",
              },
            ]
          : Array.from({ length: 21 }, (_, i) => ({
              ...request,
              id: `request-${i}`,
              goal: `Request ${i}`,
            })),
    }),
  );
  await setup(page);
  await page
    .getByRole("button", { name: "More requests", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "Older request", exact: true }),
  ).toBeVisible();
  await page.route("**/api/v1/agent-requests?*", (r) =>
    r.fulfill({ status: 503, json: { error: "offline" } }),
  );
  await page
    .getByRole("button", { name: "Refresh requests", exact: true })
    .click();
  await expect(
    page.getByRole("alert").filter({ hasText: "Could not refresh requests" }),
  ).toBeFocused();
  await expect(
    page.getByRole("heading", { name: "Older request", exact: true }),
  ).toBeVisible();
  let release!: () => void;
  const gate = new Promise<void>((resolve) => (release = resolve));
  await page.route("**/api/v1/agent-runs/child?*", async (r) => {
    await gate;
    await r.fulfill({
      json: { ...run, id: "child", goal: "Late delegated work" },
    });
  });
  await page
    .getByRole("button", { name: "View delegated work", exact: true })
    .click();
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  release();
  await expect(
    page.getByRole("heading", { name: "Settings", exact: true }),
  ).toBeVisible();
  await expect(page.locator(".inspector")).toHaveCount(0);
});
