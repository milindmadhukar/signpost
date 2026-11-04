// The dashboard's small amount of client code. Everything that changes data
// goes through htmx to Go; this file only does what a server cannot: theme,
// clipboard, dialogs, toasts, selection and keyboard shortcuts. Handlers are
// delegated from document, so htmx swaps never need them re-bound.
//
// Compiled to web/static/app.js by esbuild at build time (see Makefile and
// Dockerfile); `make check-ts` type-checks it with tsc.

type ThemePref = "system" | "light" | "dark";

const $ = <T extends Element = HTMLElement>(sel: string, root: ParentNode = document): T | null =>
  root.querySelector<T>(sel);
const $$ = <T extends Element = HTMLElement>(sel: string, root: ParentNode = document): T[] =>
  Array.from(root.querySelectorAll<T>(sel));

// ---- Theme: system / light / dark, remembered per browser ----

const media = matchMedia("(prefers-color-scheme: dark)");

function themePref(): ThemePref {
  const p = localStorage.getItem("theme");
  return p === "light" || p === "dark" ? p : "system";
}

function applyTheme(pref: ThemePref): void {
  const dark = pref === "dark" || (pref === "system" && media.matches);
  document.documentElement.dataset.theme = dark ? "dark" : "light";
  document.documentElement.dataset.themePref = pref;
  // The installed app's title bar follows the page, not just the OS setting.
  $<HTMLMetaElement>('meta[name="theme-color"]')?.setAttribute("content", dark ? "#09090b" : "#fafafa");
  for (const b of $$("[data-theme-set]")) {
    b.setAttribute("aria-pressed", String(b.dataset.themeSet === pref));
  }
}

media.addEventListener("change", () => applyTheme(themePref()));
document.addEventListener("htmx:load", () => applyTheme(themePref()));
document.addEventListener("DOMContentLoaded", () => applyTheme(themePref()));

// ---- Toasts, raised by the server through HX-Trigger ----

const toastIcons: Record<ToastDetail["kind"], string> = {
  ok: '<svg class="icon mt-px text-emerald-600 dark:text-emerald-400" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><path d="m9 12 2 2 4-4"/></svg>',
  err: '<svg class="icon mt-px text-red-600 dark:text-red-400" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><line x1="12" x2="12" y1="8" y2="12"/><line x1="12" x2="12.01" y1="16" y2="16"/></svg>',
};

// The toast box is a manual popover, so it lives in the top layer. A modal
// dialog opened later would cover it, so it is re-shown (moved to the top
// of the stack) whenever a toast appears.
// While a modal dialog is open everything outside it is inert, a toast
// included, so its buttons could not be pressed. The box moves into the open
// dialog, and back to <body> when it closes.
function raiseToasts(): void {
  const box = $("#toasts");
  if (!box || !("showPopover" in box)) return;
  const modal = $$<HTMLDialogElement>("dialog[open]").pop();
  const home = modal ?? document.body;
  if (box.matches(":popover-open")) box.hidePopover();
  if (box.parentElement !== home) home.append(box);
  if (box.childElementCount > 0) box.showPopover();
}

document.addEventListener("close", () => raiseToasts(), true);

function showToast(kind: ToastDetail["kind"], msg: string, action?: { label: string; run: () => void }): void {
  const box = $("#toasts");
  if (!box) return;
  const el = document.createElement("div");
  el.className = "toast";
  el.setAttribute("role", kind === "err" ? "alert" : "status");
  el.innerHTML = toastIcons[kind] ?? toastIcons.ok;
  const text = document.createElement("div");
  text.className = "min-w-0 flex-1 break-words text-fg";
  text.textContent = msg; // never innerHTML: messages echo user input
  el.append(text);
  if (action) {
    const b = document.createElement("button");
    b.type = "button";
    b.className = "btn btn-sm -my-1 shrink-0";
    b.textContent = action.label;
    b.addEventListener("click", action.run);
    el.append(b);
  }
  box.append(el);
  raiseToasts();
  setTimeout(
    () => {
      el.style.transition = "opacity .2s";
      el.style.opacity = "0";
      setTimeout(() => {
        el.remove();
        raiseToasts();
      }, 200);
    },
    action ? 10000 : kind === "err" ? 6000 : 3500,
  );
}

document.addEventListener("toast", (e) => showToast(e.detail.kind, e.detail.msg));

// ---- Confirm: a real dialog instead of window.confirm ----

