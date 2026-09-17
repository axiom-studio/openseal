import { test, expect, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";

const run = {
  id: "linked-work",
  goal: "Prepare the evidence review",
  status: "paused",
  revision: 3,
  owner: { type: "team", id: "evidence" },
  wakeCondition: { type: "approval", reference: "newer-approval" },
};
const request = {
  id: "linked-question",
  sourceRunId: run.id,
  goal: "Confirm the reporting period",
  kind: "request",
  status: "clarification_requested",
  revision: 2,
  requester: { type: "team", id: "evidence" },
  recipient: { type: "agent", id: "analyst" },
  clarification: "Should I include July or the entire quarter?",
};
const approval = {
  id: "linked-approval",
  runId: run.id,
  actionCallId: "linked-action",
  revision: 2,
  status: "approved",
  risk: "production",
  summary: "Publish the reviewed announcement",
  policyReason: "Public publication requires review",
  eligibleApprovers: [{ type: "role", id: "operator" }],
  decisionBy: { type: "user", id: "local-operator" },
};
const action = {
  id: "linked-action",
  runId: run.id,
  approvalId: approval.id,
  revision: 2,
  status: "completed",
  skillId: "publishing",
  skillVersion: "1",
  action: "publish",
  sideEffect: "external",
  arguments: {
    destination: "Example public channel",
    text: "Reviewed announcement",
  },
};
async function setup(page: Page) {
  const writes: string[] = [];
  page.on("request", (r) => {
    if (r.method() !== "GET") writes.push(r.url());
  });
  await page.route("**/api/v1/capabilities", (r) =>
    r.fulfill({
      json: {
        capabilities: [
          { id: "team-definitions", available: true, operations: ["list"] },
          {
            id: "channels",
            available: true,
            operations: ["list", "get", "read", "post"],
          },
          { id: "agent-runs", available: true, operations: ["get"] },
          { id: "agent-requests", available: true, operations: ["get"] },
          {
            id: "action-approvals",
            available: true,
            operations: ["get", "resolve"],
          },
          { id: "action-calls", available: true, operations: ["get"] },
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
              id: "evidence",
              activeVersion: "1",
              status: "active",
              revision: 1,
              roster: [],
            },
            definition: {
              displayName: "Evidence team",
              purpose: "Review evidence together.",
              roles: [],
            },
          },
        ],
      },
    }),
  );
  const channel = {
    id: "notes",
    title: "Review notes",
    owner: run.owner,
    status: "active",
    revision: 1,
    lastSequence: 1,
  };
  await page.route("**/api/v1/conversations?*", (r) =>
    r.fulfill({ json: [channel] }),
  );
  await page.route("**/api/v1/conversations/notes?*", (r) =>
    r.fulfill({ json: channel }),
  );
  await page.route("**/api/v1/conversations/notes/messages?*", (r) =>
    r.fulfill({
      json: [
        {
          id: "note",
          conversationId: channel.id,
          sequence: 1,
          sender: { type: "agent", id: "analyst" },
          intent: "update",
          content:
            "Please check the reporting question and the saved publication review.",
          audience: { kind: "channel" },
          createdAt: "2026-09-17T10:00:00Z",
          references: [
            { kind: "agent_request", id: request.id },
            { kind: "approval", id: approval.id },
          ],
        },
      ],
    }),
  );
  await page.route("**/api/v1/agent-runs/linked-work?*", (r) =>
    r.fulfill({ json: run }),
  );
  await page.route("**/api/v1/agent-requests/linked-question?*", (r) =>
    r.fulfill({ json: request }),
  );
  await page.route("**/api/v1/action-approvals/linked-approval?*", (r) =>
    r.fulfill({ json: approval }),
  );
  await page.route("**/api/v1/action-calls/linked-action?*", (r) =>
    r.fulfill({ json: action }),
  );
  await page.goto("/");
  await expect(
    page.getByText("Connected locally", { exact: true }),
  ).toBeVisible();
  await page.keyboard.press("Control+5");
  await page.getByRole("button", { name: /Evidence team.*active/ }).click();
  await page.getByRole("button", { name: "Channels", exact: true }).click();
  await page.getByRole("button", { name: "Review notes", exact: true }).click();
  return writes;
}

test("channel question opens its exact request with get-only access and restores the draft and focus", async ({
  page,
}) => {
  const writes = await setup(page);
  let listReads = 0;
  await page.route("**/api/v1/agent-requests?*", (r) => {
    listReads++;
    return r.fulfill({ json: [] });
  });
  const draft = page.locator(".channel-composer textarea");
  await draft.fill("Keep my reply while I inspect the question.");
  await page
    .getByRole("button", {
      name: "Agent request · linked-question",
      exact: true,
    })
    .click();
  const detail = page.getByRole("region", { name: "Reference details" });
  await expect(detail).toContainText(request.clarification);
  await detail.getByRole("button", { name: "Open request details" }).click();
  await expect(page.locator(".inspector .work-collaboration")).toContainText(
    request.clarification,
  );
  await expect(
    page.getByRole("button", { name: "Show all requests for this task" }),
  ).toHaveCount(0);
  await page.locator(".inspector button[title]").click();
  await expect(
    detail.getByRole("button", { name: "Open request details" }),
  ).toBeFocused();
  await expect(draft).toHaveValue(
    "Keep my reply while I inspect the question.",
  );
  expect(listReads).toBe(0);
  expect(writes).toEqual([]);
});

