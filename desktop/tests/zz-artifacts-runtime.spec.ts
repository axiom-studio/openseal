import { test, expect } from "@playwright/test";
import { readFileSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { join } from "node:path";

test("real daemon serves exact text versions and exports binary bytes unchanged", async ({
  page,
}) => {
  test.setTimeout(60000);
  execFileSync(
    "go",
    [
      "run",
      "tests/seed_artifact.go",
      "-db",
      join(process.env.OPENSEAL_UI_TEST_WORKSPACE!, "data/openseal.db"),
    ],
    { timeout: 30000 },
  );
  await page.goto("/");
  await page
    .getByRole("navigation")
    .getByRole("button", { name: /Work/ })
    .click();
  await page
    .getByRole("button", { name: /Inspect synthetic task artifacts/ })
    .click();
  await page.getByText("Artifacts", { exact: true }).click();
  const rows = page.locator(".artifact-list li");
  await expect(rows).toHaveCount(3);
  for (const version of [1, 2]) {
    const row = rows.filter({ hasText: `Version ${version} ·` }).filter({
      has: page.getByRole("heading", { name: "report.txt", exact: true }),
    });
    await row.getByRole("button", { name: "Preview text" }).click();
    await expect(row.getByLabel("Text preview of report.txt")).toHaveText(
      version === 1
        ? "First saved report."
        : "Verified task artifact.\nUncertainty remains explicit.",
    );
  }
  const download = page.waitForEvent("download");
  await rows
    .filter({ hasText: "evidence.bin" })
    .getByRole("button", { name: "Save as…" })
    .click();
  const result = await download;
  expect(result.suggestedFilename()).toBe("evidence.bin");
  expect(readFileSync((await result.path())!)).toEqual(
    Buffer.from([0, 255, 128, 13, 10, 1, 254]),
  );
});
