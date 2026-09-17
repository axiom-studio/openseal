import { test, expect } from "@playwright/test";

test("real daemon connects, setup is actionable, and prompt survives reload", async ({
  page,
}) => {
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.goto("/");
  await expect(
    page.getByText("Connected locally", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "Your work will find a home here" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Check setup", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "Settings", exact: true }),
  ).toBeVisible();
  await expect(page.getByLabel("Provider URL", { exact: true })).toBeVisible();
  await page
    .getByRole("button", { name: "Create agent", exact: true })
    .first()
    .click();
  await page
    .getByRole("textbox", { name: "Describe your agent" })
    .fill("Help me review research evidence.");
  await page.reload();
  await expect(
    page.getByRole("textbox", { name: "Describe your agent" }),
  ).toHaveValue("Help me review research evidence.");
  await page.getByRole("textbox", { name: "Describe your agent" }).fill("");
  await expect(
    page.getByRole("heading", { name: "Your work will find a home here" }),
  ).toBeVisible();
  await page
    .getByRole("heading", { name: "A little direction. A lot of possibility." })
    .click();
  await page.screenshot({
    path: "../.impeccable/review/desktop.png",
    fullPage: true,
  });
  expect(errors).toEqual([]);
});

test("keyboard navigation, empty search recovery, and theme selection", async ({
  page,
}) => {
  await page.goto("/");
  await expect(
    page.getByText("Connected locally", { exact: true }),
  ).toBeVisible();
  await page.keyboard.press("Control+k");
  await expect(page.getByRole("dialog")).toBeVisible();
  await page.getByRole("textbox", { name: "Search commands" }).fill("Agents");
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("Enter");
  await expect(
    page.getByRole("heading", { name: "Agents", exact: true }),
  ).toBeVisible();
  await page
    .getByRole("searchbox", { name: "Search agents" })
    .fill("unmatched");
  await expect(
    page.getByRole("heading", { name: "No matching agents" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Clear search", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "Meet your next teammate" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  await page
    .getByRole("combobox", { name: "Theme", exact: true })
    .selectOption("dark");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await page.screenshot({
    path: "../.impeccable/review/dark-settings.png",
    fullPage: true,
  });
});

test("small window preserves navigation and has no horizontal overflow", async ({
  page,
}) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/");
  await expect(
    page.getByText("Your work will find a home here", { exact: true }),
  ).toBeVisible();
  await page.screenshot({
    path: "../.impeccable/review/mobile.png",
    fullPage: true,
  });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await page.getByRole("button", { name: "Open navigation" }).click();
  await page.keyboard.press("Escape");
  await expect(
    page.getByRole("button", { name: "Open navigation" }),
  ).toBeFocused();
  await page.getByRole("button", { name: "Open navigation" }).click();
  await page.getByRole("button", { name: "Work Ctrl 3", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "Work", exact: true }),
  ).toBeVisible();
});

// Synthetic API fixtures isolate concurrency and capability behavior from model services.
test("work commands carry the current revision and inspector follows the result", async ({
  page,
}) => {
  let run = {
    id: "run-fixture",
    goal: "Compare research evidence",
    status: "running",
    owner: { type: "agent", id: "researcher" },
    revision: 7,
    createdAt: "2026-09-17T08:00:00Z",
    updatedAt: "2026-09-17T08:05:00Z",
  };
  let command: unknown;
  await page.route("**/api/v1/capabilities", (route) =>
    route.fulfill({
      json: {
        capabilities: [
          {
            id: "agent-runs",
            available: true,
            operations: ["list", "get", "pause", "resume"],
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/agent-runs?*", (route) =>
    route.fulfill({ json: [run] }),
  );
  await page.route(
    "**/api/v1/agent-runs/run-fixture/commands?*",
    async (route) => {
      command = route.request().postDataJSON();
      run = { ...run, status: "paused", revision: 8 };
      await route.fulfill({ json: { run } });
    },
  );
  await page.goto("/");
  await page.getByRole("button", { name: /Compare research evidence/ }).click();
  await expect(
    page.getByRole("complementary", { name: "Work details" }),
  ).toBeVisible();
  await expect(
    page.getByRole("complementary", { name: "Work details" }),
  ).toBeFocused();
  await page.getByRole("button", { name: "Pause work", exact: true }).click();
  expect(command).toMatchObject({ kind: "pause", expectedRevision: 7 });
  await expect(
    page.getByRole("button", { name: "Resume", exact: true }),
  ).toBeVisible();
  await page.screenshot({
    path: "../.impeccable/review/work-inspector.png",
    fullPage: true,
  });
});

test("core screens and command menu meet automated accessibility checks", async ({
  page,
}) => {
  const { default: AxeBuilder } = await import("@axe-core/playwright");
  await page.goto("/");
  await expect(
    page.getByRole("heading", { name: "Your work will find a home here" }),
  ).toBeVisible();
  expect(
    (
      await new AxeBuilder({ page })
        .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
        .analyze()
    ).violations,
  ).toEqual([]);
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  expect(
    (
      await new AxeBuilder({ page })
        .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
        .analyze()
    ).violations,
  ).toEqual([]);
  await page.keyboard.press("Control+k");
  expect(
    (
      await new AxeBuilder({ page })
        .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
        .analyze()
    ).violations,
  ).toEqual([]);
});

test("provider form saves privately, reconnects the real daemon, and retains a key on edits", async ({
  page,
  request,
}) => {
  const { readFile, stat } = await import("node:fs/promises");
  const { join } = await import("node:path");
  const workspace = process.env.OPENSEAL_UI_TEST_WORKSPACE!;
  await page.goto("/");
  await expect(
    page.getByText("Connected locally", { exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  await expect(page.getByLabel("Provider URL", { exact: true })).toHaveValue(
    "https://api.openai.com/v1",
  );
  // A loopback URL and synthetic key verify persistence without contacting a paid provider.
  await page
    .getByLabel("Provider URL", { exact: true })
    .fill("http://127.0.0.1:19999/v1");
  await page.getByLabel("Model", { exact: true }).fill("desktop-test-model");
  await page
    .getByLabel("API key", { exact: true })
    .fill("synthetic-desktop-test-key");
  await page
    .getByRole("button", { name: "Save and connect", exact: true })
    .click();
  await expect(
    page.getByText("Provider settings saved. Workspace reconnected.", {
      exact: true,
    }),
  ).toBeVisible({ timeout: 40000 });
  await expect(page.getByLabel("API key", { exact: true })).toHaveValue("");
  await expect(page.getByText("Saved securely", { exact: true })).toBeVisible();
  const response = await request.get("/__desktop/provider");
  expect(response.ok()).toBe(true);
  const body = await response.json();
  expect(body).toEqual({
    baseUrl: "http://127.0.0.1:19999/v1",
    model: "desktop-test-model",
    hasApiKey: true,
  });
  expect(JSON.stringify(body)).not.toContain("synthetic-desktop-test-key");
  const config = await readFile(join(workspace, "context.yaml"), "utf8");
  expect(config).not.toContain("synthetic-desktop-test-key");
  const secretPath = config.match(/file: (.+)/)![1].trim();
  expect(await readFile(join(workspace, secretPath), "utf8")).toBe(
    "synthetic-desktop-test-key",
  );
  if (process.platform !== "win32")
    expect((await stat(join(workspace, secretPath))).mode & 0o777).toBe(0o600);
  const capabilities = await (await request.get("/api/v1/capabilities")).json();
  expect(
    capabilities.capabilities.some(
      (c: { id: string; operations: string[] }) =>
        c.id === "workforce-authoring" && c.operations.includes("propose"),
    ),
  ).toBe(true);
  await page.getByLabel("Model", { exact: true }).fill("updated-test-model");
  await page.getByRole("button", { name: "Save changes", exact: true }).click();
  await expect(
    page.getByText("Provider settings saved. Workspace reconnected.", {
      exact: true,
    }),
  ).toBeVisible({ timeout: 40000 });
  await page.reload();
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  await expect(page.getByLabel("Model", { exact: true })).toHaveValue(
    "updated-test-model",
  );
  await expect(page.getByLabel("API key", { exact: true })).toHaveValue("");
  expect(await readFile(join(workspace, secretPath), "utf8")).toBe(
    "synthetic-desktop-test-key",
  );
  await page.screenshot({
    path: "../.impeccable/review/provider-settings.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({
    path: "../.impeccable/review/provider-mobile.png",
    fullPage: true,
  });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
});

test("outdated native build explains restart instead of offering a reload", async ({
  page,
}) => {
  await page.goto("/");
  await expect(
    page.getByText("Connected locally", { exact: true }),
  ).toBeVisible();
  await page.evaluate(() => {
    Object.assign(window, {
      isTauri: true,
      __TAURI_INTERNALS__: {
        invoke: (command: string) =>
          command === "desktop_status"
            ? Promise.resolve({
                state: "ready",
                message: "",
                workspace: "/test",
              })
            : Promise.reject(`Command ${command} not found`),
      },
    });
  });
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText(
    "Close and reopen OpenSeal",
  );
  await expect(
    page.getByRole("button", { name: "Reload settings" }),
  ).toHaveCount(0);
  await expect(page.getByLabel("Provider URL", { exact: true })).toBeDisabled();
});

test("work results wrap long text, report copy failure, and explain canceled work", async ({
  page,
}) => {
  const longText = "Result: " + "reference".repeat(90);
  const runs = [
    {
      id: "long-result",
      goal: "Review lengthy output",
      status: "completed",
      revision: 1,
      output: { reply: longText },
    },
    {
      id: "canceled-result",
      goal: "Stopped analysis",
      status: "canceled",
      revision: 1,
    },
  ];
  await page.route("**/api/v1/capabilities", (route) =>
    route.fulfill({
      json: {
        capabilities: [
          { id: "agent-runs", available: true, operations: ["get", "list"] },
        ],
      },
    }),
  );
  await page.route("**/api/v1/agent-runs?*", (route) =>
    route.fulfill({ json: runs }),
  );
  await page.addInitScript(() =>
    Object.defineProperty(navigator, "clipboard", {
      value: {
        writeText: async () => {
          throw new Error("Clipboard unavailable");
        },
      },
    }),
  );
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/");
  await page.getByRole("button", { name: /Review lengthy output/ }).click();
  const inspector = page.getByRole("complementary", { name: "Work details" });
  await expect(inspector.getByText(longText, { exact: true })).toBeVisible();
  expect(
    await inspector.evaluate((el) => el.scrollWidth <= el.clientWidth),
  ).toBe(true);
  await inspector
    .getByRole("button", { name: "Copy result", exact: true })
    .click();
  await expect(
    inspector.getByRole("region", { name: "Work result" }).getByRole("status"),
  ).toHaveText("Could not copy. Select the result text and copy it manually.");
  await page
    .getByRole("button", { name: "Close details", exact: true })
    .click();
  await page.getByRole("button", { name: /Stopped analysis/ }).click();
  await expect(
    inspector.getByText("This run ended without a result.", { exact: true }),
  ).toBeVisible();
  await expect(
    inspector.getByText("Results will appear when the agent produces them."),
  ).toHaveCount(0);
});