test("channel approval preserves the exact historical review instead of the run's newer checkpoint", async ({
  page,
}) => {
  const writes = await setup(page);
  await page
    .getByRole("button", { name: "Approval · linked-approval", exact: true })
    .click();
  const detail = page.getByRole("region", { name: "Reference details" });
  await expect(detail).toContainText(approval.summary);
  await expect(detail).toContainText("approved");
  await expect(detail).toContainText("the recorded outcome");
  await detail.scrollIntoViewIfNeeded();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: "../.impeccable/review/channel-decision-links-desktop.png",
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await detail.scrollIntoViewIfNeeded();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: "../.impeccable/review/channel-decision-links-mobile.png",
  });
  await page.setViewportSize({ width: 1440, height: 1000 });
  await detail.getByRole("button", { name: "Open approval review" }).click();
  const review = page.getByRole("region", { name: "Action approval" });
  await expect(review).toContainText("Reviewed by local-operator.");
  await expect(review.locator("pre")).toContainText(
    action.arguments.destination,
  );
  await expect(
    review.getByRole("button", { name: "Approve action", exact: true }),
  ).toHaveCount(0);
  await page.locator(".inspector button[title]").click();
  await expect(
    detail.getByRole("button", { name: "Open approval review" }),
  ).toBeFocused();
  expect(writes).toEqual([]);
});

test("mismatched approval bindings block opening and closed late requests cannot open work", async ({
  page,
}) => {
  const writes = await setup(page);
  await page.route("**/api/v1/action-calls/linked-action?*", (r) =>
    r.fulfill({ json: { ...action, approvalId: "different-approval" } }),
  );
  await page
    .getByRole("button", { name: "Approval · linked-approval", exact: true })
    .click();
  const detail = page.getByRole("region", { name: "Reference details" });
  await expect(detail.getByRole("alert")).toContainText(
    "does not match its action and work",
  );
  await expect(
    detail.getByRole("button", { name: "Open approval review" }),
  ).toHaveCount(0);
  await page.route("**/api/v1/action-calls/linked-action?*", (r) =>
    r.fulfill({ json: action }),
  );
  await detail.getByRole("button", { name: "Retry opening approval" }).click();
  await expect(
    detail.getByRole("button", { name: "Open approval review" }),
  ).toBeVisible();
  await detail.getByRole("button", { name: "Close reference" }).click();
  let release!: () => void;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  let started!: () => void;
  const requested = new Promise<void>((resolve) => {
    started = resolve;
  });
  let runReads = 0;
  await page.route("**/api/v1/agent-runs/linked-work?*", (r) => {
    runReads++;
    return r.fulfill({ json: run });
  });
  await page.route("**/api/v1/agent-requests/linked-question?*", async (r) => {
    started();
    await held;
    await r.fulfill({ json: request });
  });
  await page
    .getByRole("button", {
      name: "Agent request · linked-question",
      exact: true,
    })
    .click();
  await requested;
  await detail.getByRole("button", { name: "Close reference" }).click();
  const response = page.waitForResponse((r) =>
    r.url().includes("/agent-requests/linked-question"),
  );
  release();
  await response;
  await page.evaluate(
    () =>
      new Promise((resolve) =>
        requestAnimationFrame(() => requestAnimationFrame(resolve)),
      ),
  );
  await expect(detail).toHaveCount(0);
  await expect(page.locator(".inspector")).toHaveCount(0);
  expect(runReads).toBe(0);
  expect(writes).toEqual([]);
});

test("a linked pending approval still requires input review and explicit decision confirmation", async ({
  page,
}) => {
  const writes = await setup(page);
  await page.route("**/api/v1/agent-runs/linked-work?*", (r) =>
    r.fulfill({
      json: {
        ...run,
        status: "waiting_for_approval",
        wakeCondition: { type: "approval", reference: approval.id },
      },
    }),
  );
  await page.route("**/api/v1/action-approvals/linked-approval?*", (r) =>
    r.fulfill({
      json: {
        ...approval,
        status: "pending",
        decisionBy: undefined,
        expiresAt: new Date(Date.now() + 3600000).toISOString(),
      },
    }),
  );
  await page.route("**/api/v1/action-calls/linked-action?*", (r) =>
    r.fulfill({ json: { ...action, status: "waiting_for_approval" } }),
  );
  await page
    .getByRole("button", { name: "Approval · linked-approval", exact: true })
    .click();
  await expect(
    page.getByRole("region", { name: "Reference details" }),
  ).toContainText("before deciding");
  await page.getByRole("button", { name: "Open approval review" }).click();
  const review = page.getByRole("region", { name: "Action approval" });
  const approve = review.getByRole("button", {
    name: "Approve action",
    exact: true,
  });
  await expect(approve).toBeDisabled();
  await expect(review.locator("pre")).toContainText(action.arguments.text);
  await review.getByRole("checkbox").check();
  await approve.click();
  await expect(
    review.getByRole("button", {
      name: "Confirm: approve action",
      exact: true,
    }),
  ).toBeFocused();
  expect(writes).toEqual([]);
});
