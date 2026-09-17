import { test, expect, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
const initial = {
  id: "guided-run",
  goal: "Review research guidance",
  status: "paused",
  revision: 2,
  pendingInterventions: [] as any[],
};
async function setup(page: Page, get: () => typeof initial) {
  await page.route("**/api/v1/capabilities", (r) =>
    r.fulfill({
      json: {
        capabilities: [
          {
            id: "agent-runs",
            available: true,
            operations: ["list", "get", "intervene"],
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/agent-runs?*", (r) =>
    r.fulfill({ json: [get()] }),
  );
  await page.route("**/api/v1/agent-runs/guided-run?*", (r) =>
    r.fulfill({ json: get() }),
  );
  await page.goto("/");
  await page.getByRole("button", { name: /Review research guidance/ }).click();
  await page.getByRole("button", { name: "Add guidance" }).click();
}
function saved(run: typeof initial, body: any) {
  return {
    ...run,
    revision: run.revision + 1,
    pendingInterventions: [
      ...run.pendingInterventions,
      {
        id: body.interventionId,
        instruction: body.instruction,
        actor: body.actor,
        createdAt: "2026-09-17T12:00:00Z",
      },
    ],
  };
}

test("guidance preserves paused state, focuses feedback and keeps drafts across detail changes", async ({
  page,
}) => {
  let run = { ...initial };
  let commands = 0;
  await page.route("**/api/v1/agent-runs/guided-run/commands?*", (r) => {
    const body = r.request().postDataJSON();
    commands++;
    expect(body.kind).toBe("intervene");
    expect(body.expectedRevision).toBe(run.revision);
    expect(body.interventionId).toMatch(/^[0-9a-f-]{36}$/);
    run = saved(run, body);
    return r.fulfill({ json: { run } });
  });
  await setup(page, () => run);
  const input = page.getByLabel("Guidance for this task");
  await expect(input).toBeFocused();
  await expect(
    page.getByText(
      "Guidance is saved while work stays paused. Resume when you are ready.",
    ),
  ).toBeVisible();
  await input.fill("Separate evidence from assumptions.");
  await page
    .getByRole("button", { name: "Close details", exact: true })
    .click();
  await page.getByRole("button", { name: /Review research guidance/ }).click();
  await expect(input).toHaveValue("Separate evidence from assumptions.");
  await page.locator(".work-guidance").scrollIntoViewIfNeeded();
  await page.screenshot({
    path: "../.impeccable/review/work-guidance-form-desktop.png",
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.locator(".work-guidance").scrollIntoViewIfNeeded();
  await page.screenshot({
    path: "../.impeccable/review/work-guidance-form-mobile.png",
  });
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page
    .getByRole("button", { name: "Save guidance", exact: true })
    .click();
  await expect(page.getByText(/Guidance saved in this task/)).toBeFocused();
  expect(commands).toBe(1);
  expect(run.status).toBe("paused");
  await page.getByText("Saved guidance (1)").click();
  await expect(page.locator(".guidance-history")).toContainText(
    "Separate evidence from assumptions.",
  );
  await page.locator(".work-guidance").scrollIntoViewIfNeeded();
  await page.screenshot({
    path: "../.impeccable/review/work-guidance-desktop.png",
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.locator(".work-guidance").scrollIntoViewIfNeeded();
  await page.screenshot({
    path: "../.impeccable/review/work-guidance-mobile.png",
  });
  expect(
    (
      await new AxeBuilder({ page })
        .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
        .analyze()
    ).violations,
  ).toEqual([]);
});

test("an uncertain guidance submission retains its identity across reload and retries only on request", async ({
  page,
}) => {
  let run = { ...initial };
  const ids: string[] = [];
  await page.route("**/api/v1/agent-runs/guided-run/commands?*", async (r) => {
    const body = r.request().postDataJSON();
    ids.push(body.interventionId);
    if (ids.length === 1) {
      await r.abort("failed");
      return;
    }
    run = saved(run, body);
    await r.fulfill({ json: { run } });
  });
  await setup(page, () => run);
  await page
    .getByLabel("Guidance for this task")
    .fill("State confidence explicitly.");
  await page
    .getByRole("button", { name: "Save guidance", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "Retry guidance" }),
  ).toBeVisible();
  await expect(page.getByLabel("Guidance for this task")).toHaveAttribute(
    "readonly",
    "",
  );
  await page.reload();
  await page.getByRole("button", { name: /Review research guidance/ }).click();
  await expect(
    page.getByRole("button", { name: "Retry guidance" }),
  ).toBeVisible();
  expect(ids).toHaveLength(1);
  await page.getByRole("button", { name: "Check saved guidance" }).click();
  await expect(
    page.getByText(/This instruction is not recorded yet/),
  ).toBeFocused();
  await page.getByRole("button", { name: "Retry guidance" }).click();
  await expect(page.getByText(/Guidance saved in this task/)).toBeFocused();
  expect(ids[1]).toBe(ids[0]);
});

test("revision conflicts preserve guidance and require review instead of automatic resubmission", async ({
  page,
}) => {
  let run = { ...initial };
  let calls = 0;
  await page.route("**/api/v1/agent-runs/guided-run/commands?*", (r) => {
    calls++;
    run = { ...run, revision: 5 };
    return r.fulfill({ status: 409, json: { error: "Run changed" } });
  });
  await setup(page, () => run);
  await page
    .getByLabel("Guidance for this task")
    .fill("Use only supplied evidence.");
  await page
    .getByRole("button", { name: "Save guidance", exact: true })
    .click();
  await expect(
    page.getByText(/This task changed. Your draft is preserved/),
  ).toBeFocused();
  await expect(page.getByLabel("Guidance for this task")).toHaveValue(
    "Use only supplied evidence.",
  );
  expect(calls).toBe(1);
});

test("definitive rejection and unavailable guidance explain recoverable drafts accurately", async ({
  page,
}) => {
  let run = { ...initial };
  await page.route("**/api/v1/agent-runs/guided-run/commands?*", (r) =>
    r.fulfill({
      status: 400,
      json: { error: "Guidance is not accepted by this host." },
    }),
  );
  await setup(page, () => run);
  await page.getByLabel("Guidance for this task").fill("Preserve uncertainty.");
  await page
    .getByRole("button", { name: "Save guidance", exact: true })
    .click();
  await expect(
    page.getByText(
      "Guidance was not saved. Guidance is not accepted by this host.",
    ),
  ).toBeFocused();
  await expect(
    page.getByRole("button", { name: "Check saved guidance" }),
  ).toHaveCount(0);
  await expect(page.getByLabel("Guidance for this task")).toHaveValue(
    "Preserve uncertainty.",
  );
  await page.locator(".work-guidance").scrollIntoViewIfNeeded();
  await page.screenshot({
    path: "../.impeccable/review/work-guidance-rejection.png",
  });
  await page.route("**/api/v1/capabilities", (r) =>
    r.fulfill({
      json: {
        capabilities: [
          { id: "agent-runs", available: true, operations: ["list", "get"] },
        ],
      },
    }),
  );
  await page.reload();
  await page.getByRole("button", { name: /Review research guidance/ }).click();
  await expect(
    page.getByText(
      "Adding guidance is unavailable in this workspace. Your draft is kept here for you to copy.",
    ),
  ).toBeVisible();
  await expect(page.getByLabel("Guidance for this task")).toHaveAttribute(
    "readonly",
    "",
  );
  await expect(
    page.getByRole("button", { name: "Save guidance", exact: true }),
  ).toHaveCount(0);
  await page.locator(".work-guidance").scrollIntoViewIfNeeded();
  await page.screenshot({
    path: "../.impeccable/review/work-guidance-unavailable.png",
  });
});
