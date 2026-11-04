import { test, expect } from "@playwright/test";
import { toast, watchErrors } from "./helpers";

const pages = [
  { nav: "Links", url: /\/links$/, h1: "Links" },
  { nav: "Analytics", url: /\/analytics$/, h1: "Analytics" },
  { nav: "Domains", url: /\/domains$/, h1: "Domains" },
  { nav: "Import", url: /\/import$/, h1: "Import" },
  { nav: "Settings", url: /\/settings$/, h1: "Settings" },
];

test("the sidebar reaches every page, and Back works", async ({ page, isMobile }) => {
  const errors = watchErrors(page);
  await page.goto("/links");
  for (const p of pages) {
    if (isMobile) await page.getByRole("button", { name: "Open menu" }).click();
    await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: p.nav }).click();
    await expect(page).toHaveURL(p.url);
    await expect(page.locator("main h1")).toHaveText(p.h1);
    await expect(page.locator(`nav a[aria-current=page]`)).toHaveText(p.nav);
    if (isMobile) {
      // The slide-over closes once the page has loaded.
      await expect(page.locator("body")).not.toHaveAttribute("data-nav-open", "");
      await expect(page.getByRole("navigation", { name: "Main" })).not.toBeInViewport();
    }
  }
  await page.goBack();
  await expect(page).toHaveURL(/\/import$/);
  await expect(page.locator("main h1")).toHaveText("Import");
  await page.goBack();
  await expect(page.locator("main h1")).toHaveText("Domains");
  expect(errors).toEqual([]);
});

test("theme switch applies at once and survives a reload", async ({ page, isMobile }) => {
  await page.goto("/links");
  if (isMobile) await page.getByRole("button", { name: "Open menu" }).click();
  await page.getByRole("button", { name: "Dark" }).click();
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  if (isMobile) await page.getByRole("button", { name: "Open menu" }).click();
  await expect(page.getByRole("button", { name: "Dark" })).toHaveAttribute("aria-pressed", "true");
  await page.getByRole("button", { name: "Light" }).click();
  await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
});

test("no page scrolls sideways", async ({ page, isMobile }) => {
  // Tablet and half-screen widths too: there the sidebar is showing but the
  // wide tables no longer fit their cards and scroll inside them.
  const widths = isMobile ? [0] : [0, 820, 1024];
  for (const width of widths) {
    if (width) await page.setViewportSize({ width, height: 900 });
    for (const url of ["/links", "/links/1", "/analytics", "/analytics?link=1", "/domains", "/import", "/settings"]) {
      await page.goto(url);
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
      expect(overflow, `${url} at ${width || "default"} px`).toBeLessThanOrEqual(0);
    }
  }
});

test.describe("failed requests", () => {
  // page.route() cannot see requests from a page a service worker controls
  // (in WebKit at least), so the worker is kept out of this one test. Every
  // other test runs with it, which is what shows it leaves htmx alone.
  test.use({ serviceWorkers: "block" });

  test("a request that cannot reach the server says so, with a way out", async ({ page }) => {
    await page.goto("/links");
    // What an expired Authelia session looks like from htmx: the request is
    // redirected cross-origin and fails outright.
    await page.route("**/ui/**", (route) => route.abort("failed"));
    await page.locator("#links-table tbody summary[aria-label='More actions']").first().click();
    await page.locator("#links-table tbody .menu-list").first().getByRole("button", { name: "Check again" }).click();
    await expect(toast(page)).toContainText("Could not reach signpost");
    await expect(toast(page).getByRole("button", { name: "Reload" })).toBeVisible();
  });
});

test("every page renders without errors", async ({ page }) => {
  const errors = watchErrors(page);
  // /links/8 and /links/10 are the demo's expired and expiring links, whose
  // header once stopped the page rendering halfway.
  for (const url of [
    "/links",
    "/links/1",
    "/links/8",
    "/links/10",
    "/analytics",
    "/analytics?range=24h",
    "/analytics?range=90d&link=2",
    "/domains",
    "/import",
    "/settings",
  ]) {
    const res = await page.goto(url);
    expect(res?.status(), url).toBe(200);
    if (url.startsWith("/links/")) await expect(page.locator("#settings-card"), url).toBeVisible();
  }
  expect(errors).toEqual([]);
});
