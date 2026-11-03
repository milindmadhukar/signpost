import { test, expect } from "@playwright/test";
import { chooseHost, createLink, openDrawer, row, testHost, toast, watchErrors } from "./helpers";

test.describe("new link drawer", () => {
  test("domain picker lists the known domains and can take a new one", async ({ page }) => {
    const errors = watchErrors(page);
    await page.goto("/links");
    const drawer = await openDrawer(page);
    const select = drawer.locator("select#f-host");
    const options = await select.locator("option").allTextContents();
    expect(options).toEqual(expect.arrayContaining(["milind.dev", "plusxhub.com", "Other domain…"]));
    // A real select, so every domain is offered whatever is selected.
    expect(options.length).toBeGreaterThan(3);

    const other = drawer.getByLabel("New domain");
    await expect(other).toBeHidden();
    await select.selectOption("__other__");
    await expect(other).toBeVisible();
    await expect(other).toBeFocused();
    await select.selectOption("milind.dev");
    await expect(other).toBeHidden();
    expect(errors).toEqual([]);
  });

  test("Generate fills a slug at once, without a round trip", async ({ page }) => {
    await page.goto("/links");
    const drawer = await openDrawer(page);
    let requests = 0;
    page.on("request", (r) => {
      if (new URL(r.url()).pathname.startsWith("/ui/")) requests++;
    });
    const path = drawer.getByLabel("Path");
    await drawer.getByRole("button", { name: /Generate/ }).click();
    const first = await path.inputValue();
    expect(first).toMatch(/^[2-9a-km-zA-HJ-NP-Z]{7}$/);
    await drawer.getByRole("button", { name: /Generate/ }).click();
    expect(await path.inputValue()).not.toBe(first);
    expect(requests).toBe(0);
  });

  test("creating a link closes the drawer, toasts, and lists it", async ({ page }) => {
    const errors = watchErrors(page);
    const { host } = await createLink(page, { path: "hello" });
    await expect(row(page, `${host}/hello`)).toBeVisible();
    expect(errors).toEqual([]);
  });

  test("an empty path creates a generated slug", async ({ page }) => {
    const { host } = await createLink(page);
    await expect(page.locator("#links-table a[data-row-link]", { hasText: host })).toContainText(/\/[2-9a-km-zA-HJ-NP-Z]{7}$/);
  });

  test("Create shows it is working while the server checks the path", async ({ page }) => {
    await page.goto("/links");
    // Hold the request, as a slow live conflict check would.
    await page.route("**/ui/links", async (route) => {
      await new Promise((r) => setTimeout(r, 1200));
      await route.continue();
    });
    const drawer = await openDrawer(page);
    await drawer.getByLabel("Destination URL").fill("https://example.com/slow");
    await chooseHost(drawer, testHost());
    const create = drawer.getByRole("button", { name: /Create link|Checking/ });
    await create.click();
    await expect(drawer.getByText("Checking the path…")).toBeVisible();
    await expect(create).toBeDisabled();
    await expect(drawer).toBeHidden({ timeout: 20_000 });
  });

  test("a server-side error stays in the drawer, with what was typed", async ({ page }) => {
    await page.goto("/links");
    const drawer = await openDrawer(page);
    const host = testHost();
    await drawer.getByLabel("Destination URL").fill("ftp://example.com/nope");
    await chooseHost(drawer, host);
    await drawer.getByLabel("Path").fill("bad");
    await drawer.getByRole("button", { name: "Create link" }).click();
    await expect(drawer.getByRole("alert")).toContainText("http(s)", { timeout: 20_000 });
    await expect(drawer).toBeVisible();
    // The typed domain survives the re-render, still as "Other domain…".
    await expect(drawer.getByLabel("New domain")).toHaveValue(host);
    await expect(drawer.getByLabel("Path")).toHaveValue("bad");

    await drawer.getByLabel("Destination URL").fill("https://example.com/fixed");
    await drawer.getByRole("button", { name: "Create link" }).click();
    await expect(drawer).toBeHidden({ timeout: 20_000 });
    await expect(row(page, `${host}/bad`)).toBeVisible();
  });

  test("a duplicate path is refused", async ({ page }) => {
    const { host } = await createLink(page, { path: "dup" });
    const drawer = await openDrawer(page);
    await drawer.getByLabel("Destination URL").fill("https://example.com/second");
    await chooseHost(drawer, host);
    await drawer.getByLabel("Path").fill("dup");
    await drawer.getByRole("button", { name: "Create link" }).click();
    await expect(drawer.getByRole("alert")).toContainText("already exists", { timeout: 20_000 });
  });

  test("toasts show above the open drawer", async ({ page }) => {
    await page.goto("/links");
    const drawer = await openDrawer(page);
    await page.evaluate(() => document.dispatchEvent(new CustomEvent("toast", { detail: { kind: "err", msg: "over the drawer" } })));
    const t = toast(page);
    await expect(t).toBeVisible();
    // The topmost element at the toast's centre is the toast, not the drawer's backdrop.
    const onTop = await t.evaluate((el) => {
      const r = el.getBoundingClientRect();
      const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
      return !!hit && el.contains(hit);
    });
    expect(onTop).toBe(true);
    await expect(drawer).toBeVisible();
  });

  test("keyboard: n opens the drawer, Escape closes it, / focuses search", async ({ page, isMobile }) => {
    test.skip(isMobile, "no hardware keyboard on a phone");
    await page.goto("/links");
    await page.keyboard.press("n");
    await expect(page.locator("dialog#new-link")).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(page.locator("dialog#new-link")).toBeHidden();
    await page.keyboard.press("/");
    await expect(page.getByLabel("Search links")).toBeFocused();
  });
});

