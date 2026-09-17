import { test, expect } from "@playwright/test";

test("cancellation is deliberate and focused, with current revision authority", async ({
  page,
}) => {
  let run = {
    id: "cancel-run",
    goal: "Analyze supplied evidence",
    status: "waiting_for_approval",
    revision: 5,
  };
  const commands: unknown[] = [];
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
    route.fulfill({ json: [run] }),
  );
  await page.route(
    "**/api/v1/agent-runs/cancel-run/commands?*",
    async (route) => {
      commands.push(route.request().postDataJSON());
      run = { ...run, status: "canceled", revision: 7 };
      await route.fulfill({ json: { run } });
    },
  );
  await page.goto("/");
  await page.getByRole("button", { name: /Analyze supplied evidence/ }).click();
  await page.getByRole("button", { name: "Cancel work", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "Confirm cancellation", exact: true }),
  ).toBeFocused();
  await expect(
    page.getByRole("button", { name: "Confirm cancellation", exact: true }),
  ).toHaveAccessibleDescription(
    "End this run? Saved results remain available. External actions already started may still finish.",
  );
  expect(commands).toHaveLength(0);
  await page.getByRole("button", { name: "Go back", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "Cancel work", exact: true }),
  ).toBeFocused();
  await page.getByRole("button", { name: "Cancel work", exact: true }).click();
  run = { ...run, status: "paused", revision: 6 };
  await page
    .getByRole("button", { name: "Refresh workspace", exact: true })
    .click();
  await expect(
    page
      .getByRole("status")
      .filter({
        hasText: "This run changed. Review its latest state before canceling.",
      }),
  ).toBeFocused();
  await expect(
    page.getByRole("button", { name: "Confirm cancellation", exact: true }),
  ).toHaveCount(0);
  expect(commands).toHaveLength(0);
  await page.getByRole("button", { name: "Cancel work", exact: true }).click();
  await page
    .getByRole("button", { name: "Confirm cancellation", exact: true })
    .click();
  expect(commands).toEqual([
    expect.objectContaining({ kind: "cancel", expectedRevision: 6 }),
  ]);
  await expect(
    page
      .getByRole("status")
      .filter({ hasText: "Work canceled. Saved results remain available." }),
  ).toBeFocused();
  await expect(
    page.getByRole("button", { name: "Cancel work", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Resume", exact: true }),
  ).toHaveCount(0);
});

test("conflicting work commands load current state and never replay automatically", async ({
  page,
}) => {
  let run = {
    id: "changing",
    goal: "Review changing evidence",
    status: "running",
    revision: 1,
  };
  let commands = 0;
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
    route.fulfill({ json: [run] }),
  );
  await page.route("**/api/v1/agent-runs/changing?*", (route) =>
    route.fulfill({ json: run }),
  );
  await page.route(
    "**/api/v1/agent-runs/changing/commands?*",
    async (route) => {
      commands++;
      run = { ...run, status: "completed", revision: 3 };
      await route.fulfill({
        status: 409,
        json: { error: "revision conflict" },
      });
    },
  );
  await page.goto("/");
  await page.getByRole("button", { name: /Review changing evidence/ }).click();
  await page.getByRole("button", { name: "Pause work", exact: true }).click();
  await expect(
    page
      .getByRole("alert")
      .filter({ hasText: "This run changed. Its latest state is shown." }),
  ).toBeFocused();
  await expect(
    page
      .getByRole("complementary", { name: "Work details" })
      .getByText("Completed", { exact: true }),
  ).toBeVisible();
  expect(commands).toBe(1);
  await expect(
    page.getByRole("button", { name: "Pause work", exact: true }),
  ).toHaveCount(0);
});

test("a delayed work command cannot replace another open run", async ({
  page,
}) => {
  const first = {
    id: "first",
    goal: "First task",
    status: "running",
    revision: 1,
  };
  const second = {
    id: "second",
    goal: "Second task",
    status: "completed",
    revision: 2,
  };
  let release!: () => void;
  const gate = new Promise<void>((resolve) => (release = resolve));
  await page.route("**/api/v1/capabilities", (route) =>
    route.fulfill({
      json: {
        capabilities: [
          {
            id: "agent-runs",
            available: true,
            operations: ["list", "get", "pause"],
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/agent-runs?*", (route) =>
    route.fulfill({ json: [first, second] }),
  );
  await page.route("**/api/v1/agent-runs/first/commands?*", async (route) => {
    await gate;
    await route.fulfill({
      json: { run: { ...first, status: "paused", revision: 3 } },
    });
  });
  await page.goto("/");
  await page.getByRole("button", { name: /First task/ }).click();
  const pending = page.waitForRequest("**/api/v1/agent-runs/first/commands?*");
  await page.getByRole("button", { name: "Pause work", exact: true }).click();
  await pending;
  await page
    .getByRole("button", { name: "Close details", exact: true })
    .click();
  await page.getByRole("button", { name: /Second task/ }).click();
  const response = page.waitForResponse(
    "**/api/v1/agent-runs/first/commands?*",
  );
  release();
  await (await response).finished();
  await page.evaluate(
    () =>
      new Promise((resolve) =>
        requestAnimationFrame(() => requestAnimationFrame(resolve)),
      ),
  );
  await expect(
    page
      .getByRole("complementary", { name: "Work details" })
      .getByRole("heading", { name: "Second task", exact: true }),
  ).toBeVisible();
});
