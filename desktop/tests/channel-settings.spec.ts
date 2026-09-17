import { test, expect, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
const team = {
  deployment: {
    id: "research",
    activeVersion: "1",
    status: "active",
    revision: 1,
    roster: [],
  },
  definition: {
    displayName: "Research team",
    purpose: "Keep source decisions clear.",
    roles: [],
  },
};
const initial = {
  id: "channel",
  title: "Source decisions",
  owner: { type: "team", id: "research" },
  status: "active",
  revision: 2,
  lastSequence: 1,
};
async function visit(page: Page, title = "Source decisions", archived = false) {
  await page.goto("/");
  await page.keyboard.press("Control+5");
  await page.getByRole("button", { name: /Research team.*active/ }).click();
  await page.getByRole("button", { name: "Channels", exact: true }).click();
  if (archived)
    await page.getByLabel("Channel status").selectOption("archived");
  await page
    .getByRole("button", {
      name: archived ? `${title} · Archived` : title,
      exact: true,
    })
    .click();
}
async function setup(page: Page, get = () => initial, writable = true) {
  await page.route("**/api/v1/capabilities", (r) =>
    r.fulfill({
      json: {
        capabilities: [
          { id: "team-definitions", available: true, operations: ["list"] },
          {
            id: "channels",
            available: true,
            operations: [
              "list",
              "get",
              "read",
              "post",
              ...(writable ? ["update"] : []),
            ],
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/team-deployments?*", (r) =>
    r.fulfill({ json: { items: [team] } }),
  );
  await page.route("**/api/v1/conversations?*", (r) => {
    const p = new URL(r.request().url()).searchParams;
    expect(p.get("scopeId")).toBe("default");
    return r.fulfill({
      json: !p.has("status") || p.get("status") === get().status ? [get()] : [],
    });
  });
  await page.route("**/api/v1/conversations/channel?*", (r) =>
    r.fulfill({ json: get() }),
  );
  await page.route("**/api/v1/conversations/channel/messages?*", (r) =>
    r.fulfill({
      json: [
        {
          id: "one",
          conversationId: "channel",
          sequence: 1,
          sender: { type: "user", id: "local-operator" },
          content: "Keep the evidence attached to the decision.",
          intent: "update",
          audience: { kind: "channel" },
          createdAt: "2026-09-17T10:00:00Z",
        },
      ],
    }),
  );
  await visit(page);
}
async function settings(page: Page) {
  await page
    .getByRole("button", { name: "Channel settings", exact: true })
    .click();
  return page.getByRole("region", { name: "Channel settings", exact: true });
}

test("rename archive and restore preserve channel history and the message draft", async ({
  page,
}) => {
  let current = { ...initial };
  const writes: any[] = [];
  await page.route("**/api/v1/conversations/channel", (r) => {
    expect(r.request().method()).toBe("PATCH");
    const body = r.request().postDataJSON();
    expect(body.expectedRevision).toBe(current.revision);
    writes.push(body);
    current = {
      ...current,
      ...(body.title ? { title: body.title } : {}),
      ...(body.status ? { status: body.status } : {}),
      revision: current.revision + 1,
    };
    return r.fulfill({ json: current });
  });
  await setup(page, () => current);
  await page
    .getByLabel("Message to this channel", { exact: true })
    .fill("Keep my unfinished message.");
  const controls = await settings(page);
  await controls
    .getByLabel("Channel name", { exact: true })
    .fill("July source decisions");
  await controls
    .getByRole("button", { name: "Save channel name", exact: true })
    .click();
  await expect(
    controls.getByText("Channel renamed.", { exact: true }),
  ).toBeFocused();
  await expect(
    page.getByRole("heading", { name: current.title, exact: true }),
  ).toBeVisible();
  await controls
    .getByRole("button", { name: "Archive channel", exact: true })
    .click();
  await expect(
    controls.getByRole("button", { name: "Confirm archive", exact: true }),
  ).toBeFocused();
  await controls
    .getByRole("button", { name: "Keep current state", exact: true })
    .click();
  await expect(
    controls.getByRole("button", { name: "Archive channel", exact: true }),
  ).toBeFocused();
  await controls
    .getByRole("button", { name: "Archive channel", exact: true })
    .click();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await controls.scrollIntoViewIfNeeded();
  await page.screenshot({
    path: "../.impeccable/review/channel-settings-desktop.png",
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await controls.scrollIntoViewIfNeeded();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: "../.impeccable/review/channel-settings-mobile.png",
  });
  await controls
    .getByRole("button", { name: "Confirm archive", exact: true })
    .click();
  await expect(
    controls.getByText(
      "Channel archived. Its messages and your drafts are kept.",
      { exact: true },
    ),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Post message", exact: true }),
  ).toBeDisabled();
  await expect(
    page.getByLabel("Message to this channel", { exact: true }),
  ).toHaveValue("Keep my unfinished message.");
  await page.getByLabel("Channel status").selectOption("archived");
  await page
    .getByRole("button", {
      name: "July source decisions · Archived",
      exact: true,
    })
    .click();
  await settings(page);
  await controls
    .getByRole("button", { name: "Restore channel", exact: true })
    .click();
  await controls
    .getByRole("button", { name: "Confirm restore", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "Post message", exact: true }),
  ).toBeEnabled();
  expect(writes).toHaveLength(3);
});

test("conflicts preserve the name and uncertain changes are checked after reload without replay", async ({
  page,
}) => {
  let current = { ...initial },
    writes = 0;
  await page.route("**/api/v1/conversations/channel", (r) => {
    writes++;
    const body = r.request().postDataJSON();
    if (writes === 1) {
      current = {
        ...current,
        title: "Updated by another operator",
        revision: 3,
      };
      return r.fulfill({ status: 409, json: { error: "revision conflict" } });
    }
    expect(body.expectedRevision).toBe(3);
    current = { ...current, title: body.title, revision: 4 };
    return r.fulfill({ status: 503, json: { error: "response lost" } });
  });
  await setup(page, () => current);
  let controls = await settings(page);
  await controls.getByLabel("Channel name").fill("My reviewed name");
  await controls
    .getByRole("button", { name: "Save channel name", exact: true })
    .click();
  await expect(controls.getByRole("alert")).toContainText(
    "Check the saved channel",
  );
  await controls
    .getByRole("button", { name: "Check saved channel", exact: true })
    .click();
  await expect(controls.getByLabel("Channel name")).toHaveValue(
    "My reviewed name",
  );
  await controls
    .getByRole("button", { name: "Save channel name", exact: true })
    .click();
  await expect(controls.getByRole("alert")).toContainText("Could not confirm");
  await visit(page, "My reviewed name");
  controls = page.getByRole("region", {
    name: "Channel settings",
    exact: true,
  });
  await expect(controls.getByLabel("Channel name")).toBeDisabled();
  await controls
    .getByRole("button", { name: "Check saved channel", exact: true })
    .click();
  await expect(
    controls.getByText(
      "The requested channel state is saved. No change was sent again.",
      { exact: true },
    ),
  ).toBeFocused();
  expect(writes).toBe(2);
});

test("new remote names require review and late changes cannot replace another screen", async ({
  page,
}) => {
  let current = { ...initial };
  await setup(page, () => current);
  const controls = await settings(page);
  await controls.getByLabel("Channel name").fill("My draft");
  current = { ...current, title: "New saved name", revision: 3 };
  await page
    .getByRole("button", { name: "Refresh messages", exact: true })
    .click();
  await expect(controls.getByText(/The saved name is now/)).toBeVisible();
  await expect(
    controls.getByRole("button", { name: "Save channel name", exact: true }),
  ).toBeDisabled();
  await controls
    .getByRole("button", { name: "Check saved channel", exact: true })
    .click();
  let release!: () => void;
  const gate = new Promise<void>((resolve) => (release = resolve));
  await page.route("**/api/v1/conversations/channel", async (r) => {
    await gate;
    await r.fulfill({ json: { ...current, title: "My draft", revision: 4 } });
  });
  await controls
    .getByRole("button", { name: "Save channel name", exact: true })
    .click();
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  release();
  await expect(
    page.getByRole("heading", { name: "Settings", exact: true }),
  ).toBeVisible();
  await expect(page.locator(".channel-thread")).toHaveCount(0);
});

test("read-only capability hides management and unavailable recovery storage prevents a mutation", async ({
  page,
}) => {
  await setup(page, () => initial, false);
  await expect(
    page.getByRole("button", { name: "Channel settings", exact: true }),
  ).toHaveCount(0);
  await setup(page);
  const controls = await settings(page);
  let writes = 0;
  await page.route("**/api/v1/conversations/channel", (r) => {
    writes++;
    return r.fulfill({ json: initial });
  });
  await page.evaluate(() => {
    Storage.prototype.setItem = () => {
      throw new Error("storage disabled");
    };
  });
  await controls.getByLabel("Channel name").fill("Unsent rename");
  await controls
    .getByRole("button", { name: "Save channel name", exact: true })
    .click();
  await expect(
    controls.getByRole("alert").filter({ hasText: "Nothing was changed" }),
  ).toBeVisible();
  expect(writes).toBe(0);
});
