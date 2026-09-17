import { defineConfig } from "@playwright/test";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
// Every test run owns its workspace. It never changes a developer's provider.
process.env.OPENSEAL_UI_TEST_WORKSPACE ||= mkdtempSync(
  join(tmpdir(), "openseal-ui-test-"),
);
export default defineConfig({
  testDir: "./tests",
  fullyParallel: false,
  workers: 1,
  use: {
    baseURL: "http://127.0.0.1:1421",
    headless: true,
    viewport: { width: 1440, height: 1000 },
    trace: "retain-on-failure",
  },
  webServer: {
    command: "pnpm dev --port 1421",
    url: "http://127.0.0.1:1421/__desktop/status",
    reuseExistingServer: false,
    timeout: 60000,
    env: { OPENSEAL_DEV_WORKSPACE: process.env.OPENSEAL_UI_TEST_WORKSPACE },
  },
});