document.addEventListener("htmx:confirm", (e) => {
  const { question, elt, issueRequest } = e.detail;
  if (!question) return;
  e.preventDefault();
  const dlg = $<HTMLDialogElement>("#confirm");
  const text = dlg && $("#confirm-text", dlg);
  const ok = dlg && $("#confirm-ok", dlg);
  if (!dlg || !text || !ok) return;
  text.textContent = question;
  ok.textContent = elt.dataset.confirmLabel ?? "Delete";
  dlg.returnValue = "";
  dlg.addEventListener("close", () => {
    if (dlg.returnValue === "ok") issueRequest(true);
  }, { once: true });
  dlg.showModal();
  raiseToasts();
});

// ---- Clicks: copy, theme, dialogs, menus, mobile nav, table rows ----

function closeMenus(except?: EventTarget | null): void {
  for (const d of $$<HTMLDetailsElement>("details.menu[open]")) {
    if (!(except instanceof Node && d.contains(except))) d.open = false;
  }
}

document.addEventListener("click", (e) => {
  const target = e.target;
  if (!(target instanceof Element)) return;

  const copy = target.closest<HTMLElement>("[data-copy]");
  if (copy?.dataset.copy) {
    e.preventDefault();
    const value = copy.dataset.copy;
    navigator.clipboard
      .writeText(value)
      .then(() => showToast("ok", "Copied " + value.replace(/^https?:\/\//, "")))
      .catch(() => showToast("err", "Could not copy to the clipboard"));
    return;
  }

  const theme = target.closest<HTMLElement>("[data-theme-set]");
  if (theme) {
    const pref = theme.dataset.themeSet as ThemePref;
    localStorage.setItem("theme", pref);
    applyTheme(pref);
    return;
  }

  if (target.closest("[data-generate-slug]")) {
    const path = $<HTMLInputElement>("#f-path");
    if (path) {
      path.value = randomSlug();
      path.focus();
      path.select();
    }
    return;
  }

  // Following a sidebar link on a phone: close the drawer straight away,
  // rather than leave it over the page that loads.
  if (target.closest("aside a[href]")) document.body.removeAttribute("data-nav-open");

  const open = target.closest<HTMLElement>("[data-open]");
  if (open?.dataset.open) {
    const dlg = document.getElementById(open.dataset.open);
    if (dlg instanceof HTMLDialogElement) {
      dlg.showModal();
      raiseToasts();
      $<HTMLInputElement>("[autofocus], input:not([type=hidden])", dlg)?.focus();
    }
    return;
  }

  const close = target.closest("[data-close]");
  if (close) {
    close.closest("dialog")?.close();
    return;
  }

  // A click on the backdrop (outside the dialog box) closes it.
  if (target instanceof HTMLDialogElement && e instanceof MouseEvent) {
    const r = target.getBoundingClientRect();
    if (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom) target.close();
    return;
  }

  if (target.closest("[data-nav-toggle]")) {
    document.body.toggleAttribute("data-nav-open");
    return;
  }

  closeMenus(target);

  // A click on a table row (not on a control) follows the row's main link,
  // which hx-boost turns into an in-place page load.
  const row = target.closest("tr[data-row]");
  if (row && !target.closest("a, button, input, label, details, summary")) {
    $<HTMLAnchorElement>("a[data-row-link]", row)?.click();
  }
});

// ---- Short slugs ----

// Same alphabet as slugAlphabet in service.go: no 0/O or 1/l/I, so a slug
// read off a poster survives. Made here so Generate is instant; the server
// still refuses a path that is taken when the link is created.
const SLUG_ALPHABET = "23456789abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ";
const SLUG_LENGTH = 7;

function randomSlug(): string {
  const out: string[] = [];
  const buf = new Uint8Array(1);
  while (out.length < SLUG_LENGTH) {
    crypto.getRandomValues(buf);
    const b = buf[0] ?? 255;
    // 256 is not a multiple of 56: drop the top of the range to stay uniform.
    if (b < 224) out.push(SLUG_ALPHABET.charAt(b % SLUG_ALPHABET.length));
  }
  return out.join("");
}

// ---- Domain picker: "Other domain…" reveals a text field ----

document.addEventListener("change", (e) => {
  const sel = e.target;
  if (!(sel instanceof HTMLSelectElement) || !sel.matches("[data-host-select]")) return;
  const other = $<HTMLInputElement>("#f-host-other");
  if (!other) return;
  const typing = sel.value === "__other__";
  other.hidden = !typing;
  other.required = typing;
  if (typing) other.focus();
});

// ---- Loading feedback ----
// A thin bar at the top for anything that takes longer than a moment, and a
// dimmed page while a boosted navigation loads. Buttons show their own
// spinner through the .htmx-request class (see app.css).

let inflight = 0;
let progressTimer: number | undefined;

function setProgress(on: boolean): void {
  const bar = $("#progress");
  if (!bar) return;
  if (on) {
    bar.style.opacity = "1";
    bar.style.transform = "scaleX(0.7)";
  } else {
    bar.style.transform = "scaleX(1)";
    setTimeout(() => {
      bar.style.opacity = "0";
      bar.style.transform = "scaleX(0)";
    }, 200);
  }
}

document.addEventListener("htmx:beforeRequest", (e) => {
  inflight++;
  if (e.detail.boosted) document.body.setAttribute("data-loading", "");
  window.clearTimeout(progressTimer);
  progressTimer = window.setTimeout(() => setProgress(true), 120);
});

document.addEventListener("htmx:afterRequest", () => {
  inflight = Math.max(0, inflight - 1);
  if (inflight > 0) return;
  window.clearTimeout(progressTimer);
  document.body.removeAttribute("data-loading");
  setProgress(false);
});

document.addEventListener("htmx:beforeSwap", (e) => {
  if (e.detail.boosted) document.body.removeAttribute("data-nav-open");
});

// ---- Bulk selection on the links table ----

function syncSelection(): void {
  const boxes = $$<HTMLInputElement>("#links-table input[name=ids]");
  const picked = boxes.filter((b) => b.checked);
  const bar = $("#bulk-bar");
  if (bar) {
    bar.hidden = picked.length === 0;
    const count = $("#bulk-count", bar);
    if (count) count.textContent = `${picked.length} selected`;
  }
  const all = $<HTMLInputElement>("#select-all");
  if (all) {
    all.checked = picked.length > 0 && picked.length === boxes.length;
    all.indeterminate = picked.length > 0 && picked.length < boxes.length;
  }
  for (const b of boxes) b.closest("tr")?.classList.toggle("bg-accent-soft", b.checked);
}

document.addEventListener("change", (e) => {
  const t = e.target;
  if (!(t instanceof HTMLInputElement)) return;
  if (t.id === "select-all") {
    for (const b of $$<HTMLInputElement>("#links-table input[name=ids]")) b.checked = t.checked;
  }
  if (t.matches("#links-table input[type=checkbox]")) syncSelection();
});
document.addEventListener("htmx:afterSwap", syncSelection);

// ---- Drawer: close after a link is created ----

document.addEventListener("link-created", () => {
  const dlg = $<HTMLDialogElement>("#new-link");
  if (dlg?.open) dlg.close();
});

// ---- Keyboard: / search, n new link, Esc closes (native for dialogs) ----

document.addEventListener("keydown", (e) => {
  if (e.metaKey || e.ctrlKey || e.altKey) return;
  const typing = e.target instanceof Element ? e.target.closest<HTMLElement>("input, textarea, select, [contenteditable]") : null;
  if (e.key === "Escape") {
    closeMenus();
    if (typing instanceof HTMLInputElement && typing.name === "q") typing.blur();
    document.body.removeAttribute("data-nav-open");
    return;
  }
  if (typing || $("dialog[open]")) return;
  if (e.key === "/") {
    const search = $<HTMLInputElement>("input[name=q]");
    if (search) {
      e.preventDefault();
      search.focus();
      search.select();
    }
  } else if (e.key === "n") {
    const button = $("[data-open=new-link]");
    if (button) {
      e.preventDefault();
      button.click();
    }
  }
});

// ---- Session: when Authelia's session ends, htmx requests are bounced to
// the login page cross-origin and fail. Reload, so the browser follows the
// redirect and the user can sign in again. ----

document.addEventListener("htmx:sendError", () => {
  // Offline, a reload would only swap the page (and any half-filled form)
  // for the offline page, so just say so.
  if (!navigator.onLine) {
    showToast("err", "You're offline. Try again once you're connected.");
    return;
  }
  showToast("err", "Could not reach signpost. If you were away a while, your sign-in may have expired.", {
    label: "Reload",
    run: () => location.reload(),
  });
});
document.addEventListener("htmx:responseError", (e) => {
  const status = e.detail.xhr.status;
  if (status === 401) {
    showToast("err", "Your sign-in has expired.", { label: "Sign in", run: () => location.reload() });
  } else {
    showToast("err", `Something went wrong (${status}). Nothing was changed.`, { label: "Reload", run: () => location.reload() });
  }
});

// ---- World map: a tooltip per country, following the pointer. A mouse
// shows it on hover; a tap shows it until the next tap elsewhere. ----

function geoCountry(t: EventTarget | null): SVGPathElement | null {
  return t instanceof Element ? t.closest<SVGPathElement>(".geo path[data-name]") : null;
}

function hideGeoTips(): void {
  for (const tip of $$("[data-geo] .geo-tip")) tip.hidden = true;
  for (const p of $$<SVGPathElement>(".geo path.on")) p.classList.remove("on");
}

function showGeoTip(path: SVGPathElement, e: PointerEvent): void {
  const box = path.closest<HTMLElement>("[data-geo]");
  const tip = box && $(".geo-tip", box);
  if (!box || !tip) return;
  if (!path.classList.contains("on")) {
    hideGeoTips();
    path.classList.add("on");
    // Last in the SVG paints on top, so the outline is not hidden under a neighbour.
    path.parentNode?.append(path);
  }
  const n = Number(path.dataset.n ?? 0);
  const name = document.createElement("span");
  name.className = "font-semibold";
  name.textContent = path.dataset.name ?? "";
  const rest = document.createElement("span");
  rest.className = "opacity-70";
  rest.textContent = n > 0 ? ` · ${n.toLocaleString("en-US")} click${n === 1 ? "" : "s"} · ${path.dataset.pct ?? ""}` : " · No clicks";
  tip.replaceChildren(name, rest);
  tip.hidden = false;

  // Centre on the pointer, kept inside the map; below it when near the top.
  const r = box.getBoundingClientRect();
  const half = tip.offsetWidth / 2;
  const x = Math.min(Math.max(e.clientX - r.left, half), Math.max(r.width - half, half));
  const y = e.clientY - r.top;
  tip.style.left = `${x}px`;
  tip.style.top = `${y}px`;
  tip.style.transform = y < tip.offsetHeight + 14 ? "translate(-50%, 14px)" : "";
}

document.addEventListener("pointermove", (e) => {
  if (e.pointerType !== "mouse") return;
  const path = geoCountry(e.target);
  if (path) showGeoTip(path, e);
  else if ($(".geo path.on")) hideGeoTips();
});
document.addEventListener("pointerdown", (e) => {
  const path = geoCountry(e.target);
  if (path) showGeoTip(path, e);
  else hideGeoTips();
});
document.addEventListener("htmx:beforeSwap", hideGeoTips);

// Keep the viewer's time zone fresh for server-side bucketing.
document.cookie = `tz=${-new Date().getTimezoneOffset()};path=/;samesite=lax;max-age=31536000`;

// ---- Installed app: service worker, home-screen shortcut, share sheet ----

// The worker is versioned like /static/*, so each build installs afresh.
if ("serviceWorker" in navigator) {
  const v = document.documentElement.dataset.version ?? "";
  navigator.serviceWorker.register(`/sw.js?v=${encodeURIComponent(v)}`, { scope: "/" }).catch(() => {});
}

// The manifest's "New link" shortcut opens /links?new=1, and sharing a page
// to signpost opens /links?title=…&text=…&url=…. Either way, open the new-link
// drawer, with the shared address as the target. Android tends to put the
// address in `text`, after the page title, so take the first URL found.
function openFromLaunch(): void {
  if (location.pathname !== "/links") return;
  const q = new URLSearchParams(location.search);
  const keys = ["new", "url", "text", "title"];
  if (!keys.some((k) => q.has(k))) return;
  const shared = ["url", "text", "title"]
    .map((k) => q.get(k)?.match(/https?:\/\/\S+/)?.[0])
    .find((u) => u);
  for (const k of keys) q.delete(k);
  const rest = q.toString();
  history.replaceState(history.state, "", location.pathname + (rest ? `?${rest}` : "") + location.hash);

  $("[data-open=new-link]")?.click();
  const target = $<HTMLInputElement>("#f-target");
  if (shared && target) {
    target.value = shared;
    target.dispatchEvent(new Event("input", { bubbles: true }));
  }
}

document.addEventListener("DOMContentLoaded", openFromLaunch);
