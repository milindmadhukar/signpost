import { expect, type Page, type Locator } from "@playwright/test";

// A host nothing on the internet answers, so signpost's live conflict check
// finds no site there and lets the link through. Unique per call, so tests
// never trip over each other's links.
export function testHost(): string {
  return `e2e-${Date.now().toString(36)}${Math.floor(Math.random() * 1e4).toString(36)}.test`;
}

// Fail the test on any page error or console error, which is how htmx
// reports a broken attribute (a bad hx-target, a selector that matches
// nothing) without anything visibly going wrong.
export function watchErrors(page: Page): string[] {
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(`pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error" && !/favicon|Failed to load resource/.test(m.text())) errors.push(`console: ${m.text()}`);
  });
  return errors;
}

export function toast(page: Page): Locator {
  return page.locator("#toasts .toast").last();
}

export function row(page: Page, url: string): Locator {
  return page.locator("#links-table tbody tr", { has: page.locator(`a[data-row-link]`, { hasText: url }) });
}

/** Opens the new-link drawer from the Links page. */
export async function openDrawer(page: Page): Promise<Locator> {
  await page.getByRole("button", { name: /New link/ }).first().click();
  const drawer = page.locator("dialog#new-link");
  await expect(drawer).toBeVisible();
  return drawer;
}

/** Creates a link through the drawer, typing a new domain. */
export async function createLink(page: Page, opts: { host?: string; path?: string; target?: string } = {}) {
  const host = opts.host ?? testHost();
  const target = opts.target ?? `https://example.com/${host}`;
  if (!page.url().endsWith("/links")) await page.goto("/links");
  const drawer = await openDrawer(page);
  await drawer.getByLabel("Destination URL").fill(target);
  await chooseHost(drawer, host);
  if (opts.path !== undefined) await drawer.getByLabel("Path").fill(opts.path);
  await drawer.getByRole("button", { name: "Create link" }).click();
  // Creating asks the live internet whether the path is taken first, and a
  // made-up .test host can take the resolver a few seconds to give up on.
  await expect(drawer).toBeHidden({ timeout: 20_000 });
  await expect(toast(page)).toContainText("Created");
  return { host, target };
}

/** Picks a domain in the drawer: an existing one, or "Other domain…" and types it. */
export async function chooseHost(drawer: Locator, host: string) {
  const select = drawer.locator("select#f-host");
  const options = await select.locator("option").allTextContents();
  if (options.includes(host)) {
    await select.selectOption(host);
  } else {
    await select.selectOption("__other__");
    const other = drawer.getByLabel("New domain");
    await expect(other).toBeVisible();
    await other.fill(host);
  }
}
