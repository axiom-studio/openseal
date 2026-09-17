import { test, expect, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { createHash } from "node:crypto";
const team = {
  deployment: {
    id: "evidence",
    activeVersion: "1",
    status: "active",
    revision: 1,
    roster: [],
  },
  definition: {
    displayName: "Evidence team",
    purpose: "Keep research decisions and source reviews together.",
    roles: [],
  },
};
const channel = {
  id: "evidence-notes",
  title: "Source review",
  owner: { type: "team", id: "evidence" },
  status: "active",
  revision: 2,
  lastSequence: 1,
};
const note = {
  id: "first",
  conversationId: channel.id,
  sequence: 1,
  sender: { type: "agent", id: "reviewer" },
  senderDisplayName: "Evidence reviewer",
  intent: "question",
  content: "Which reporting period should the team use?",
  createdAt: "2026-09-17T10:00:00Z",
  audience: { kind: "channel" },
};
async function setup(page: Page, writable = true, references = false) {
  await page.route("**/api/v1/capabilities", (r) =>
    r.fulfill({
      json: {
        capabilities: [
          { id: "team-definitions", available: true, operations: ["list"] },
          ...(references
            ? [
                { id: "agent-runs", available: true, operations: ["get"] },
                {
                  id: "artifacts",
                  available: true,
                  operations: ["get", "download"],
                },
              ]
            : []),
          {
            id: "channels",
            available: true,
            operations: [
              "list",
              "get",
              "read",
              ...(writable ? ["create", "post"] : []),
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
    expect(p.get("scopeKind")).toBe("local");
    expect(p.get("ownerId")).toBe(team.deployment.id);
    return r.fulfill({ json: [channel] });
  });
  await page.route("**/api/v1/conversations/evidence-notes?*", (r) =>
    r.fulfill({ json: channel }),
  );
  await page.goto("/");
  await expect(
    page.getByText("Connected locally", { exact: true }),
  ).toBeVisible();
  await page.keyboard.press("Control+5");
  await page.getByRole("button", { name: /Evidence team.*active/ }).click();
  await page.getByRole("button", { name: "Channels", exact: true }).click();
}
async function open(page: Page) {
  await page
    .getByRole("button", { name: "Source review", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "Source review", exact: true }),
  ).toBeVisible();
}

test("channel conversations are accessible and uncertain posts recover the same operation after reopening", async ({
  page,
}) => {
  let notes = [note];
  const keys: string[] = [];
  let original: any;
  await page.route("**/api/v1/conversations/evidence-notes/messages?*", (r) =>
    r.fulfill({ json: [...notes].reverse() }),
  );
  await page.route("**/api/v1/conversations/evidence-notes/messages", (r) => {
    const body = r.request().postDataJSON();
    const key = r.request().headers()["idempotency-key"];
    keys.push(key);
    if (keys.length === 1) {
      original = body;
      expect(body.sender).toEqual({ type: "user", id: "local-operator" });
      notes.push({
        ...note,
        id: "posted",
        sequence: 2,
        sender: body.sender,
        senderDisplayName: "",
        content: body.content,
        intent: "update",
      });
      return r.fulfill({ status: 503, json: { error: "Response lost" } });
    }
    expect(body).toEqual(original);
    expect(key).toBe(keys[0]);
    return r.fulfill({
      json: {
        conversation: { ...channel, revision: 3, lastSequence: 2 },
        message: notes[1],
        replayed: true,
      },
    });
  });
  await setup(page);
  await open(page);
  const editor = page.getByLabel("Message to this channel", { exact: true });
  await editor.fill(
    "Use July. Keep the date range explicit in the final report.",
  );
  await page.locator(".channel-thread").scrollIntoViewIfNeeded();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: "../.impeccable/review/team-channels-desktop.png",
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.locator(".channel-thread").scrollIntoViewIfNeeded();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: "../.impeccable/review/team-channels-mobile.png",
  });
  await page.getByRole("button", { name: "Post message", exact: true }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "Could not confirm" }),
  ).toBeVisible();
  await expect(editor).toBeDisabled();
  await page.setViewportSize({ width: 1440, height: 1000 });
  await setup(page);
  await open(page);
  await expect(editor).toHaveValue(original.content);
  await expect(editor).toBeDisabled();
  await page
    .getByRole("button", { name: "Retry posting message", exact: true })
    .click();
  await expect(editor).toHaveValue("");
  await expect(
    page.getByText("Message posted.", { exact: true }),
  ).toBeVisible();
  expect(keys).toHaveLength(2);
});

test("message history is cursor paginated and failures keep visible history; readonly channels remain readable", async ({
  page,
}) => {
  let offline = false;
  await page.route("**/api/v1/conversations/evidence-notes/messages?*", (r) => {
    if (offline) return r.fulfill({ status: 503, json: { error: "offline" } });
    const p = new URL(r.request().url()).searchParams;
    expect(p.get("order")).toBe("desc");
    expect(p.get("limit")).toBe("51");
    return r.fulfill({
      json:
        p.get("beforeSequence") === "11"
          ? [{ ...note, sequence: 10, content: "Earlier evidence note" }]
          : Array.from({ length: 51 }, (_, i) => ({
              ...note,
              id: `n${61 - i}`,
              sequence: 60 - i,
              content: `Evidence note ${60 - i}`,
            })),
    });
  });
  await setup(page, false);
  await open(page);
  await expect(
    page.getByRole("button", { name: "Post message", exact: true }),
  ).toHaveCount(0);
  await page
    .getByRole("button", { name: "Older messages", exact: true })
    .click();
  await expect(
    page.getByText("Earlier evidence note", { exact: true }),
  ).toBeVisible();
  offline = true;
  await page
    .getByRole("button", { name: "Refresh messages", exact: true })
    .click();
  await expect(
    page.getByRole("alert").filter({ hasText: "Previously loaded messages" }),
  ).toBeFocused();
  await expect(
    page.getByText("Earlier evidence note", { exact: true }),
  ).toBeVisible();
  offline = false;
  await page
    .getByRole("button", { name: "Latest messages", exact: true })
    .click();
  await expect(
    page.getByText("Evidence note 60", { exact: true }),
  ).toBeVisible();
});

test("revision conflicts keep drafts and replies preserve the original targeted audience", async ({
  page,
}) => {
  let posts = 0;
  const audience = {
    kind: "participants",
    participants: [{ type: "user", id: "local-operator" }],
  };
  await page.route("**/api/v1/conversations/evidence-notes/messages?*", (r) =>
    r.fulfill({
      json: [
        {
          ...note,
          audience,
          content: "Private scope question <script>bad()</script>",
        },
      ],
    }),
  );
  await page.route("**/api/v1/conversations/evidence-notes/messages", (r) => {
    posts++;
    const body = r.request().postDataJSON();
    expect(body.audience).toEqual(audience);
    expect(body.replyToMessageId).toBe(note.id);
    return r.fulfill({ status: 409, json: { error: "revision conflict" } });
  });
  await setup(page);
  await open(page);
  await page
    .getByRole("button", { name: "Reply to message 1", exact: true })
    .click();
  const input = page.getByLabel("Your reply", { exact: true });
  await expect(input).toBeFocused();
  await input.fill("Use July only.");
  await page.getByRole("button", { name: "Post message", exact: true }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "This channel changed" }),
  ).toBeVisible();
  await expect(input).toHaveValue("Use July only.");
  await expect(input).toBeEnabled();
  expect(posts).toBe(1);
  await page.getByRole("button", { name: "Cancel reply", exact: true }).click();
  await expect(
    page.getByLabel("Message to this channel", { exact: true }),
  ).toHaveValue("");
  await page
    .getByRole("button", { name: "Reply to message 1", exact: true })
    .click();
  await expect(input).toHaveValue("Use July only.");
});

