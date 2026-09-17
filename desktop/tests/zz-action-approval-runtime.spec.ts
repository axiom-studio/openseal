import { test, expect } from "@playwright/test";
import { execFileSync } from "node:child_process";
import { join } from "node:path";

test("real daemon records a desktop decision on an exact synthetic checkpoint", async ({
  page,
  request,
}) => {
  test.setTimeout(60000);
  execFileSync(
    "go",
    [
      "run",
      "tests/seed_approval.go",
      "-db",
      join(process.env.OPENSEAL_UI_TEST_WORKSPACE!, "data/openseal.db"),
    ],
    { timeout: 30000 },
  );
  await page.goto("/");
  await page
    .getByRole("navigation")
    .getByRole("button", { name: /Reviews/ })
    .click();
  await page.getByRole("button", { name: /Review a synthetic action/ }).click();
  const panel = page.getByRole("region", { name: "Action approval" });
  await expect(
    panel.getByText("Review a synthetic action", { exact: true }),
  ).toBeVisible();
  await panel.getByRole("checkbox").check();
  await panel
    .getByRole("button", { name: "Approve action", exact: true })
    .click();
  await panel
    .getByRole("button", { name: "Confirm: approve action", exact: true })
    .click();
  await expect(
    panel.getByText(/Action approved. Execution remains/),
  ).toBeFocused();
  const recorded = await (
    await request.get(
      "/api/v1/action-approvals/ui-approval?scopeKind=local&scopeId=default",
    )
  ).json();
  expect(recorded.status).toBe("approved");
  expect(recorded.revision).toBe(2);
  expect(recorded.decisionBy).toEqual({ type: "user", id: "local-operator" });
  expect(recorded.actionCallId).toBe("ui-action");
  await page
    .getByRole("button", { name: "Close details", exact: true })
    .click();
  await page
    .getByRole("combobox", { name: "Review status" })
    .selectOption("approved");
  await page.getByRole("button", { name: /Review a synthetic action/ }).click();
  await expect(
    panel.getByText("Reviewed by local-operator.", { exact: true }),
  ).toBeVisible();
  await expect(
    panel.getByRole("button", { name: "Approve action", exact: true }),
  ).toHaveCount(0);
  await page.reload();
  await page
    .getByRole("navigation")
    .getByRole("button", { name: /Reviews/ })
    .click();
  await page
    .getByRole("combobox", { name: "Review status" })
    .selectOption("approved");
  await page.getByRole("button", { name: /Review a synthetic action/ }).click();
  await expect(
    panel.getByText("Reviewed by local-operator.", { exact: true }),
  ).toBeVisible();
});
