import { defineConfig, devices } from "@playwright/test";

// The dashboard is exercised in two engines: desktop Chromium, and WebKit
// on an emulated iPhone, since Safari is where htmx-and-native-controls
// trouble (datalists, dialogs, popovers, tap targets) tends to show up.
export default defineConfig({
  testDir: "tests",
  // The tests share one server and database, and some read global state
  // (counts, the domain list), so they run one at a time.
  workers: 1,
  fullyParallel: false,
  retries: process.env.CI ? 1 : 0,
  timeout: 30_000,
  expect: { timeout: 8_000 },
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : "list",
  use: {
    baseURL: "http://localhost:18281",
    extraHTTPHeaders: { "Remote-User": "e2e" },
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  projects: [
    { name: "desktop-chromium", use: { ...devices["Desktop Chrome"], viewport: { width: 1440, height: 900 } } },
    { name: "iphone-webkit", use: { ...devices["iPhone 13"] } },
  ],
  webServer: {
    command: "sh serve.sh",
    url: "http://localhost:18281/healthz",
    reuseExistingServer: !process.env.CI,
    timeout: 60_000,
    stdout: "ignore",
    stderr: "pipe",
  },
});