test("creation persists its key, storage failure prevents posting, and late creation cannot reopen a team", async ({
  page,
}) => {
  await page.route("**/api/v1/conversations/evidence-notes/messages?*", (r) =>
    r.fulfill({ json: [] }),
  );
  await setup(page);
  await open(page);
  await page.evaluate(() => {
    Storage.prototype.setItem = () => {
      throw new Error("storage disabled");
    };
  });
  let posts = 0;
  await page.route("**/api/v1/conversations/evidence-notes/messages", (r) => {
    posts++;
    return r.fulfill({ json: {} });
  });
  await page
    .getByLabel("Message to this channel", { exact: true })
    .fill("Keep this unsent draft.");
  await page.getByRole("button", { name: "Post message", exact: true }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "Nothing was sent" }),
  ).toBeVisible();
  expect(posts).toBe(0);
  await setup(page);
  await page.getByRole("button", { name: "New channel", exact: true }).click();
  const title = page.getByLabel("Channel name", { exact: true });
  await expect(title).toBeFocused();
  await title.fill("Launch review");
  let release!: () => void;
  const gate = new Promise<void>((resolve) => (release = resolve));
  await page.route("**/api/v1/conversations", async (r) => {
    expect(r.request().headers()["idempotency-key"]).toBeTruthy();
    await gate;
    await r.fulfill({
      json: { ...channel, id: "new", title: "Launch review" },
    });
  });
  await page
    .getByRole("button", { name: "Create channel", exact: true })
    .click();
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  release();
  await expect(
    page.getByRole("heading", { name: "Settings", exact: true }),
  ).toBeVisible();
  await expect(page.locator(".channel-thread")).toHaveCount(0);
});

