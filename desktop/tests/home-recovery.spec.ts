import { test, expect, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
const scope = { kind: "local", id: "default" };
const builder = "openseal-assistant";
// builderStart is the daemon's answer to POST /agent-builder/conversations:
// the builder chat and its first message, the request exactly as typed.
function builderStart(
  id: string,
  prompt: string,
  kind: "agent" | "team",
  conversationScope = scope,
) {
  return {
    conversation: {
      id,
      scope: conversationScope,
      owner: { type: "agent", id: builder },
      origin: { kind: "agent_builder", id: "request" },
      title: "New agent",
      status: "active",
      revision: 1,
      lastSequence: 1,
    },
    message: {
      id: `${id}-message`,
      conversationId: id,
      sequence: 1,
      sender: { type: "user", id: "local-operator" },
      intent: "update",
      content: prompt,
      builderParameters: { kind },
      createdAt: "2026-11-08T10:00:00Z",
      audience: { kind: "channel" },
    },
    replayed: false,
  };
}
async function builderChat(page: Page, id: string) {
  await page.route(`**/api/v1/conversations/${id}?*`, (r) =>
    r.fulfill({ json: builderStart(id, "", "agent").conversation }),
  );
  await page.route(`**/api/v1/conversations/${id}/messages?*`, (r) =>
    r.fulfill({ json: [] }),
  );
}
async function setup(page: Page) {
  await page.route("**/api/v1/capabilities", (r) =>
    r.fulfill({
      json: {
        capabilities: [
          {
            id: "agent-builder",
            available: true,
            operations: ["create", "list"],
            context: { builderAgentId: builder },
          },
          { id: "agent-definitions", available: true, operations: ["list"] },
          {
            id: "agent-runs",
            available: true,
            operations: ["create", "get", "list"],
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/agent-deployments?*", (r) =>
    r.fulfill({
      json: {
        items: [
          {
            deployment: {
              id: "analyst",
              rolloutStatus: "active",
              activeVersion: "1",
            },
            definition: {
              displayName: "Evidence analyst",
              purpose: "Review sources",
            },
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/agent-runs?*", (r) => r.fulfill({ json: [] }));
  await page.goto("/");
  await expect(
    page.getByText("Connected locally", { exact: true }),
  ).toBeVisible();
}
for (const mode of ["agent", "team", "work"] as const) {
  test(`${mode} creation replays the exact saved request after a lost response and reload`, async ({
    page,
  }) => {
    const writes: { key: string; body: any }[] = [];
    const prompt = `Review evidence for this ${mode}.`;
    const path =
      mode === "work" ? "/agent-runs" : "/agent-builder/conversations";
    const record =
      mode === "work"
        ? {
            id: "saved-work",
            scope,
            kind: "agent_work",
            owner: { type: "agent", id: "analyst" },
            assignedAgentId: "analyst",
            goal: prompt,
            status: "queued",
            revision: 1,
            createdAt: "2026-09-17T10:00:00Z",
            updatedAt: "2026-09-17T10:00:00Z",
          }
        : builderStart("saved-conversation", prompt, mode);
    await page.route(`**/api/v1${path}`, async (r) => {
      writes.push({
        key: r.request().headers()["idempotency-key"],
        body: r.request().postDataJSON(),
      });
      if (writes.length === 1) return r.abort("failed");
      return r.fulfill({ json: mode === "work" ? { run: record } : record });
    });
    await builderChat(page, "saved-conversation");
    await setup(page);
    if (mode !== "agent")
      await page
        .getByRole("group", { name: "What to create" })
        .getByRole("button", {
          name: mode === "team" ? "Create a team" : "Start work",
          exact: true,
        })
        .click();
    if (mode === "work")
      await page.getByLabel("Assign to").selectOption("analyst");
    const editor = page.locator("#prompt");
    await editor.fill(prompt);
    await page.locator(".composer button[type=submit]").click();
    const recovery = page.getByRole("region", {
      name: "Saved request recovery",
    });
    await expect(recovery.getByRole("alert")).toContainText(
      "Could not confirm",
    );
    await expect(recovery.getByRole("alert")).toBeFocused();
    await expect(editor).toHaveAttribute("readonly", "");
    const saved = await page.evaluate(() =>
      JSON.parse(localStorage.getItem("openseal.home-submission")!),
    );
    expect(saved.key).toBe(writes[0].key);
    expect(saved.body).toEqual(writes[0].body);
    // R7: the request as typed, the kind as a parameter, no text prefix.
    if (mode !== "work")
      expect(writes[0].body).toEqual({
        scope,
        prompt,
        parameters: { kind: mode },
      });
    await page
      .getByRole("button", { name: "Create agent", exact: true })
      .first()
      .click();
    await expect(editor).toHaveValue(prompt);
    await page.reload();
    await expect(
      page.getByRole("button", { name: "Retry saved request", exact: true }),
    ).toBeVisible();
    expect(writes).toHaveLength(1);
    await expect(editor).toHaveValue(prompt);
    if (mode === "team") {
      expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
      await page.screenshot({
        path: "../.impeccable/review/home-recovery-desktop.png",
      });
      await page.setViewportSize({ width: 390, height: 844 });
      await page.locator(".composer").scrollIntoViewIfNeeded();
      expect(
        await page.evaluate(
          () => document.documentElement.scrollWidth > innerWidth,
        ),
      ).toBe(false);
      await page.screenshot({
        path: "../.impeccable/review/home-recovery-mobile.png",
      });
    }
    await page
      .getByRole("button", { name: "Retry saved request", exact: true })
      .click();
    await expect
      .poll(() =>
        page.evaluate(() => localStorage.getItem("openseal.home-submission")),
      )
      .toBeNull();
    expect(writes).toHaveLength(2);
    expect(writes[1]).toEqual(writes[0]);
    if (mode === "work")
      await expect(page.locator(".inspector")).toContainText(prompt);
    else {
      expect(
        await page.evaluate(() =>
          localStorage.getItem("openseal.builder-conversation"),
        ),
      ).toBe("saved-conversation");
      await expect(
        page.getByRole("heading", { name: "Builder chat", exact: true }),
      ).toBeVisible();
    }
    await page.reload();
    await expect(page.locator("#prompt")).toHaveValue("");
    expect(writes).toHaveLength(2);
  });
}

test("storage failure sends nothing and keeps the editable draft", async ({
  page,
}) => {
  let writes = 0;
  await page.route("**/api/v1/agent-builder/conversations", (r) => {
    writes++;
    return r.abort();
  });
  await setup(page);
  await page.evaluate(() => {
    const original = Storage.prototype.setItem;
    Storage.prototype.setItem = function (key, value) {
      if (key === "openseal.home-submission") throw new Error("Storage full");
      original.call(this, key, value);
    };
  });
  await page.locator("#prompt").fill("Preserve my unsent agent request.");
  await page.locator(".composer button[type=submit]").click();
  await expect(
    page.getByRole("region", { name: "Saved request recovery" }),
  ).toContainText("Nothing was sent");
  await expect(page.locator("#prompt")).toBeEditable();
  expect(writes).toBe(0);
});

test("mismatched response and failed local cleanup preserve the original retry identity", async ({
  page,
}) => {
  let writes = 0;
  let key = "";
  let valid = false;
  await page.route("**/api/v1/agent-builder/conversations", (r) => {
    writes++;
    key ||= r.request().headers()["idempotency-key"];
    expect(r.request().headers()["idempotency-key"]).toBe(key);
    const body = r.request().postDataJSON();
    return r.fulfill({
      json: builderStart(
        "saved",
        body.prompt,
        body.parameters.kind,
        valid ? scope : { kind: "local", id: "foreign" },
      ),
    });
  });
  await builderChat(page, "saved");
  await setup(page);
  await page.locator("#prompt").fill("Check response identity.");
  await page.locator(".composer button[type=submit]").click();
  await expect(
    page.getByRole("region", { name: "Saved request recovery" }),
  ).toContainText("does not match");
  valid = true;
  await page.evaluate(() => {
    const remove = Storage.prototype.removeItem;
    Storage.prototype.removeItem = function (key) {
      if (key === "openseal.home-submission")
        throw new Error("Cleanup blocked");
      remove.call(this, key);
    };
  });
  await page.getByRole("button", { name: "Retry saved request" }).click();
  await expect(
    page.getByRole("region", { name: "Saved request recovery" }),
  ).toContainText(
    "Saved in the workspace, but local recovery could not be cleared",
  );
  await page.reload();
  await page.getByRole("button", { name: "Retry saved request" }).click();
  await expect
    .poll(() =>
      page.evaluate(() => localStorage.getItem("openseal.home-submission")),
    )
    .toBeNull();
  expect(writes).toBe(3);
});

test("unreadable recovery blocks new requests until explicitly forgotten without canceling work", async ({
  page,
}) => {
  let writes = 0;
  await page.route("**/api/v1/agent-builder/conversations", (r) => {
    writes++;
    return r.abort();
  });
  await page.addInitScript(() => {
    localStorage.setItem("openseal.home-submission", "{broken");
    localStorage.setItem(
      "openseal.draft",
      JSON.stringify({
        version: 1,
        prompt: "Keep this text.",
        mode: "agent",
        agentID: "",
      }),
    );
  });
  await setup(page);
  await expect(page.locator(".composer button[type=submit]")).toBeDisabled();
  await expect(
    page.getByRole("region", { name: "Saved request recovery" }),
  ).toContainText("could not be read");
  await page.getByRole("button", { name: "Forget saved request…" }).click();
  await expect(
    page.getByRole("button", {
      name: "Forget local retry record",
      exact: true,
    }),
  ).toBeFocused();
  await expect(
    page.getByRole("region", { name: "Saved request recovery" }),
  ).toContainText("does not cancel");
  await page.getByRole("button", { name: "Keep retry record" }).click();
  await expect(
    page.getByRole("button", { name: "Forget saved request…" }),
  ).toBeFocused();
  await page.getByRole("button", { name: "Forget saved request…" }).click();
  await page
    .getByRole("button", { name: "Forget local retry record", exact: true })
    .click();
  await expect(page.locator("#prompt")).toBeFocused();
  await expect(page.locator("#prompt")).toHaveValue("Keep this text.");
  await expect(page.locator(".composer button[type=submit]")).toBeEnabled();
  expect(writes).toBe(0);
});

test("a delayed successful task submission does not navigate away from settings", async ({
  page,
}) => {
  let release!: () => void;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route("**/api/v1/agent-runs", async (r) => {
    await gate;
    const body = r.request().postDataJSON();
    return r.fulfill({
      json: { run: { id: "late", ...body, status: "queued", revision: 1 } },
    });
  });
  await setup(page);
  await page
    .getByRole("group", { name: "What to create" })
    .getByRole("button", { name: "Start work", exact: true })
    .click();
  await page.getByLabel("Assign to").selectOption("analyst");
  await page.locator("#prompt").fill("Finish without moving my view.");
  await page.locator(".composer button[type=submit]").click();
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  release();
  await expect
    .poll(() =>
      page.evaluate(() => localStorage.getItem("openseal.home-submission")),
    )
    .toBeNull();
  await expect(
    page.getByRole("heading", { name: "Settings", exact: true }),
  ).toBeVisible();
  await expect(page.locator(".inspector")).toHaveCount(0);
});
