import { test, expect, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";

async function fixture(page: Page, resolve = true, eligible = true) {
  const run = {
    id: "review-run",
    kind: "agent_work",
    goal: "Review a proposed publication",
    status: "waiting_for_approval",
    revision: 2,
    wakeCondition: { type: "approval", reference: "approval-1" },
    owner: { type: "agent", id: "analyst" },
  };
  const approval = {
    id: "approval-1",
    runId: run.id,
    actionCallId: "action-1",
    revision: 1,
    status: "pending",
    risk: "production",
    summary: "Publish the reviewed announcement",
    policyReason: "Public publication requires review",
    eligibleApprovers: [
      {
        type: eligible ? "role" : "user",
        id: eligible ? "operator" : "someone-else",
      },
    ],
    expiresAt: new Date(Date.now() + 3600000).toISOString(),
  };
  const call = {
    id: "action-1",
    runId: run.id,
    approvalId: approval.id,
    revision: 1,
    status: "waiting_for_approval",
    skillId: "publishing",
    skillVersion: "1",
    action: "publish",
    sideEffect: "external",
    arguments: {
      destination: "Example public channel",
      text: "Draft announcement for review",
    },
  };
  await page.route("**/api/v1/capabilities", (route) =>
    route.fulfill({
      json: {
        capabilities: [
          { id: "agent-runs", available: true, operations: ["list", "get"] },
          {
            id: "action-approvals",
            available: true,
            operations: resolve ? ["get", "resolve"] : ["get"],
          },
          { id: "action-calls", available: true, operations: ["get"] },
        ],
      },
    }),
  );
  await page.route("**/api/v1/agent-runs?*", (route) =>
    route.fulfill({ json: [run] }),
  );
  await page.route("**/api/v1/action-approvals/approval-1?*", (route) =>
    route.fulfill({ json: approval }),
  );
  await page.route("**/api/v1/action-calls/action-1?*", (route) =>
    route.fulfill({ json: call }),
  );
  await page.goto("/");
  await page
    .getByRole("button", { name: /Review a proposed publication/ })
    .click();
  const panel = page.getByRole("region", { name: "Action approval" });
  await expect(
    panel.getByText("Publish the reviewed announcement", { exact: true }),
  ).toBeVisible();
  return { run, approval, call, panel };
}

test("approval shows exact inputs and requires reviewed confirmation before a decision", async ({
  page,
}) => {
  const { run, approval, call, panel } = await fixture(page);
  let sent: any;
  await page.route(
    "**/api/v1/action-approvals/approval-1/decisions?*",
    (route) => {
      sent = route.request().postDataJSON();
      expect(route.request().headers()["idempotency-key"]).toBe(
        sent.decisionId,
      );
      return route.fulfill({
        json: {
          approval: {
            ...approval,
            status: "approved",
            revision: 2,
            decisionBy: sent.principal,
          },
          call: { ...call, status: "ready", revision: 2 },
          run: {
            ...run,
            status: "waiting_for_dependency",
            wakeCondition: { type: "action", reference: call.id },
            revision: 3,
          },
        },
      });
    },
  );
  await expect(
    panel.getByRole("button", { name: "Approve action", exact: true }),
  ).toBeDisabled();
  await expect(panel.locator("pre")).toContainText("Example public channel");
  await panel.getByRole("checkbox").check();
  await panel
    .getByRole("button", { name: "Approve action", exact: true })
    .click();
  const confirm = panel.getByRole("button", {
    name: "Confirm: approve action",
    exact: true,
  });
  await expect(confirm).toBeFocused();
  await expect(confirm).toHaveAccessibleDescription(/permits the exact action/);
  expect(sent).toBeUndefined();
  await page.screenshot({
    path: "../.impeccable/review/action-approval-desktop.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await confirm.scrollIntoViewIfNeeded();
  await page.screenshot({
    path: "../.impeccable/review/action-approval-mobile.png",
  });
  expect(
    (
      await new AxeBuilder({ page })
        .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
        .analyze()
    ).violations,
  ).toEqual([]);
  await confirm.click();
  await expect(
    panel.getByText(/Action approved. Execution remains/),
  ).toBeFocused();
  expect(sent).toMatchObject({
    expectedRevision: 1,
    decision: "approve",
    principal: { type: "user", id: "local-operator" },
  });
  await expect(
    panel.getByRole("button", { name: "Approve action", exact: true }),
  ).toHaveCount(0);
});

for (const decision of ["reject", "request_changes"] as const)
  test(`${decision} records reviewer intent and requires guidance for changes`, async ({
    page,
  }) => {
    const { run, approval, call, panel } = await fixture(page);
    let sent: any;
    await page.route(
      "**/api/v1/action-approvals/approval-1/decisions?*",
      (route) => {
        sent = route.request().postDataJSON();
        return route.fulfill({
          json: {
            approval: {
              ...approval,
              revision: 2,
              status: decision === "reject" ? "rejected" : "changes_requested",
              decisionReason: sent.reason,
            },
            call: { ...call, status: "denied", revision: 2 },
            run: { ...run, status: "queued", wakeCondition: null, revision: 3 },
          },
        });
      },
    );
    await panel.getByRole("checkbox").check();
    await panel
      .getByRole("button", {
        name: decision === "reject" ? "Reject action" : "Request changes",
        exact: true,
      })
      .click();
    const confirm = panel.getByRole("button", { name: /^Confirm:/ });
    if (decision === "request_changes") {
      await expect(confirm).toBeDisabled();
      await expect(
        panel.getByRole("textbox", { name: /Reviewer guidance/ }),
      ).toBeFocused();
    }
    await panel
      .getByRole("textbox", { name: /Reviewer guidance/ })
      .fill("Use the internal review channel first.");
    await confirm.click();
    await expect(panel.getByRole("status")).toBeFocused();
    expect(sent).toMatchObject({
      decision,
      reason: "Use the internal review channel first.",
      expectedRevision: 1,
    });
  });

test("a revision conflict refreshes exact approval details and requires another review", async ({
  page,
}) => {
  const { approval, panel } = await fixture(page);
  let requests = 0;
  await page.route(
    "**/api/v1/action-approvals/approval-1/decisions?*",
    (route) => {
      requests++;
      approval.revision = 2;
      approval.summary = "Publish revised destination";
      return route.fulfill({
        status: 409,
        json: { error: "revision conflict" },
      });
    },
  );
  await panel.getByRole("checkbox").check();
  await panel
    .getByRole("button", { name: "Approve action", exact: true })
    .click();
  await panel
    .getByRole("button", { name: "Confirm: approve action", exact: true })
    .click();
  await expect(panel.getByRole("alert")).toContainText("This approval changed");
  await panel
    .getByRole("button", { name: "Refresh approval", exact: true })
    .click();
  await expect(
    panel.getByText("Publish revised destination", { exact: true }),
  ).toBeVisible();
  await expect(panel.getByRole("checkbox")).not.toBeChecked();
  await expect(
    panel.getByRole("button", { name: "Approve action", exact: true }),
  ).toBeDisabled();
  expect(requests).toBe(1);
});

test("uncertain delivery retains its decision key when retried after checking state", async ({
  page,
}) => {
  const { run, approval, call, panel } = await fixture(page);
  const keys: string[] = [];
  await page.route(
    "**/api/v1/action-approvals/approval-1/decisions?*",
    (route) => {
      keys.push(route.request().postDataJSON().decisionId);
      if (keys.length === 1) return route.abort("failed");
      return route.fulfill({
        json: {
          approval: { ...approval, status: "approved", revision: 2 },
          call: { ...call, status: "ready" },
          run: {
            ...run,
            status: "waiting_for_dependency",
            wakeCondition: { type: "action", reference: call.id },
            revision: 3,
          },
        },
      });
    },
  );
  await panel.getByRole("checkbox").check();
  await panel
    .getByRole("button", { name: "Approve action", exact: true })
    .click();
  await panel
    .getByRole("button", { name: "Confirm: approve action", exact: true })
    .click();
  await expect(panel.getByRole("alert")).toContainText(
    "Could not confirm the decision",
  );
  await expect(
    panel.getByRole("button", { name: "Confirm: approve action", exact: true }),
  ).toBeDisabled();

  await panel
    .getByRole("button", { name: "Refresh approval", exact: true })
    .click();
  await expect(panel.getByRole("status")).toBeFocused();
  await panel
    .getByRole("button", { name: "Confirm: approve action", exact: true })
    .click();
  await expect(
    panel.getByText(/Action approved. Execution remains/),
  ).toBeVisible();
  expect(keys).toHaveLength(2);
  expect(keys[0]).toBe(keys[1]);
});

test("ineligible reviewers can inspect the action without decision controls", async ({
  page,
}) => {
  const { panel } = await fixture(page, true, false);
  await expect(panel.getByText(/local operator is not eligible/)).toBeVisible();
  await expect(
    panel.getByRole("button", { name: "Approve action", exact: true }),
  ).toHaveCount(0);
  await expect(panel.locator("pre")).toContainText(
    "Draft announcement for review",
  );
});

test("expired checkpoints remain inspectable without allowing a decision", async ({
  page,
}) => {
  const { approval } = await fixture(page);
  approval.expiresAt = new Date(Date.now() - 60000).toISOString();
  await page.reload();
  await page
    .getByRole("button", { name: /Review a proposed publication/ })
    .click();
  const panel = page.getByRole("region", { name: "Action approval" });
  await expect(panel.getByText(/review deadline has passed/)).toBeVisible();
  await expect(
    panel.getByRole("button", { name: "Approve action", exact: true }),
  ).toHaveCount(0);
});

test("read-only approval capabilities never expose decision controls", async ({
  page,
}) => {
  const { panel } = await fixture(page, false);
  await expect(
    panel.getByText(/does not allow approval decisions/),
  ).toBeVisible();
  await expect(panel.getByRole("checkbox")).toHaveCount(0);
});

test("a late approval response cannot replace another open task", async ({
  page,
}) => {
  const { run, approval, call, panel } = await fixture(page);
  let release!: () => void;
  let started!: () => void;
  const began = new Promise<void>((resolve) => {
    started = resolve;
  });
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  const other = {
    ...run,
    id: "other-run",
    goal: "Keep this other task open",
    status: "completed",
    wakeCondition: null,
  };
  await page.route("**/api/v1/agent-runs?*", (route) =>
    route.fulfill({ json: [run, other] }),
  );
  await page.route(
    "**/api/v1/action-approvals/approval-1/decisions?*",
    async (route) => {
      started();
      await held;
      return route.fulfill({
        json: {
          approval: { ...approval, status: "approved", revision: 2 },
          call: { ...call, status: "ready" },
          run: {
            ...run,
            status: "waiting_for_dependency",
            revision: 3,
            wakeCondition: { type: "action", reference: call.id },
          },
        },
      });
    },
  );
  await page
    .getByRole("button", { name: "Refresh workspace", exact: true })
    .click();
  await panel.getByRole("checkbox").check();
  await panel
    .getByRole("button", { name: "Approve action", exact: true })
    .click();
  await panel
    .getByRole("button", { name: "Confirm: approve action", exact: true })
    .click();
  await began;
  await page.getByRole("button", { name: /Keep this other task open/ }).click();
  const response = page.waitForResponse((response) =>
    response.url().includes("/approval-1/decisions?"),
  );
  release();
  await response;
  await expect(
    page
      .getByRole("complementary", { name: "Work details" })
      .getByRole("heading", { name: "Keep this other task open" }),
  ).toBeVisible();
  await expect(
    page.getByRole("region", { name: "Action approval" }),
  ).toHaveCount(0);
});
