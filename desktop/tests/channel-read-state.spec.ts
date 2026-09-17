import { test, expect, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
const scope = { kind: "local", id: "default" };
const participant = { type: "user", id: "local-operator" };
const saved = {
  scope,
  conversationId: "notes",
  participant,
  readSequence: 10,
  deliveredSequence: 10,
  revision: 1,
};
async function setup(page: Page, receipts = true, projection?: () => object) {
  const channel = {
    id: "notes",
    title: "Evidence notes",
    owner: { type: "team", id: "research" },
    status: "active",
    revision: 121,
    lastSequence: 120,
  };
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
              ...(receipts ? ["receipts"] : []),
            ],
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/team-deployments?*", (r) =>
    r.fulfill({
      json: {
        items: [
          {
            deployment: {
              id: "research",
              activeVersion: "1",
              status: "active",
              revision: 1,
              roster: [],
            },
            definition: {
              displayName: "Research team",
              purpose: "Review evidence together.",
              roles: [],
            },
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/conversations?*", (r) =>
    r.fulfill({ json: [{ ...channel, ...projection?.() }] }),
  );
  await page.route("**/api/v1/conversations/notes?*", (r) =>
    r.fulfill({ json: channel }),
  );
  await page.route("**/api/v1/conversations/notes/messages?*", (r) => {
    const p = new URL(r.request().url()).searchParams;
    let all = Array.from({ length: 120 }, (_, i) => ({
      id: `note-${i + 1}`,
      conversationId: "notes",
      sequence: i + 1,
      sender: { type: "agent", id: "analyst" },
      intent: "update",
      audience: { kind: "channel" },
      createdAt: "2026-09-17T10:00:00Z",
      content: `Evidence note ${i + 1}`,
    }));
    if (p.has("afterSequence"))
      all = all.filter((m) => m.sequence > Number(p.get("afterSequence")));
    const before = Number(p.get("beforeSequence"));
    if (before) all = all.filter((m) => m.sequence < before);
    if (p.get("order") === "desc") all.reverse();
    return r.fulfill({ json: all.slice(0, Number(p.get("limit"))) });
  });
  await page.goto("/");
  await expect(
    page.getByText("Connected locally", { exact: true }),
  ).toBeVisible();
  await page.keyboard.press("Control+5");
  await page.getByRole("button", { name: /Research team.*active/ }).click();
  await page.getByRole("button", { name: "Channels", exact: true }).click();
  await page
    .getByRole("button", { name: /^Evidence notes(?: · \d+ unread)?$/ })
    .click();
}
test("unread navigation starts after the durable reader position and marking is explicit and bounded to displayed history", async ({
  page,
}) => {
  let cursor = { ...saved };
  const writes: any[] = [];
  await page.route("**/api/v1/conversations/notes/cursor**", (r) => {
    if (r.request().method() === "PUT") {
      const body = r.request().postDataJSON();
      writes.push(body);
      expect(body.participant).toEqual(participant);
      expect(body.scope).toEqual(scope);
      expect(body.expectedRevision).toBe(cursor.revision);
      cursor = { ...cursor, ...body, revision: cursor.revision + 1 };
      return r.fulfill({ json: { cursor } });
    }
    return r.fulfill({ json: cursor });
  });
  await setup(page);
  const read = page.getByRole("region", { name: "Your reading position" });
  await expect(read).toContainText("110 unread messages");
  await expect(
    page.getByText("Evidence note 71", { exact: true }),
  ).toBeVisible();
  expect(writes).toEqual([]);
  await read.getByRole("button", { name: "Start at unread messages" }).click();
  await expect(
    page.getByText("Evidence note 11", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("Evidence note 60", { exact: true }),
  ).toBeVisible();
  await expect(page.getByText("Evidence note 61", { exact: true })).toHaveCount(
    0,
  );
  await read.getByRole("button", { name: "Start at unread messages" }).click();
  await expect(page.getByText("Evidence note 11", { exact: true })).toBeVisible(
    {
      timeout: 1500,
    },
  );
  await read.scrollIntoViewIfNeeded();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: "../.impeccable/review/channel-read-desktop.png",
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await read.scrollIntoViewIfNeeded();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: "../.impeccable/review/channel-read-mobile.png",
  });
  await page.setViewportSize({ width: 1440, height: 1000 });
  await read
    .getByRole("button", { name: "Mark read through message 60", exact: true })
    .click();
  await expect(read.getByRole("status")).toContainText("60 unread messages");
  await expect(read.getByRole("status")).toBeFocused();
  expect(writes).toHaveLength(1);
  expect(writes[0].readSequence).toBe(60);
  await page
    .getByRole("button", { name: "Newer messages", exact: true })
    .click();
  await expect(
    page.getByText("Evidence note 61", { exact: true }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Latest messages", exact: true })
    .click();
  await expect(
    page.getByText("Evidence note 120", { exact: true }),
  ).toBeVisible();
  await page.reload();
  await expect(
    page.getByText("Connected locally", { exact: true }),
  ).toBeVisible();
  await page.keyboard.press("Control+5");
  await page.getByRole("button", { name: /Research team.*active/ }).click();
  await page.getByRole("button", { name: "Channels", exact: true }).click();
  await page
    .getByRole("button", { name: "Evidence notes", exact: true })
    .click();
  await expect(read).toContainText("You marked read through message 60");
  expect(writes).toHaveLength(1);
});
test("uncertain writes require checking the saved position without replay and mismatched readers stay unavailable", async ({
  page,
}) => {
  let cursor = { ...saved };
  let writes = 0;
  let mismatch = true;
  await page.route("**/api/v1/conversations/notes/cursor**", (r) => {
    if (r.request().method() === "PUT") {
      writes++;
      cursor = {
        ...cursor,
        readSequence: 120,
        deliveredSequence: 120,
        revision: 2,
      };
      return r.abort("failed");
    }
    return r.fulfill({
      json: mismatch
        ? { ...cursor, participant: { type: "agent", id: "analyst" } }
        : cursor,
    });
  });
  await setup(page);
  const read = page.getByRole("region", { name: "Your reading position" });
  await expect(read.getByRole("alert")).toContainText("does not match");
  await expect(read.getByRole("button", { name: /Mark read/ })).toHaveCount(0);
  mismatch = false;
  await read.getByRole("button", { name: "Refresh read position" }).click();
  await read
    .getByRole("button", { name: "Mark read through message 120" })
    .click();
  await expect(read.getByRole("alert")).toContainText(
    "Check the saved position",
  );
  await expect(read.getByRole("button", { name: /Mark read/ })).toBeDisabled();
  await read.getByRole("button", { name: "Check saved read position" }).click();
  await expect(read).toContainText("You’re caught up");
  expect(writes).toBe(1);
});
test("without receipt capability no read-position requests or controls appear", async ({
  page,
}) => {
  let requests = 0;
  await page.route("**/api/v1/conversations/notes/cursor**", (r) => {
    requests++;
    return r.fulfill({ json: saved });
  });
  await setup(page, false);
  await expect(
    page.getByText("Evidence note 71", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("region", { name: "Your reading position" }),
  ).toHaveCount(0);
  expect(requests).toBe(0);
});

test("invalid message sequences cannot advance a read position", async ({
  page,
}) => {
  let writes = 0;
  await page.route("**/api/v1/conversations/notes/cursor**", (r) => {
    if (r.request().method() !== "GET") writes++;
    return r.fulfill({ json: saved });
  });
  await setup(page);
  await page.route("**/api/v1/conversations/notes/messages?*", (r) =>
    r.fulfill({
      json: [
        {
          id: "forged",
          conversationId: "notes",
          sequence: 1.5,
          sender: participant,
          intent: "update",
          audience: { kind: "channel" },
          createdAt: "2026-09-17T10:00:00Z",
          content: "Invalid sequence",
        },
      ],
    }),
  );
  await page
    .getByRole("button", { name: "Refresh messages", exact: true })
    .click();
  await expect(page.locator(".channel-thread > [role=alert]")).toContainText(
    "Messages do not match",
  );
  await expect(
    page.getByRole("button", { name: "Mark read through message 120" }),
  ).toBeDisabled();
  expect(writes).toBe(0);
});

test("channel list unread counts share its scoped request and never regress a newer saved read position", async ({
  page,
}) => {
  let cursor = { ...saved };
  let lastSequence = 120;
  let malformed = false;
  const listQueries: URLSearchParams[] = [];
  let cursorReads = 0;
  page.on("request", (r) => {
    const url = new URL(r.url());
    if (url.pathname === "/api/v1/conversations")
      listQueries.push(url.searchParams);
  });
  await page.route("**/api/v1/conversations/notes/cursor**", (r) => {
    if (r.request().method() === "PUT") {
      cursor = { ...cursor, ...r.request().postDataJSON(), revision: 2 };
      return r.fulfill({ json: { cursor } });
    }
    cursorReads++;
    return r.fulfill({ json: cursor });
  });
  await setup(page, true, () => ({
    lastSequence,
    revision: lastSequence + 1,
    readPosition: {
      participant: malformed ? { type: "agent", id: "analyst" } : participant,
      readSequence: 10,
      revision: 1,
    },
  }));
  const picker = page.getByRole("group", { name: "Choose a channel" });
  await expect(
    picker.getByRole("button", {
      name: "Evidence notes · 110 unread",
      exact: true,
    }),
  ).toBeVisible();
  expect(listQueries.length).toBeGreaterThan(0);
  for (const query of listQueries) {
    expect(query.get("participantType")).toBe("user");
    expect(query.get("participantId")).toBe("local-operator");
    expect(query.get("scopeId")).toBe("default");
    expect(query.get("limit")).toBe("21");
  }
  await expect(
    page.getByRole("region", { name: "Your reading position" }),
  ).toContainText("You marked read through message 10");
  expect(cursorReads).toBe(1); // only the selected conversation requests its full cursor
  await picker.scrollIntoViewIfNeeded();
  await page.screenshot({
    path: "../.impeccable/review/channel-unread-list-desktop.png",
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await picker.scrollIntoViewIfNeeded();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await page.screenshot({
    path: "../.impeccable/review/channel-unread-list-mobile.png",
  });
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page
    .getByRole("button", { name: "Mark read through message 120", exact: true })
    .click();
  await expect(
    picker.getByRole("button", { name: "Evidence notes", exact: true }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Refresh channels", exact: true })
    .click();
  await expect(page.locator('.team-channels > p[role="status"]')).toContainText(
    "1 channel on this page",
  );
  await expect(
    picker.getByRole("button", { name: "Evidence notes", exact: true }),
  ).toBeVisible();
  lastSequence = 125;
  await page
    .getByRole("button", { name: "Refresh channels", exact: true })
    .click();
  await expect(
    picker.getByRole("button", {
      name: "Evidence notes · 5 unread",
      exact: true,
    }),
  ).toBeVisible();
  malformed = true;
  await page
    .getByRole("button", { name: "Refresh channels", exact: true })
    .click();
  await expect(page.locator('.team-channels > p[role="alert"]')).toContainText(
    "Unread counts do not match",
  );
  await expect(
    picker.getByRole("button", {
      name: "Evidence notes · 5 unread",
      exact: true,
    }),
  ).toBeVisible();
});
