import { test, expect } from "@playwright/test";
import { createLink, testHost, toast, watchErrors } from "./helpers";

test.describe("link page", () => {
  test("edit settings, set and remove a password, switch range", async ({ page }) => {
    const errors = watchErrors(page);
    const { host } = await createLink(page, { path: "detail" });
    await page.locator("#links-table a[data-row-link]", { hasText: `${host}/detail` }).click();
    await expect(page).toHaveURL(/\/links\/\d+/);

    const settings = page.locator("#settings-card");
    await settings.getByLabel("Destination").fill("https://example.com/changed");
    await settings.getByLabel("Tags").fill("one, Two");
    await settings.getByRole("button", { name: "Save changes" }).click();
    await expect(toast(page)).toContainText("Saved");
    await expect(page.locator("#link-header")).toContainText("https://example.com/changed");
    await expect(page.locator("#link-header .tag")).toHaveText(["one", "two"]);

    const pw = page.locator("#password-card");
    await pw.getByLabel("Password").fill("hunter2");
    await pw.getByRole("button", { name: "Set" }).click();
    await expect(toast(page)).toContainText("Password set");
    await expect(page.locator("#link-header")).toContainText("Password");
    await page.locator("#password-card").getByRole("button", { name: "Remove password" }).click();
    await page.locator("dialog#confirm").getByRole("button", { name: "Remove" }).click();
    await expect(toast(page)).toContainText("Password removed");
    await expect(page.locator("#password-card")).toContainText("Off");

    await page.locator(".seg a", { hasText: "7d" }).click();
    await expect(page).toHaveURL(/range=7d/);
    await expect(page.getByText("Last 7 days, by day")).toBeVisible();
    await expect(page.locator(".seg a[aria-current=true]")).toHaveText("7d");
    expect(errors).toEqual([]);
  });

  test("disable from the settings card turns the link off", async ({ page }) => {
    const { host } = await createLink(page, { path: "switch" });
    await page.locator("#links-table a[data-row-link]", { hasText: `${host}/switch` }).click();
    await page.locator("#settings-card").getByLabel("Enabled").uncheck();
    await page.locator("#settings-card").getByRole("button", { name: "Save changes" }).click();
    await expect(page.locator("#link-header")).toContainText("Disabled");
  });

  test("delete from the link page goes back to the list", async ({ page }) => {
    const { host } = await createLink(page, { path: "goner" });
    await page.locator("#links-table a[data-row-link]", { hasText: `${host}/goner` }).click();
    await page.getByRole("button", { name: "Delete link" }).click();
    await page.locator("dialog#confirm").getByRole("button", { name: "Delete" }).click();
    await expect(page).toHaveURL(/\/links$/);
    await expect(page.locator("#links-table a[data-row-link]", { hasText: `${host}/goner` })).toHaveCount(0);
  });
});

test.describe("analytics", () => {
  test("overall, then per link, then back", async ({ page }) => {
    const errors = watchErrors(page);
    await page.goto("/analytics?range=30d");
    const perLink = page.locator("#per-link");
    await expect(perLink).toBeVisible();
    await expect(perLink.locator("tbody tr").first()).toBeVisible();

    await page.getByLabel("Link", { exact: true }).selectOption({ label: "plusxhub.com/tickets" });
    await expect(page).toHaveURL(/link=\d+/);
    await expect(page.getByRole("link", { name: "Link settings" })).toBeVisible();
    await expect(perLink).toHaveCount(0);

    // Switching range keeps the link.
    await page.locator(".seg a", { hasText: "7d" }).click();
    await expect(page).toHaveURL(/range=7d&link=\d+/);
    await expect(page.getByLabel("Link", { exact: true })).toHaveValue(/\d+/);

    await page.getByRole("link", { name: "All links" }).click();
    await expect(page).toHaveURL(/\/analytics\?range=7d$/);
    await expect(page.locator("#per-link")).toBeVisible();

    // A row opens that link's page on the same range.
    await page.locator("#per-link tbody tr").first().locator("td").nth(2).click();
    await expect(page).toHaveURL(/\/links\/\d+\?range=7d/);
    expect(errors).toEqual([]);
  });

  // Hover on desktop, tap on the iPhone: either way the country's numbers
  // show, and a tap or move off the map hides them.
  test("world map shows a country's clicks", async ({ page, isMobile }) => {
    const errors = watchErrors(page);
    await page.goto("/analytics?range=30d");
    const card = page.locator("#locations");
    const india = card.locator('.geo path[data-name="India"]');
    await india.scrollIntoViewIfNeeded();
    await expect(india).not.toHaveClass("geo-0");
    const box = (await india.boundingBox())!;
    const at = { x: box.x + box.width / 2, y: box.y + box.height * 0.4 };
    if (isMobile) await page.touchscreen.tap(at.x, at.y);
    else await page.mouse.move(at.x, at.y);
    const tip = card.locator(".geo-tip");
    await expect(tip).toBeVisible();
    await expect(tip).toContainText(/India · [\d,]+ clicks · \d+%/);
    // The list beside the map agrees with the tooltip.
    const n = (await card.locator('[data-geo-pane="countries"] .barlist-row', { hasText: "India" }).locator(".font-medium").textContent())!.trim();
    await expect(tip).toContainText(`${n} click`);

    const title = card.locator(".card-title");
    if (isMobile) await title.tap();
    else await title.hover();
    await expect(tip).toBeHidden();
    expect(errors).toEqual([]);
  });

  // The switch swaps the list beside the map, and the choice survives a
  // range change and a reload.
  test("locations list switches between countries and cities", async ({ page }) => {
    const errors = watchErrors(page);
    await page.goto("/analytics?range=30d");
    await page.evaluate(() => localStorage.removeItem("geo-list"));
    await page.reload();
    const card = page.locator("#locations");
    const countries = card.locator('[data-geo-pane="countries"]');
    const cities = card.locator('[data-geo-pane="cities"]');
    await expect(countries).toBeVisible();
    await expect(cities).toBeHidden();
    await expect(countries.locator(".barlist-row").first()).toContainText("India");

    await card.getByRole("button", { name: "Cities" }).click();
    await expect(card.getByRole("button", { name: "Cities" })).toHaveAttribute("aria-pressed", "true");
    await expect(countries).toBeHidden();
    // "City, Country", and India's busiest city is in the list.
    await expect(cities.locator(".barlist-row").first()).toContainText(/\S, \S/);
    await expect(cities.locator(".barlist-row", { hasText: "Mumbai, India" })).toBeVisible();

    await page.goto("/analytics?range=7d");
    await expect(page.locator('#locations [data-geo-pane="cities"]')).toBeVisible();
    await expect(page.locator("#locations").getByRole("button", { name: "Cities" })).toHaveAttribute("aria-pressed", "true");

    await page.locator("#locations").getByRole("button", { name: "Countries" }).click();
    await expect(page.locator('#locations [data-geo-pane="countries"]')).toBeVisible();
    expect(errors).toEqual([]);
  });
});

