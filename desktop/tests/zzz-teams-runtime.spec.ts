import { test, expect } from "@playwright/test";
import { execFileSync } from "node:child_process";
import { join } from "node:path";

test("real daemon exposes only workspace teams and restores inspected content after reload", async ({
  page,
}) => {
  test.setTimeout(60000);
  execFileSync(
    "go",
    [
      "run",
      "tests/seed_team.go",
      "-db",
      join(process.env.OPENSEAL_UI_TEST_WORKSPACE!, "data/openseal.db"),
    ],
    { timeout: 30000 },
  );
  await page.goto("/");
  await expect(
    page.getByText("Connected locally", { exact: true }),
  ).toBeVisible();
  await page.keyboard.press("Control+5");
  const row = page.getByRole("button", {
    name: /Synthetic review team.*draft/,
  });
  await expect(row).toHaveCount(1);
  await row.click();
  await expect(page.getByText("Report uncertainty clearly.")).toBeVisible();
  await page.getByRole("button", { name: "Channels", exact: true }).click();
  await page.getByRole("button", { name: "New channel", exact: true }).click();
  await page
    .getByLabel("Channel name", { exact: true })
    .fill("Durable evidence notes");
  await page
    .getByRole("button", { name: "Create channel", exact: true })
    .click();
  const thread = page.getByRole("region", {
    name: "Channel conversation",
    exact: true,
  });
  await expect(
    thread.getByRole("heading", { name: "Durable evidence notes" }),
  ).toBeVisible();
  await page
    .getByLabel("Message to this channel", { exact: true })
    .fill("Use July evidence and preserve uncertainty.");
  await page.getByRole("button", { name: "Post message", exact: true }).click();
  await expect(
    thread.getByText("Use July evidence and preserve uncertainty.", {
      exact: true,
    }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Reply to message 1", exact: true })
    .click();
  await page
    .getByLabel("Your reply", { exact: true })
    .fill("Keep this context for the next review.");
  await page.getByRole("button", { name: "Post message", exact: true }).click();
  await expect(
    thread.getByText("Keep this context for the next review.", { exact: true }),
  ).toBeVisible();
  await page.reload();
  await expect(
    page.getByText("Connected locally", { exact: true }),
  ).toBeVisible();
  await page.keyboard.press("Control+5");
  await row.click();
  await expect(
    page.getByText("Version 1 · 0 members · Revision 1"),
  ).toBeVisible();
  await page.getByRole("button", { name: "Channels", exact: true }).click();
  await page
    .getByRole("button", {
      name: "Durable evidence notes · 2 unread",
      exact: true,
    })
    .click();
  await expect(
    page.getByText("Keep this context for the next review.", { exact: true }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Original message", exact: true })
    .click();
  const original = page.getByRole("region", {
    name: "Original message",
    exact: true,
  });
  await expect(
    original.getByRole("heading", { name: "Original message · Message 1" }),
  ).toBeFocused();
  await expect(
    original.getByText("Use July evidence and preserve uncertainty.", {
      exact: true,
    }),
  ).toBeVisible();
  await original.getByRole("button", { name: "Close message" }).click();
  await expect(
    page.getByRole("button", { name: "Original message", exact: true }),
  ).toBeFocused();
  await page
    .getByRole("button", { name: "Channel settings", exact: true })
    .click();
  const controls = page.getByRole("region", {
    name: "Channel settings",
    exact: true,
  });
  await controls.getByLabel("Channel name").fill("Reviewed evidence notes");
  await controls
    .getByRole("button", { name: "Save channel name", exact: true })
    .click();
  await expect(
    controls.getByText("Channel renamed.", { exact: true }),
  ).toBeVisible();
  await controls
    .getByRole("button", { name: "Archive channel", exact: true })
    .click();
  await controls
    .getByRole("button", { name: "Confirm archive", exact: true })
    .click();
  await expect(
    controls.getByText(
      "Channel archived. Its messages and your drafts are kept.",
      { exact: true },
    ),
  ).toBeVisible();
  await page.reload();
  await expect(
    page.getByText("Connected locally", { exact: true }),
  ).toBeVisible();
  await page.keyboard.press("Control+5");
  await row.click();
  await page.getByRole("button", { name: "Channels", exact: true }).click();
  await page.getByLabel("Channel status").selectOption("archived");
  await page
    .getByRole("button", {
      name: "Reviewed evidence notes · Archived · 2 unread",
      exact: true,
    })
    .click();
  await expect(
    page.getByText("Keep this context for the next review.", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Post message", exact: true }),
  ).toBeDisabled();
  await page
    .getByRole("button", { name: "Channel settings", exact: true })
    .click();
  await controls
    .getByRole("button", { name: "Restore channel", exact: true })
    .click();
  await controls
    .getByRole("button", { name: "Confirm restore", exact: true })
    .click();
  await page
    .getByLabel("Message to this channel", { exact: true })
    .fill("Posting is available after restoration.");
  await page.getByRole("button", { name: "Post message", exact: true }).click();
  await expect(
    page.getByText("Posting is available after restoration.", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("region", { name: "Reviewer", exact: true }),
  ).toContainText("No member assigned.");
});