test("linked messages outside history open in place and preserve drafts and keyboard position", async ({
  page,
}) => {
  const linked = {
    ...note,
    id: "reply",
    sequence: 75,
    content:
      "July is confirmed. Keep the original question with this decision.",
    replyToMessageId: "first",
    resolvesMessageId: "first",
    supersedesMessageId: "previous",
  };
  await page.route("**/api/v1/conversations/evidence-notes/messages?*", (r) =>
    r.fulfill({ json: [linked] }),
  );
  let reads = 0;
  await page.route(
    "**/api/v1/conversations/evidence-notes/messages/first?*",
    (r) => {
      reads++;
      expect(new URL(r.request().url()).searchParams.get("scopeId")).toBe(
        "default",
      );
      return r.fulfill({ json: note });
    },
  );
  await page.route(
    "**/api/v1/conversations/evidence-notes/messages/previous?*",
    (r) =>
      r.fulfill({
        json: {
          ...note,
          id: "previous",
          sequence: 2,
          content: "Earlier decision <script>not executable</script>",
          audience: { kind: "roles", roles: ["reviewer"] },
        },
      }),
  );
  await setup(page);
  await open(page);
  const editor = page.getByLabel("Message to this channel", { exact: true });
  await editor.fill("My unfinished follow-up");
  const trigger = page.getByRole("button", {
    name: "Original message",
    exact: true,
  });
  await trigger.click();
  const context = page.getByRole("region", {
    name: "Original message",
    exact: true,
  });
  await expect(context.getByRole("heading")).toBeFocused();
  await expect(context.getByText(note.content, { exact: true })).toBeVisible();
  await expect(editor).toHaveValue("My unfinished follow-up");
  await expect(
    page.getByText("Messages 75–75.", { exact: true }),
  ).toBeVisible();
  await page.locator(".channel-messages > li").scrollIntoViewIfNeeded();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: "../.impeccable/review/channel-context-desktop.png",
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.locator(".channel-messages > li").scrollIntoViewIfNeeded();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: "../.impeccable/review/channel-context-mobile.png",
  });
  await context.getByRole("button", { name: "Close message" }).click();
  await expect(trigger).toBeFocused();
  await expect(context).toHaveCount(0);
  await page
    .getByRole("button", { name: "Resolved message", exact: true })
    .click();
  await expect(
    page
      .getByRole("region", { name: "Resolved message", exact: true })
      .getByText(note.content, { exact: true }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Replaced message", exact: true })
    .click();
  const replaced = page.getByRole("region", {
    name: "Replaced message",
    exact: true,
  });
  await expect(replaced.getByText("Audience: reviewer")).toBeVisible();
  await expect(
    replaced.getByText("Earlier decision <script>not executable</script>", {
      exact: true,
    }),
  ).toBeVisible();
  expect(reads).toBe(2);
});

test("missing or mismatched linked messages recover explicitly and late reads cannot reopen context", async ({
  page,
}) => {
  await page.route("**/api/v1/conversations/evidence-notes/messages?*", (r) =>
    r.fulfill({
      json: [{ ...note, id: "reply", sequence: 2, replyToMessageId: "first" }],
    }),
  );
  let mode = "missing";
  let release!: () => void;
  const gate = new Promise<void>((resolve) => (release = resolve));
  let requested = false;
  await page.route(
    "**/api/v1/conversations/evidence-notes/messages/first?*",
    async (r) => {
      if (mode === "missing")
        return r.fulfill({
          status: 404,
          json: { error: "Message unavailable" },
        });
      if (mode === "mismatch")
        return r.fulfill({
          json: {
            ...note,
            conversationId: "another-channel",
            content: "Do not display this",
          },
        });
      if (mode === "delayed") {
        requested = true;
        await gate;
      }
      return r.fulfill({ json: note });
    },
  );
  await setup(page, false);
  await open(page);
  const trigger = page.getByRole("button", {
    name: "Original message",
    exact: true,
  });
  await trigger.click();
  const context = page.getByRole("region", {
    name: "Original message",
    exact: true,
  });
  await expect(context.getByRole("alert")).toBeFocused();
  mode = "mismatch";
  await context.getByRole("button", { name: "Retry opening message" }).click();
  await expect(context.getByRole("alert")).toContainText("did not match");
  await expect(
    page.getByText("Do not display this", { exact: true }),
  ).toHaveCount(0);
  mode = "ready";
  await context.getByRole("button", { name: "Retry opening message" }).click();
  await expect(context.getByRole("heading")).toBeFocused();
  await context.getByRole("button", { name: "Close message" }).click();
  mode = "delayed";
  await trigger.click();
  await expect.poll(() => requested).toBe(true);
  await context.getByRole("button", { name: "Close message" }).click();
  release();
  await expect(trigger).toBeFocused();
  await expect(context).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Post message", exact: true }),
  ).toHaveCount(0);
});

