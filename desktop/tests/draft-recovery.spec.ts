import { test, expect } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";

test("task drafts restore their mode and assignment, and reject an unavailable saved agent", async ({
  page,
}) => {
  let active = true;
  let creates = 0;
  await page.route("**/api/v1/capabilities", (route) =>
    route.fulfill({
      json: {
        capabilities: [
          { id: "agent-definitions", available: true, operations: ["list"] },
          { id: "agent-runs", available: true, operations: ["list", "create"] },
        ],
      },
    }),
  );
  await page.route("**/api/v1/agent-deployments?*", (route) =>
    route.fulfill({
      json: {
        items: [
          {
            deployment: {
              id: "analyst",
              activeVersion: "1",
              rolloutStatus: active ? "active" : "inactive",
            },
            definition: { displayName: "Analyst" },
          },
          {
            deployment: {
              id: "reviewer",
              activeVersion: "1",
              rolloutStatus: "active",
            },
            definition: { displayName: "Reviewer" },
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/agent-runs?*", (route) =>
    route.fulfill({ json: [] }),
  );
  await page.route("**/api/v1/agent-runs", (route) => {
    creates++;
    return route.fulfill({
      status: 500,
      json: { error: "Unexpected submission" },
    });
  });
  await page.goto("/");
  await page
    .getByRole("button", { name: "Start work", exact: true })
    .first()
    .click();
  await page
    .getByRole("textbox", { name: "Describe the work" })
    .fill("Review my unfinished evidence notes.");
  await page
    .getByRole("combobox", { name: "Assign to" })
    .selectOption("analyst");
  await page.reload();
  await expect(
    page.getByRole("textbox", { name: "Describe the work" }),
  ).toHaveValue("Review my unfinished evidence notes.");
  await expect(page.getByRole("combobox", { name: "Assign to" })).toHaveValue(
    "analyst",
  );
  await expect(page.locator('form button[type="submit"]')).toBeEnabled();
  expect(creates).toBe(0);
  active = false;
  await page.reload();
  const picker = page.getByRole("combobox", { name: "Assign to" });
  await expect(picker).toHaveValue("analyst");
  await expect(picker).toHaveAccessibleDescription(
    /assigned agent is unavailable/,
  );
  await expect(page.locator('form button[type="submit"]')).toBeDisabled();
  await page
    .getByRole("textbox", { name: "Describe the work" })
    .press("Control+Enter");
  expect(creates).toBe(0);
  await page.screenshot({
    path: "../.impeccable/review/draft-recovery-desktop.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({
    path: "../.impeccable/review/draft-recovery-mobile.png",
    fullPage: true,
  });
  expect(
    (
      await new AxeBuilder({ page })
        .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
        .analyze()
    ).violations,
  ).toEqual([]);
  await picker.selectOption("reviewer");
  await expect(page.locator('form button[type="submit"]')).toBeEnabled();
  await page.reload();
  await expect(picker).toHaveValue("reviewer");
  await expect(
    page.getByRole("textbox", { name: "Describe the work" }),
  ).toHaveValue("Review my unfinished evidence notes.");
  expect(creates).toBe(0);
});

test("unreadable saved context recovers a legacy prompt and keeps it editable", async ({
  page,
}) => {
  await page.addInitScript(() => {
    localStorage.setItem(
      "openseal.draft",
      '{"version":1,"prompt":42,"mode":"work","agentID":"unknown"}',
    );
    localStorage.setItem("openseal.prompt", "My existing agent idea");
  });
  await page.goto("/");
  const prompt = page.getByRole("textbox", { name: "Describe your agent" });
  await expect(prompt).toHaveValue("My existing agent idea");
  await prompt.fill("Updated agent idea");
  await expect
    .poll(() =>
      page.evaluate(() =>
        JSON.parse(localStorage.getItem("openseal.draft") || "{}"),
      ),
    )
    .toEqual({
      version: 1,
      prompt: "Updated agent idea",
      mode: "agent",
      agentID: "",
    });
});
