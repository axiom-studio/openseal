import { test, expect } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";

test("proposal review answers the next eligible question and preserves revision authority", async ({
  page,
}) => {
  let proposal = {
    id: "review-fixture",
    prompt: "Review research evidence",
    status: "blocked",
    revision: 7,
    candidateDigest: "fixture-digest",
    result: {
      valid: false,
      candidate: {
        agents: [
          {
            id: "research",
            displayName: "Research reviewer",
            purpose: "Compare evidence and report uncertainty.",
            systemPrompt: "Cite your sources. Ask before external actions.",
          },
        ],
      },
      assumptions: ["Reports should include citations."],
      validation: [
        {
          message: "Choose the intended audience.",
          path: "audience",
          code: "missing",
        },
      ],
    },
    refinement: {
      questions: [
        {
          id: "dependent",
          prompt: "Which technical field?",
          whyNeeded: "Tailor the detail.",
          priority: 100,
          dependsOn: [
            { questionId: "audience", requiredOptionIds: ["technical"] },
          ],
          answer: { kind: "text" },
        },
        {
          id: "audience",
          prompt: "Who is the report for?",
          whyNeeded: "This determines the level of detail.",
          priority: 10,
          answer: {
            kind: "single_select",
            options: [
              {
                id: "technical",
                label: "Technical readers",
                description: "Include methods and limitations.",
              },
              { id: "general", label: "General readers" },
            ],
          },
        },
      ],
      answers: [] as { questionId: string; value: { optionIds: string[] } }[],
    },
  };
  await page.addInitScript(() =>
    localStorage.setItem("openseal.proposal", "review-fixture"),
  );
  await page.route("**/api/v1/capabilities**", (route) =>
    route.fulfill({
      json: {
        capabilities: [
          {
            id: "workforce-authoring",
            available: true,
            operations: ["get", "propose", "refine"],
            context: { changeSetId: proposal.id, revision: proposal.revision },
          },
        ],
      },
    }),
  );
  await page.route(
    "**/api/v1/authoring/workforce/change-sets/**",
    async (route) => {
      if (route.request().method() === "POST") {
        const body = route.request().postDataJSON();
        expect(body.expectedRevision).toBe(7);
        expect(body.questionId).toBe("audience");
        expect(body.value).toEqual({ optionIds: ["technical"] });
        expect(route.request().headers()["idempotency-key"]).toBeTruthy();
        proposal = {
          ...proposal,
          revision: 8,
          refinement: {
            ...proposal.refinement,
            answers: [
              { questionId: "audience", value: { optionIds: ["technical"] } },
            ],
          },
        };
      }
      await route.fulfill({ json: proposal });
    },
  );
  await page.goto("/");
  await expect(
    page.getByRole("heading", { name: "Research reviewer" }),
  ).toBeVisible();
  await expect(
    page.getByText("Which technical field?", { exact: true }),
  ).toHaveCount(0);
  await page.getByLabel("Technical readers", { exact: false }).check();
  await expect(
    page.getByRole("button", { name: "Save answer and continue" }),
  ).toBeEnabled();
  expect(
    (
      await new AxeBuilder({ page })
        .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
        .analyze()
    ).violations,
  ).toEqual([]);
  await page.screenshot({
    path: "../.impeccable/review/proposal-review.png",
    fullPage: true,
  });
  await page.getByRole("button", { name: "Save answer and continue" }).click();
  await expect(
    page.getByText("Which technical field?", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("status").filter({ hasText: "Answer saved" }),
  ).toBeVisible();
  await page.setViewportSize({ width: 390, height: 844 });
  await page.evaluate(() => {
    (document.activeElement as HTMLElement)?.blur();
    window.scrollTo(0, 0);
  });
  await page.screenshot({
    path: "../.impeccable/review/proposal-mobile.png",
    fullPage: true,
  });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
});

test("failed proposal shows the kernel error and reuses retry key after uncertain delivery", async ({
  page,
}) => {
  const proposal = {
    id: "failed-fixture",
    prompt: "Draft a report",
    status: "failed",
    revision: 4,
    generation: { attempt: 1, lastError: "Provider could not be reached." },
    result: { valid: false, candidate: { agents: [] } },
  };
  const keys: string[] = [];
  await page.addInitScript(() =>
    localStorage.setItem("openseal.proposal", "failed-fixture"),
  );
  await page.route("**/api/v1/capabilities**", (route) =>
    route.fulfill({
      json: {
        capabilities: [
          {
            id: "workforce-authoring",
            available: true,
            operations: ["get", "retry"],
            context: { changeSetId: proposal.id, revision: proposal.revision },
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/authoring/workforce/change-sets/**", (route) => {
    if (route.request().method() === "POST") {
      expect(route.request().postDataJSON().expectedRevision).toBe(4);
      keys.push(route.request().headers()["idempotency-key"]);
      return route.fulfill({
        status: 503,
        json: {
          error: "Connection interrupted. Check the proposal before retrying.",
        },
      });
    }
    return route.fulfill({ json: proposal });
  });
  await page.goto("/");
  await expect(page.getByText("Provider could not be reached.")).toBeVisible();
  await page
    .getByRole("button", { name: "Retry generation", exact: true })
    .click();
  await expect(
    page.getByText(
      "Connection interrupted. Check the proposal before retrying.",
    ),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Retry generation", exact: true })
    .click();
  await expect.poll(() => keys.length).toBe(2);
  expect(keys[0]).toBeTruthy();
  expect(keys[0]).toBe(keys[1]);
});

test("revision conflicts refresh the proposal without replaying an outdated answer", async ({
  page,
}) => {
  let revision = 3;
  let mutations = 0;
  const proposal = () => ({
    id: "conflict-fixture",
    prompt: "Review evidence",
    status: "blocked",
    revision,
    candidateDigest: "fixture",
    result: { valid: false, candidate: { agents: [] } },
    refinement: {
      questions: [
        {
          id: "audience",
          prompt: "Who is this for?",
          whyNeeded: "Determine the detail.",
          priority: 1,
          answer: { kind: "text" },
        },
      ],
      answers: [],
    },
  });
  await page.addInitScript(() =>
    localStorage.setItem("openseal.proposal", "conflict-fixture"),
  );
  await page.route("**/api/v1/capabilities**", (route) =>
    route.fulfill({
      json: {
        capabilities: [
          {
            id: "workforce-authoring",
            available: true,
            operations: ["get", "refine"],
            context: { changeSetId: "conflict-fixture", revision: 3 },
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/authoring/workforce/change-sets/**", (route) => {
    if (route.request().method() === "POST") {
      mutations++;
      revision = 4;
      return route.fulfill({
        status: 409,
        json: { error: "revision conflict" },
      });
    }
    return route.fulfill({ json: proposal() });
  });
  await page.goto("/");
  await page
    .getByRole("textbox", { name: "Your answer", exact: true })
    .fill("Research staff");
  await page.getByRole("button", { name: "Save answer and continue" }).click();
  await expect(page.getByRole("alert")).toContainText(
    "The proposal changed while you were reviewing it",
  );
  // Capabilities for revision 3 cannot authorize a mutation against revision 4.
  await expect(
    page.getByRole("button", { name: "Save answer and continue" }),
  ).toBeDisabled();
  expect(mutations).toBe(1);
});

test("collection answers enforce advertised minimum and maximum", async ({
  page,
}) => {
  await page.addInitScript(() =>
    localStorage.setItem("openseal.proposal", "limits-fixture"),
  );
  await page.route("**/api/v1/capabilities**", (route) =>
    route.fulfill({
      json: {
        capabilities: [
          {
            id: "workforce-authoring",
            available: true,
            operations: ["get", "refine"],
            context: { changeSetId: "limits-fixture", revision: 1 },
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/authoring/workforce/change-sets/**", (route) =>
    route.fulfill({
      json: {
        id: "limits-fixture",
        revision: 1,
        status: "blocked",
        prompt: "Research topics",
        refinement: {
          questions: [
            {
              id: "topics",
              priority: 1,
              prompt: "Which two topics?",
              whyNeeded: "Choose the report scope.",
              answer: {
                kind: "multi_select",
                minimum: 2,
                maximum: 2,
                options: [
                  { id: "a", label: "Alpha" },
                  { id: "b", label: "Beta" },
                  { id: "c", label: "Gamma" },
                ],
              },
            },
          ],
        },
      },
    }),
  );
  await page.goto("/");
  await expect(page.getByText("Provide exactly 2 values.")).toBeVisible();
  await page.getByLabel("Alpha", { exact: true }).check();
  await expect(
    page.getByRole("button", { name: "Save answer and continue" }),
  ).toBeDisabled();
  await page.getByLabel("Beta", { exact: true }).check();
  await expect(
    page.getByRole("button", { name: "Save answer and continue" }),
  ).toBeEnabled();
  await expect(page.getByLabel("Gamma", { exact: true })).toBeDisabled();
  await page.getByLabel("Alpha", { exact: true }).uncheck();
  await expect(page.getByLabel("Gamma", { exact: true })).toBeEnabled();
});

test("an old refinement response cannot replace a newly created proposal", async ({
  page,
}) => {
  const old = {
    id: "old-proposal",
    revision: 1,
    status: "blocked",
    prompt: "Old proposal",
    refinement: {
      questions: [
        {
          id: "audience",
          priority: 1,
          prompt: "Who is it for?",
          whyNeeded: "Set the audience.",
          answer: { kind: "text" },
        },
      ],
    },
  };
  const next = {
    id: "new-proposal",
    scope: { kind: "local", id: "default" },
    revision: 1,
    status: "review",
    prompt: "New proposal",
  };
  let release!: () => void;
  const pending = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.addInitScript(() =>
    localStorage.setItem("openseal.proposal", "old-proposal"),
  );
  await page.route("**/api/v1/capabilities**", (route) =>
    route.fulfill({
      json: {
        capabilities: [
          {
            id: "workforce-authoring",
            available: true,
            operations: ["get", "propose", "refine"],
            context: { changeSetId: "old-proposal", revision: 1 },
          },
        ],
      },
    }),
  );
  await page.route(
    "**/api/v1/authoring/workforce/change-sets**",
    async (route) => {
      const url = new URL(route.request().url());
      if (url.pathname.endsWith("/refinements")) {
        await pending;
        return route.fulfill({ json: { ...old, revision: 2 } });
      }
      return route.fulfill({
        json:
          route.request().method() === "POST" ||
          url.pathname.endsWith("/new-proposal")
            ? next
            : old,
      });
    },
  );
  await page.goto("/");
  await page
    .getByRole("textbox", { name: "Your answer", exact: true })
    .fill("Research staff");
  await page.getByRole("button", { name: "Save answer and continue" }).click();
  await expect(
    page.getByRole("button", { name: "Saving answer…" }),
  ).toBeVisible();
  await page
    .getByRole("textbox", { name: "Describe your agent" })
    .fill("New proposal");
  await page
    .getByRole("button", { name: "Create proposal", exact: true })
    .click();
  await expect(page.locator(".proposal-prompt")).toHaveText("New proposal");
  const response = page.waitForResponse((r) =>
    r.url().endsWith("/refinements"),
  );
  release();
  await (await response).finished();
  await page.evaluate(
    () =>
      new Promise<void>((resolve) =>
        requestAnimationFrame(() => requestAnimationFrame(() => resolve())),
      ),
  );
  await expect(page.locator(".proposal-prompt")).toHaveText("New proposal");
  expect(
    await page.evaluate(() => localStorage.getItem("openseal.proposal")),
  ).toBe("new-proposal");
});
