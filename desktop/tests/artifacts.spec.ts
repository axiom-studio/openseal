import { test, expect, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { createHash } from "node:crypto";

const run = {
  id: "artifact-run",
  goal: "Inspect research files",
  status: "completed",
  revision: 1,
};
const bytes = Buffer.from(
  "<script>window.artifactExecuted=true</script>\nResearch evidence with explicit uncertainty.",
);
const record = (id = "report", version = 2) => ({
  id,
  version,
  name: "Research report.html",
  mediaType: "text/html",
  sizeBytes: bytes.length,
  digest: `sha256:${createHash("sha256").update(bytes).digest("hex")}`,
  classification: "internal",
  provenance: {
    runId: run.id,
    producer: { type: "agent", id: "Research analyst" },
  },
});
async function setup(page: Page) {
  await page.route("**/api/v1/capabilities", (r) =>
    r.fulfill({
      json: {
        capabilities: [
          { id: "agent-runs", available: true, operations: ["list", "get"] },
          {
            id: "artifacts",
            available: true,
            operations: ["list", "get", "download"],
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/agent-runs?*", (r) => r.fulfill({ json: [run] }));
}

test("artifacts preserve immutable versions, preview inert text and remain accessible at narrow widths", async ({
  page,
}) => {
  await setup(page);
  let lists = 0;
  await page.route("**/api/v1/artifacts?*", (r) => {
    lists++;
    const q = new URL(r.request().url()).searchParams;
    expect(q.get("producerRunId")).toBe(run.id);
    expect(q.get("scopeKind")).toBe("local");
    expect(q.get("scopeId")).toBe("default");
    expect(q.get("latestOnly")).toBe("false");
    return r.fulfill({ json: [record(), record("report", 1)] });
  });
  await page.route("**/api/v1/artifacts/report?*", (r) => {
    expect(new URL(r.request().url()).searchParams.get("version")).toBe("2");
    return r.fulfill({ json: record() });
  });
  await page.route("**/api/v1/artifacts/report/content?*", (r) => {
    expect(new URL(r.request().url()).searchParams.get("version")).toBe("2");
    return r.fulfill({ body: bytes, contentType: "text/html" });
  });
  await page.goto("/");
  await page.getByRole("button", { name: /Inspect research files/ }).click();
  expect(lists).toBe(0);
  await page.getByText("Artifacts", { exact: true }).click();
  await expect(page.locator(".artifact-list li")).toHaveCount(2);
  await page.getByRole("button", { name: "Preview text" }).first().click();
  await expect(
    page.getByLabel("Text preview of Research report.html"),
  ).toHaveText(bytes.toString());
  expect(await page.evaluate(() => "artifactExecuted" in window)).toBe(false);
  await expect(
    page.getByLabel("Text preview of Research report.html"),
  ).toBeFocused();
  await page.locator(".task-artifacts").scrollIntoViewIfNeeded();
  await page.screenshot({
    path: "../.impeccable/review/artifacts-desktop.png",
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.locator(".artifact-list").scrollIntoViewIfNeeded();
  await page.screenshot({ path: "../.impeccable/review/artifacts-mobile.png" });
  expect(
    (
      await new AxeBuilder({ page })
        .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
        .analyze()
    ).violations,
  ).toEqual([]);
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
});

test("corrupt artifact content never downloads and refresh recovers list failures", async ({
  page,
}) => {
  await setup(page);
  let fail = true;
  await page.route("**/api/v1/artifacts?*", (r) =>
    fail
      ? r.fulfill({ status: 503, json: { error: "Temporarily unavailable" } })
      : r.fulfill({ json: [record()] }),
  );
  await page.route("**/api/v1/artifacts/report?*", (r) =>
    r.fulfill({ json: record() }),
  );
  await page.route("**/api/v1/artifacts/report/content?*", (r) =>
    r.fulfill({ body: Buffer.alloc(bytes.length, 120) }),
  );
  let downloads = 0;
  page.on("download", () => downloads++);
  await page.goto("/");
  await page.getByRole("button", { name: /Inspect research files/ }).click();
  await page.getByText("Artifacts", { exact: true }).click();
  await expect(page.locator(".task-artifacts [role=alert]")).toContainText(
    "Could not load artifacts",
  );
  fail = false;
  await page.getByRole("button", { name: "Refresh artifacts" }).click();
  await page.getByRole("button", { name: "Save as…" }).click();
  await expect(page.locator(".task-artifacts [role=alert]")).toHaveText(
    "Artifact checksum does not match. No file was saved.",
  );
  expect(downloads).toBe(0);
});

test("artifact paging and delayed previews cannot leak into a different task", async ({
  page,
}) => {
  await setup(page);
  const other = { ...run, id: "other", goal: "Other research files" };
  await page.route("**/api/v1/agent-runs?*", (r) =>
    r.fulfill({ json: [run, other] }),
  );
  await page.route("**/api/v1/artifacts?*", (r) => {
    const q = new URL(r.request().url()).searchParams;
    if (q.get("producerRunId") === "other") return r.fulfill({ json: [] });
    return r.fulfill({
      json:
        q.get("offset") === "20"
          ? [{ ...record("last"), name: "Last artifact.txt" }]
          : Array.from({ length: 21 }, (_, i) => ({
              ...record(`report-${i}`),
              name: `Report ${i}.html`,
            })),
    });
  });
  let release!: () => void;
  const gate = new Promise<void>((resolve) => (release = resolve));
  await page.route("**/api/v1/artifacts/last?*", (r) =>
    r.fulfill({ json: { ...record("last"), name: "Last artifact.txt" } }),
  );
  await page.route("**/api/v1/artifacts/last/content?*", async (r) => {
    await gate;
    await r.fulfill({ body: bytes, contentType: "text/plain" });
  });
  await page.goto("/");
  await page.getByRole("button", { name: /Inspect research files/ }).click();
  await page.getByText("Artifacts", { exact: true }).click();
  await expect(page.locator(".artifact-list li")).toHaveCount(20);
  await page.getByRole("button", { name: "Next artifacts" }).click();
  await expect(page.locator(".artifact-list li")).toHaveCount(1);
  await expect(
    page.getByRole("heading", { name: "Last artifact.txt" }),
  ).toBeVisible();
  const started = page.waitForRequest("**/api/v1/artifacts/last/content?*");
  await page.getByRole("button", { name: "Preview text" }).click();
  await started;
  await page
    .getByRole("button", { name: "Close details", exact: true })
    .click();
  await page.getByRole("button", { name: /Other research files/ }).click();
  await page.getByText("Artifacts", { exact: true }).click();
  await expect(
    page.getByText("No artifacts have been recorded for this task."),
  ).toBeVisible();
  const response = page.waitForResponse("**/api/v1/artifacts/last/content?*");
  release();
  await (await response).finished();
  await page.evaluate(
    () =>
      new Promise((resolve) =>
        requestAnimationFrame(() => requestAnimationFrame(resolve)),
      ),
  );
  await expect(page.locator(".task-artifacts pre")).toHaveCount(0);
  await expect(page.getByText(/Preview loaded for Last/)).toHaveCount(0);
});
