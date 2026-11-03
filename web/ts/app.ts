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

function showToast(kind: ToastDetail["kind"], msg: string): void {
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
  box.append(el);
  setTimeout(
    () => {
      el.style.transition = "opacity .2s";
      el.style.opacity = "0";
      setTimeout(() => el.remove(), 200);
    },
    kind === "err" ? 6000 : 3500,
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

  const open = target.closest<HTMLElement>("[data-open]");
  if (open?.dataset.open) {
    const dlg = document.getElementById(open.dataset.open);
    if (dlg instanceof HTMLDialogElement) {
      dlg.showModal();
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

document.addEventListener("htmx:sendError", () => location.reload());
document.addEventListener("htmx:responseError", (e) => {
  const status = e.detail.xhr.status;
  if (status === 401 || status === 403) location.reload();
  else showToast("err", `Request failed (${status})`);
});

// Keep the viewer's time zone fresh for server-side bucketing.
document.cookie = `tz=${-new Date().getTimezoneOffset()};path=/;samesite=lax;max-age=31536000`;