const referencedRun = {
  id: "evidence-run",
  goal: "Review the July source notes",
  owner: { type: "team", id: "evidence" },
  status: "completed",
  revision: 3,
  createdAt: "2026-09-17T10:00:00Z",
  updatedAt: "2026-09-17T10:00:00Z",
};
const referenceText = Buffer.from(
  "# First saved report\n<script>window.executed=true</script>\nUncertainty is explicit.\n",
);
const referencedFile = {
  id: "evidence-report",
  name: "Source report.md",
  version: 1,
  mediaType: "text/markdown",
  sizeBytes: referenceText.length,
  digest: `sha256:${createHash("sha256").update(referenceText).digest("hex")}`,
  classification: "internal",
  provenance: {
    runId: referencedRun.id,
    producer: { type: "agent", id: "Evidence reviewer" },
  },
};
async function referenceMessage(
  page: Page,
  references = [
    { kind: "artifact", id: referencedFile.id, version: 1 },
    { kind: "run", id: referencedRun.id, version: 0 },
  ],
) {
  await page.route("**/api/v1/conversations/evidence-notes/messages?*", (r) =>
    r.fulfill({
      json: [
        {
          ...note,
          content:
            "The July report is ready for review. Its original version and producing work are linked below.",
          references,
        },
      ],
    }),
  );
  await page.route("**/api/v1/agent-runs/evidence-run?*", (r) =>
    r.fulfill({ json: referencedRun }),
  );
  await page.route("**/api/v1/artifacts/evidence-report?*", (r) => {
    expect(new URL(r.request().url()).searchParams.get("version")).toBe("1");
    return r.fulfill({ json: referencedFile });
  });
  await page.route("**/api/v1/artifacts/evidence-report/content?*", (r) => {
    expect(new URL(r.request().url()).searchParams.get("version")).toBe("1");
    return r.fulfill({ body: referenceText, contentType: "text/markdown" });
  });
}

test("channel evidence opens exact artifact versions and work without losing the conversation", async ({
  page,
}) => {
  await referenceMessage(page);
  await setup(page, true, true);
  await open(page);
  const editor = page.getByLabel("Message to this channel", { exact: true });
  await editor.fill("My unsent evidence review.");
  const fileButton = page.getByRole("button", {
    name: "Artifact · evidence-report · Version 1",
    exact: true,
  });
  await fileButton.click();
  const details = page.getByRole("region", {
    name: "Reference details",
    exact: true,
  });
  await expect(
    details.getByRole("heading", { name: "Referenced artifact" }),
  ).toBeFocused();
  await details
    .getByRole("button", { name: "Preview text", exact: true })
    .click();
  await expect(
    details.getByLabel("Text preview of Source report.md"),
  ).toBeFocused();
  await expect(
    details.getByLabel("Text preview of Source report.md"),
  ).toHaveText(referenceText.toString());
  expect(await page.evaluate(() => (window as any).executed)).toBeUndefined();
  await details.scrollIntoViewIfNeeded();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: "../.impeccable/review/channel-references-desktop.png",
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await details.scrollIntoViewIfNeeded();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: "../.impeccable/review/channel-references-mobile.png",
  });
  const download = page.waitForEvent("download");
  await details.getByRole("button", { name: "Save as…", exact: true }).click();
  expect((await download).suggestedFilename()).toBe("Source report.md");
  await expect(fileButton).toHaveAttribute("aria-expanded", "true");
  await details.getByRole("button", { name: "Close reference" }).click();
  await expect(fileButton).toBeFocused();
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page
    .getByRole("button", { name: "Work · evidence-run", exact: true })
    .click();
  await expect(
    details.getByText(referencedRun.goal, { exact: true }),
  ).toBeVisible();
  const openWork = details.getByRole("button", { name: "Open work inspector" });
  await openWork.evaluate((button) => button.click());
  await expect(
    page.getByRole("complementary", { name: "Work details" }),
  ).toContainText(referencedRun.goal);
  await page
    .getByRole("button", { name: "Close details", exact: true })
    .click();
  await expect(openWork).toBeFocused();
  await expect(editor).toHaveValue("My unsent evidence review.");
});

