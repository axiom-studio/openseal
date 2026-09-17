import { test, expect } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";

const capability = {
  capabilities: [
    { id: "agent-runs", available: true, operations: ["list", "get"] },
    { id: "activity", available: true, operations: ["list"] },
  ],
};
const event = (id: string, summary: string) => ({
  id,
  eventType: "run.paused",
  summary,
  severity: "info",
  createdAt: "2026-09-17T12:00:00Z",
});

test("history is run-scoped, paginated, and preserves older entries until explicitly refreshed", async ({
  page,
}) => {
  let run = {
    id: "history-run",
    goal: "Analyze evidence",
    status: "paused",
    revision: 2,
  };
  let firstPageCalls = 0;
  await page.route("**/api/v1/capabilities", (route) =>
    route.fulfill({ json: capability }),
  );
  await page.route("**/api/v1/agent-runs?*", (route) =>
    route.fulfill({ json: [run] }),
  );
  await page.route("**/api/v1/activity?*", (route) => {
    const query = new URL(route.request().url()).searchParams;
    expect(query.get("runId")).toBe(run.id);
    expect(query.get("scopeKind")).toBe("local");
    expect(query.get("scopeId")).toBe("default");
    expect(query.has("includeDetails")).toBe(false);
    if (query.has("cursor")) {
      expect(query.get("cursor")).toBe("older-page");
      return route.fulfill({
        json: {
          items: [event("recent", "Work paused"), event("old", "Work started")],
          hasMore: false,
        },
      });
    }
    firstPageCalls++;
    return route.fulfill({
      json: {
        items: [
          event("recent", run.revision === 2 ? "Work paused" : "New activity"),
        ],
        hasMore: true,
        nextCursor: "older-page",
      },
    });
  });
  await page.goto("/");
  await page.getByRole("button", { name: /Analyze evidence/ }).click();
  expect(firstPageCalls).toBe(0);
  await page.getByText("Activity history", { exact: true }).click();
  await expect(
    page
      .getByRole("list", { name: "Recorded activity" })
      .getByText("Work paused", { exact: true }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Load earlier activity", exact: true })
    .click();
  await expect(
    page.getByText("Earlier activity loaded.", { exact: true }),
  ).toBeFocused();
  await expect(
    page.getByRole("list", { name: "Recorded activity" }).getByRole("listitem"),
  ).toHaveCount(2);
  run = { ...run, revision: 3 };
  await page
    .getByRole("button", { name: "Refresh workspace", exact: true })
    .click();
  await expect(
    page.getByText("The run changed. Refresh to see its latest activity.", {
      exact: true,
    }),
  ).toBeVisible();
  expect(firstPageCalls).toBe(1);
  await expect(page.getByText("Work started", { exact: true })).toBeVisible();
  await page
    .getByRole("button", { name: "Refresh history", exact: true })
    .click();
  await expect(
    page.getByText("History refreshed.", { exact: true }),
  ).toBeFocused();
  await expect(
    page
      .getByRole("list", { name: "Recorded activity" })
      .getByText("New activity", { exact: true }),
  ).toBeVisible();
  await expect(page.getByText("Work started", { exact: true })).toHaveCount(0);
  await page.evaluate(() => {
    (document.activeElement as HTMLElement)?.blur();
    window.scrollTo(0, 0);
  });
  await expect(
    page.getByRole("region", { name: "Work result", exact: true }),
  ).toHaveCount(1);
  await page.locator(".work-history").scrollIntoViewIfNeeded();
  await page.screenshot({
    path: "../.impeccable/review/work-history-desktop.png",
    fullPage: false,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.locator(".work-history").scrollIntoViewIfNeeded();
  await expect(
    page.getByRole("list", { name: "Recorded activity" }),
  ).toBeInViewport();
  await page.screenshot({
    path: "../.impeccable/review/work-history-mobile.png",
    fullPage: false,
  });
  expect(
    (
      await new AxeBuilder({ page })
        .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
        .analyze()
    ).violations,
  ).toEqual([]);
});

test("history failures are recoverable and old responses cannot appear under another run", async ({
  page,
}) => {
  const runs = [
    { id: "first", goal: "First history", status: "completed", revision: 1 },
    { id: "second", goal: "Second history", status: "completed", revision: 1 },
  ];
  let release!: () => void;
  const gate = new Promise<void>((resolve) => (release = resolve));
  let secondAttempts = 0;
  await page.route("**/api/v1/capabilities", (route) =>
    route.fulfill({ json: capability }),
  );
  await page.route("**/api/v1/agent-runs?*", (route) =>
    route.fulfill({ json: runs }),
  );
  await page.route("**/api/v1/activity?*", async (route) => {
    if (new URL(route.request().url()).searchParams.get("runId") === "first") {
      await gate;
      await route.fulfill({
        json: {
          items: [event("private-first", "First run event")],
          hasMore: false,
        },
      });
      return;
    }
    secondAttempts++;
    if (secondAttempts === 1) {
      await route.fulfill({
        status: 503,
        json: { error: "History temporarily unavailable" },
      });
      return;
    }
    await route.fulfill({ json: { items: [], hasMore: false } });
  });
  await page.goto("/");
  await page.getByRole("button", { name: /First history/ }).click();
  const pending = page.waitForRequest("**/api/v1/activity?*");
  await page.getByText("Activity history", { exact: true }).click();
  await pending;
  await page
    .getByRole("button", { name: "Close details", exact: true })
    .click();
  await page.getByRole("button", { name: /Second history/ }).click();
  await page.getByText("Activity history", { exact: true }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "Could not load activity" }),
  ).toBeVisible();
  const response = page.waitForResponse((r) => r.url().includes("runId=first"));
  release();
  await (await response).finished();
  await page.evaluate(
    () =>
      new Promise((resolve) =>
        requestAnimationFrame(() => requestAnimationFrame(resolve)),
      ),
  );
  await expect(page.getByText("First run event", { exact: true })).toHaveCount(
    0,
  );
  await page
    .getByRole("button", { name: "Refresh history", exact: true })
    .click();
  await expect(
    page.getByText("No activity has been recorded for this run yet.", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(
    page.getByText("History refreshed.", { exact: true }),
  ).toBeFocused();
});
