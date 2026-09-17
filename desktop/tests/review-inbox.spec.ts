import { test, expect, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";

async function setup(page: Page) {
  const reviews = Array.from({ length: 28 }, (_, i) => ({
    id: `approval-${i}`,
    runId: `older-run-${i}`,
    summary: `Review proposed action ${i + 1}`,
    status: "pending",
    risk: "production",
    expiresAt: "2099-09-17T10:00:00Z",
    revision: 1,
    actionCallId: `action-${i}`,
    eligibleApprovers: [{ type: "role", id: "operator" }],
  }));
  const history = {
    ...reviews[0],
    id: "past-approval",
    actionCallId: "past-action",
    summary: "Saved publication decision",
    status: "approved",
    revision: 2,
    decisionBy: { type: "user", id: "local-operator" },
    decisionReason: "Reviewed the exact destination.",
  };
  await page.route("**/api/v1/capabilities", (route) =>
    route.fulfill({
      json: {
        capabilities: [
          { id: "agent-runs", available: true, operations: ["list", "get"] },
          {
            id: "action-approvals",
            available: true,
            operations: ["list", "get", "resolve"],
          },
          { id: "action-calls", available: true, operations: ["get"] },
        ],
      },
    }),
  );
  await page.route("**/api/v1/agent-runs?*", (route) =>
    route.fulfill({ json: [] }),
  );
  await page.route("**/api/v1/action-approvals?*", (route) => {
    const url = new URL(route.request().url());
    expect(url.searchParams.get("scopeKind")).toBe("local");
    expect(url.searchParams.get("scopeId")).toBe("default");
    const offset = Number(url.searchParams.get("offset") || 0);
    const values =
      url.searchParams.get("status") === "approved" ? [history] : reviews;
    return route.fulfill({
      json: values.slice(
        offset,
        offset + Number(url.searchParams.get("limit")),
      ),
    });
  });
  await page.route("**/api/v1/agent-runs/*?*", (route) => {
    const id = new URL(route.request().url()).pathname.split("/").at(-1);
    const review = reviews.find((item) => item.runId === id)!;
    return route.fulfill({
      json: {
        id,
        goal: `Task for ${review.summary}`,
        status: "waiting_for_approval",
        revision: 2,
        wakeCondition: { type: "approval", reference: review.id },
        owner: { type: "agent", id: "analyst" },
      },
    });
  });
  await page.route("**/api/v1/action-approvals/*?*", (route) => {
    const id = new URL(route.request().url()).pathname.split("/").at(-1);
    return route.fulfill({
      json:
        id === history.id ? history : reviews.find((item) => item.id === id),
    });
  });
  await page.route("**/api/v1/action-calls/*?*", (route) => {
    const id = new URL(route.request().url()).pathname.split("/").at(-1);
    const review =
      id === history.actionCallId
        ? history
        : reviews.find((item) => item.actionCallId === id)!;
    return route.fulfill({
      json: {
        id,
        runId: review.runId,
        approvalId: review.id,
        revision: 1,
        status:
          review.status === "approved" ? "succeeded" : "waiting_for_approval",
        skillId: "publishing",
        skillVersion: "1",
        action: "publish",
        arguments: { destination: "Synthetic destination" },
        sideEffect: "external",
      },
    });
  });
  await page.goto("/");
  await expect(
    page.getByRole("button", { name: "Reviews, pending actions", exact: true }),
  ).toBeVisible();
  return { reviews, history };
}

test("review inbox pages waiting actions and retains the selected historical checkpoint", async ({
  page,
}) => {
  await setup(page);
  await page.keyboard.press("Control+4");
  await expect(
    page.getByRole("heading", { name: "Reviews", exact: true }),
  ).toBeFocused();
  await expect(
    page.getByRole("button", { name: /^Review proposed action 25 / }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Next page", exact: true }).click();
  await expect(page.getByText("Showing 26–28.", { exact: true })).toBeFocused();
  const row = page.getByRole("button", { name: /^Review proposed action 26 / });
  await row.click();
  const panel = page.getByRole("region", { name: "Action approval" });
  await expect(
    panel.getByRole("heading", {
      name: "Review proposed action 26",
      exact: true,
    }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Close details", exact: true })
    .click();
  await expect(row).toBeFocused();
  await page
    .getByRole("combobox", { name: "Review status" })
    .selectOption("approved");
  await expect(
    page.getByRole("button", { name: "Previous page", exact: true }),
  ).toBeDisabled();
  await page
    .getByRole("button", { name: /Saved publication decision/ })
    .click();
  await expect(
    panel.getByText("Reviewer guidance: Reviewed the exact destination.", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(
    panel.getByRole("button", { name: "Approve action", exact: true }),
  ).toHaveCount(0);
  await panel
    .getByRole("button", { name: "Refresh approval", exact: true })
    .click();
  await expect(
    panel.getByText("Saved review refreshed.", { exact: true }),
  ).toBeFocused();

  await page
    .getByRole("button", { name: "Refresh workspace", exact: true })
    .click();
  await expect(
    panel.getByRole("heading", {
      name: "Saved publication decision",
      exact: true,
    }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Close details", exact: true })
    .click();
  await page
    .getByRole("combobox", { name: "Review status" })
    .selectOption("pending");
  await expect(
    page.getByRole("button", { name: /^Review proposed action 1 / }),
  ).toBeVisible();
  await page.screenshot({
    path: "../.impeccable/review/review-inbox-desktop.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({
    path: "../.impeccable/review/review-inbox-mobile.png",
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

test("a late task lookup cannot open a review after leaving the inbox", async ({
  page,
}) => {
  await setup(page);
  let release!: () => void;
  let began!: () => void;
  const started = new Promise<void>((resolve) => {
    began = resolve;
  });
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route("**/api/v1/agent-runs/older-run-0?*", async (route) => {
    began();
    await held;
    return route.fulfill({
      json: {
        id: "older-run-0",
        goal: "Late task",
        status: "completed",
        revision: 3,
      },
    });
  });
  await page
    .getByRole("navigation")
    .getByRole("button", { name: /Reviews/ })
    .click();
  await page
    .getByRole("button", { name: /^Review proposed action 1 / })
    .click();
  await started;
  await page
    .getByRole("navigation")
    .getByRole("button", { name: /Home/ })
    .click();
  const response = page.waitForResponse((response) =>
    response.url().includes("/agent-runs/older-run-0?"),
  );
  release();
  await response;
  await expect(
    page.getByRole("complementary", { name: "Work details" }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("heading", {
      name: "A little direction. A lot of possibility.",
    }),
  ).toBeVisible();
});

test("failed review loads recover without claiming the inbox is empty", async ({
  page,
}) => {
  await setup(page);
  let fail = true;
  await page.route("**/api/v1/action-approvals?*", (route) => {
    if (new URL(route.request().url()).searchParams.get("limit") === "1")
      return route.fulfill({ json: [] });
    return fail
      ? route.fulfill({ status: 503, json: { error: "Temporary outage" } })
      : route.fulfill({ json: [] });
  });
  await page
    .getByRole("navigation")
    .getByRole("button", { name: /Reviews/ })
    .click();
  await expect(page.getByRole("alert")).toContainText("Could not load reviews");
  await expect(
    page.getByRole("heading", { name: "No actions waiting for review" }),
  ).toHaveCount(0);
  fail = false;
  await page.getByRole("button", { name: "Try again", exact: true }).click();
  await expect(page.getByText("No results.", { exact: true })).toBeFocused();
  await expect(
    page.getByRole("heading", { name: "No actions waiting for review" }),
  ).toBeVisible();
});
