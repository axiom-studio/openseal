import { test, expect } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";

test("work searches the whole workspace, paginates, and resets its page when filtered", async ({
  page,
}) => {
  const runs = Array.from({ length: 120 }, (_, i) => ({
    id: `run-${i}`,
    goal: i === 119 ? "Historic evidence report" : `Evidence task ${i + 1}`,
    revision: 1,
    status: i === 119 ? "completed" : "running",
    owner: { type: "agent", id: "analyst" },
    createdAt: "2026-09-17T10:00:00Z",
  }));
  const requests: URL[] = [];
  await page.route("**/api/v1/capabilities", (route) =>
    route.fulfill({
      json: {
        capabilities: [
          { id: "agent-runs", available: true, operations: ["list", "get"] },
        ],
      },
    }),
  );
  await page.route("**/api/v1/agent-runs?*", (route) => {
    const url = new URL(route.request().url());
    requests.push(url);
    const q = (url.searchParams.get("q") || "").toLowerCase();
    const status = url.searchParams.get("status");
    const filtered = runs.filter(
      (run) =>
        run.goal.toLowerCase().includes(q) &&
        (!status || status.split(",").includes(run.status)),
    );
    const offset = Number(url.searchParams.get("offset") || 0);
    return route.fulfill({
      json: filtered.slice(
        offset,
        offset + Number(url.searchParams.get("limit")),
      ),
    });
  });
  await page.route("**/api/v1/agent-runs/run-119?*", (route) =>
    route.fulfill({
      json: {
        ...runs[119],
        revision: 2,
        output: { reply: "Fresh result from the older task." },
      },
    }),
  );
  await page.goto("/");
  await page
    .getByRole("navigation")
    .getByRole("button", { name: /Work/ })
    .click();
  await expect(
    page.getByRole("button", { name: /^Evidence task 25 / }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Next page", exact: true }).click();
  await expect(
    page.getByText("Showing 26–50. More work is available.", { exact: true }),
  ).toBeFocused();
  await page.getByRole("searchbox", { name: "Search work" }).fill("Historic");
  await expect(
    page.getByRole("button", { name: /Historic evidence report/ }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Previous page", exact: true }),
  ).toBeDisabled();
  await expect(
    page.getByRole("button", { name: "Next page", exact: true }),
  ).toBeDisabled();
  const search = requests.findLast(
    (url) => url.searchParams.get("q") === "Historic",
  )!;
  expect(search.searchParams.get("scopeId")).toBe("default");
  expect(search.searchParams.get("order")).toBe("created_desc");
  expect(search.searchParams.get("offset")).toBe("0");
  await page
    .getByRole("combobox", { name: "Work status" })
    .selectOption("failed");
  await expect(
    page.getByRole("heading", { name: "No work matches this view" }),
  ).toBeVisible();
  await page
    .getByRole("combobox", { name: "Work status" })
    .selectOption("completed");
  await expect(
    page.getByRole("button", { name: /Historic evidence report/ }),
  ).toBeVisible();
  await page.screenshot({
    path: "../.impeccable/review/work-browser-desktop.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({
    path: "../.impeccable/review/work-browser-mobile.png",
    fullPage: true,
  });
  expect(
    (
      await new AxeBuilder({ page })
        .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
        .analyze()
    ).violations,
  ).toEqual([]);
  await page.getByRole("button", { name: /Historic evidence report/ }).click();
  await page
    .getByRole("button", { name: "Refresh workspace", exact: true })
    .click();
  await expect(
    page.getByText("Fresh result from the older task.", { exact: true }),
  ).toBeVisible();
});

test("work ignores late searches and can recover from a failed page", async ({
  page,
}) => {
  let release!: () => void;
  let began!: () => void;
  const started = new Promise<void>((resolve) => {
    began = resolve;
  });
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  let fail = true;
  await page.route("**/api/v1/capabilities", (route) =>
    route.fulfill({
      json: {
        capabilities: [
          { id: "agent-runs", available: true, operations: ["list"] },
        ],
      },
    }),
  );
  await page.route("**/api/v1/agent-runs?*", async (route) => {
    const q = new URL(route.request().url()).searchParams.get("q");
    if (q === "old") {
      began();
      await held;
      return route.fulfill({
        json: [
          {
            id: "old",
            goal: "Outdated result",
            status: "completed",
            revision: 1,
          },
        ],
      });
    }
    if (q === "new" && fail)
      return route.fulfill({
        status: 503,
        json: { error: "Temporary outage" },
      });
    return route.fulfill({ json: [] });
  });
  await page.goto("/");
  await page
    .getByRole("navigation")
    .getByRole("button", { name: /Work/ })
    .click();
  await page.getByRole("searchbox", { name: "Search work" }).fill("old");
  await started;
  await page.getByRole("searchbox", { name: "Search work" }).fill("new");
  await expect(page.getByRole("alert")).toContainText("Could not load work");
  release();
  fail = false;
  await page.getByRole("button", { name: "Try again", exact: true }).click();
  await expect(page.getByText("No results.", { exact: true })).toBeFocused();
  await expect(page.getByText("Outdated result", { exact: true })).toHaveCount(
    0,
  );
});