test("reference failures never substitute another work item or artifact version and closed reads stay closed", async ({
  page,
}) => {
  await referenceMessage(page);
  await page.route("**/api/v1/artifacts/evidence-report?*", (r) =>
    r.fulfill({ json: { ...referencedFile, version: 2 } }),
  );
  await page.route("**/api/v1/agent-runs/evidence-run?*", (r) =>
    r.fulfill({ status: 404, json: { error: "Work unavailable" } }),
  );
  await setup(page, true, true);
  await open(page);
  await page
    .getByRole("button", {
      name: "Artifact · evidence-report · Version 1",
      exact: true,
    })
    .click();
  const details = page.getByRole("region", {
    name: "Reference details",
    exact: true,
  });
  await expect(details.getByRole("alert")).toContainText(
    "did not match the referenced version",
  );
  await expect(
    details.getByRole("button", { name: "Preview text" }),
  ).toHaveCount(0);
  await page
    .getByRole("button", { name: "Work · evidence-run", exact: true })
    .click();
  await expect(details.getByRole("alert")).toBeFocused();
  await page.route("**/api/v1/agent-runs/evidence-run?*", (r) =>
    r.fulfill({ json: { ...referencedRun, id: "wrong-work" } }),
  );
  await details.getByRole("button", { name: "Retry opening work" }).click();
  await expect(details.getByRole("alert")).toContainText("did not match");
  let release!: () => void;
  const gate = new Promise<void>((resolve) => (release = resolve));
  let started = false;
  await page.route("**/api/v1/agent-runs/evidence-run?*", async (r) => {
    started = true;
    await gate;
    await r.fulfill({ json: referencedRun });
  });
  await details.getByRole("button", { name: "Retry opening work" }).click();
  await expect.poll(() => started).toBe(true);
  await details.getByRole("button", { name: "Close reference" }).click();
  release();
  await expect(details).toHaveCount(0);
  await expect(page.locator(".inspector")).toHaveCount(0);
});

test("unversioned files disclose latest-version resolution while unavailable references remain honest", async ({
  page,
}) => {
  await referenceMessage(page, [
    { kind: "artifact", id: referencedFile.id, version: 0 },
    { kind: "external_source", id: "source-record", version: 0 },
  ]);
  const seen: string[] = [];
  await page.route("**/api/v1/artifacts/evidence-report?*", (r) => {
    const version = new URL(r.request().url()).searchParams.get("version")!;
    seen.push(version);
    return r.fulfill({ json: { ...referencedFile, version: 2 } });
  });
  await page.route("**/api/v1/artifacts/evidence-report/content?*", (r) => {
    expect(new URL(r.request().url()).searchParams.get("version")).toBe("2");
    return r.fulfill({ body: referenceText, contentType: "text/markdown" });
  });
  await setup(page, false, true);
  await open(page);
  await page
    .getByRole("button", { name: "Artifact · evidence-report", exact: true })
    .click();
  await expect(
    page.getByText(/This reference does not pin a version/),
  ).toBeVisible();
  await page.getByRole("button", { name: "Preview text" }).click();
  await expect(
    page.getByLabel("Text preview of Source report.md"),
  ).toBeVisible();
  expect(seen).toContain("0");
  expect(seen.at(-1)).toBe("2");
  expect(seen.every((version) => version === "0" || version === "2")).toBe(
    true,
  );
  await expect(
    page.getByText(
      /External source · source-record — Opening this reference is unavailable/,
    ),
  ).toBeVisible();
  await setup(page, false, false);
  await open(page);
  await expect(
    page.getByRole("button", {
      name: "Artifact · evidence-report",
      exact: true,
    }),
  ).toHaveCount(0);
  await expect(
    page.getByText(
      /Artifact · evidence-report — Opening this reference is unavailable/,
    ),
  ).toBeVisible();
});