test.describe("links table", () => {
  test("search and filters narrow the list and keep the URL", async ({ page }) => {
    const { host } = await createLink(page, { path: "findme" });
    await page.getByLabel("Search links").fill(host);
    await expect(page.locator("#links-table tbody tr")).toHaveCount(1);
    await expect(page).toHaveURL(new RegExp(`q=${host.replace(/\./g, "\\.")}`));
    await page.getByLabel("Search links").fill("");
    await expect(page.locator("#links-table tbody tr").nth(1)).toBeVisible();

    await page.getByLabel("Status").selectOption("disabled");
    await expect(page).toHaveURL(/state=disabled/);
    const badges = await page.locator("#links-table tbody tr .badge").allTextContents();
    expect(badges.length).toBeGreaterThan(0);
    for (const b of badges) expect(b.trim()).toBe("Disabled");
  });

  test("a row opens its link; the checkbox cell selects instead", async ({ page }) => {
    const { host } = await createLink(page, { path: "rowclick" });
    const r = row(page, `${host}/rowclick`);
    // Clicking beside the checkbox, inside its cell, selects the row.
    const cell = r.locator("td").first();
    const box = await cell.boundingBox();
    await page.mouse.click(box!.x + box!.width - 4, box!.y + box!.height / 2);
    await expect(r.locator("input[name=ids]")).toBeChecked();
    await expect(page).toHaveURL(/\/links(\?|$)/);
    await expect(page.locator("#bulk-bar")).toBeVisible();
    await expect(page.locator("#bulk-count")).toHaveText("1 selected");

    // Clicking the link text opens the link's page.
    await r.locator("a[data-row-link]").click();
    await expect(page).toHaveURL(/\/links\/\d+/);
    await expect(page.locator("h1")).toContainText("/rowclick");
  });

  test("row menu: disable, enable, delete with a confirm", async ({ page }) => {
    const errors = watchErrors(page);
    const { host } = await createLink(page, { path: "menu" });
    const url = `${host}/menu`;
    const menu = async () => {
      await row(page, url).locator("summary[aria-label='More actions']").click();
      return row(page, url).locator(".menu-list");
    };

    await (await menu()).getByRole("button", { name: "Disable" }).click();
    await expect(toast(page)).toContainText("Disabled");
    await expect(row(page, url).locator(".badge:visible", { hasText: "Disabled" })).toBeVisible();

    await (await menu()).getByRole("button", { name: "Enable" }).click();
    await expect(toast(page)).toContainText("Enabled");

    // Cancel keeps it.
    await (await menu()).getByRole("button", { name: "Delete" }).click();
    const confirm = page.locator("dialog#confirm");
    await expect(confirm).toBeVisible();
    await expect(confirm).toContainText(url);
    await confirm.getByRole("button", { name: "Cancel" }).click();
    await expect(confirm).toBeHidden();
    await expect(row(page, url)).toBeVisible();

    // Delete removes it.
    await (await menu()).getByRole("button", { name: "Delete" }).click();
    await confirm.getByRole("button", { name: "Delete" }).click();
    await expect(row(page, url)).toHaveCount(0);
    await expect(toast(page)).toContainText("Deleted");
    expect(errors).toEqual([]);
  });

  test("the row menu closes when clicking elsewhere", async ({ page }) => {
    await page.goto("/links");
    const details = page.locator("#links-table tbody details.menu").first();
    await details.locator("summary").click();
    await expect(details).toHaveAttribute("open", "");
    await page.locator("h1").click();
    await expect(details).not.toHaveAttribute("open", "");
  });

  test("copy puts the full URL on the clipboard", async ({ page, context, browserName, isMobile }) => {
    test.skip(browserName !== "chromium" || isMobile, "clipboard permissions are Chromium-only in Playwright");
    await context.grantPermissions(["clipboard-read", "clipboard-write"]);
    const { host } = await createLink(page, { path: "copyme" });
    await row(page, `${host}/copyme`).getByRole("button", { name: /Copy/ }).click();
    await expect(toast(page)).toContainText("Copied");
    expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(`https://${host}/copyme`);
  });

  test("bulk: select, tag and delete several links", async ({ page }) => {
    const host = testHost();
    await createLink(page, { host, path: "bulk-a" });
    await createLink(page, { host, path: "bulk-b" });
    await page.getByLabel("Search links").fill(host);
    await expect(page.locator("#links-table tbody tr")).toHaveCount(2);

    await page.locator("#select-all").check();
    await expect(page.locator("#bulk-count")).toHaveText("2 selected");
    await page.getByLabel("Tag to add").fill("E2E Batch");
    await page.getByRole("button", { name: "Add tag" }).click();
    await expect(toast(page)).toContainText("Tagged 2 links");
    await expect(page.locator("#links-table tbody .tag", { hasText: "e2e-batch" })).toHaveCount(2);

    await page.locator("#select-all").check();
    await page.locator("#bulk-bar").getByRole("button", { name: "Delete" }).click();
    await page.locator("dialog#confirm").getByRole("button", { name: "Delete" }).click();
    await expect(toast(page)).toContainText("Deleted 2 links");
    await expect(page.locator("#links-table tbody tr")).toHaveCount(0);
  });
});