test.describe("domains", () => {
  test("connect, set a fallback, remove", async ({ page }) => {
    const errors = watchErrors(page);
    const host = testHost();
    await page.goto("/domains");
    await page.getByLabel("Domain to connect").fill(host);
    await page.getByRole("button", { name: "Connect" }).click();
    await expect(toast(page)).toContainText(`Connected ${host}`);
    const domainRow = page.locator("#domains-table tbody tr", { hasText: host });
    await expect(domainRow).toBeVisible();
    await expect(page.getByLabel("Domain to connect")).toHaveValue("");

    // The fallback input sits outside its row's <form> (linked by form=), which
    // is easy for htmx to miss.
    const fallback = domainRow.getByLabel(`Fallback for ${host}`);
    test.skip(!(await fallback.isVisible()), "fallback column is hidden on small screens");
    await fallback.fill("https://example.com/{path}");
    await domainRow.getByRole("button", { name: "Save" }).click();
    await expect(toast(page)).toContainText(`Saved ${host}`);
    await page.reload();
    await expect(page.locator("#domains-table tbody tr", { hasText: host }).getByLabel(`Fallback for ${host}`)).toHaveValue("https://example.com/{path}");

    await page.locator("#domains-table tbody tr", { hasText: host }).getByRole("button", { name: `Remove ${host}` }).click();
    await page.locator("dialog#confirm").getByRole("button", { name: "Remove" }).click();
    await expect(page.locator("#domains-table tbody tr", { hasText: host })).toHaveCount(0);
    expect(errors).toEqual([]);
  });
});

test.describe("import", () => {
  test("preview, then create exactly what was previewed", async ({ page }) => {
    const host = testHost();
    await page.goto("/import");
    await page.getByLabel("CSV rows").fill(`source,target\n${host}/one,https://example.com/1\n${host}/,https://example.com/slug\n${host}/one,https://example.com/dup`);
    await page.getByLabel(/Skip the live check/).check();
    await page.getByRole("button", { name: "Check rows" }).click();
    const result = page.locator("#import-result");
    await expect(result).toContainText("2 ready");
    await expect(result).toContainText("1 duplicate");
    const slug = await result.locator("tbody tr").nth(1).locator("td").nth(1).innerText();

    await result.getByRole("button", { name: "Create 2 links" }).click();
    await expect(result).toContainText("2 created");
    await expect(result.locator("tbody tr").nth(1).locator("td").nth(1)).toHaveText(slug);
    await expect(toast(page)).toContainText("Imported 2 links");
  });
});

test.describe("settings", () => {
  test("the default host preselects in the new-link drawer", async ({ page }) => {
    await page.goto("/settings");
    await page.getByLabel("Default host").selectOption("go.milind.dev");
    await page.getByRole("button", { name: "Save" }).click();
    await expect(toast(page)).toContainText("Settings saved");
    await page.goto("/links");
    await page.getByRole("button", { name: /New link/ }).first().click();
    await expect(page.locator("dialog#new-link select#f-host")).toHaveValue("go.milind.dev");
    // Put it back for the other tests.
    await page.goto("/settings");
    await page.getByLabel("Default host").selectOption("");
    await page.getByRole("button", { name: "Save" }).click();
    await expect(toast(page)).toContainText("Settings saved");
  });
});
