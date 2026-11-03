// The service worker. It keeps the static assets (CSS, JS, fonts, icons) in a
// cache so the installed app starts fast, and shows an offline page when the
// network is down. Pages and data are never cached: they come live from
// SQLite behind Authelia, and a cached page would go stale and outlive a
// sign-out.
//
// Registered by app.ts as /sw.js?v=<build version>, the same version that
// cache-busts /static/*. A new build means a new worker and a fresh cache.
// Compiled to web/static/sw.js by esbuild; `make check-ts` checks it with
// tsconfig.sw.json, which has the WebWorker types.

export {}; // a module, so `self` below can be retyped
declare const self: ServiceWorkerGlobalScope;

const version = new URL(self.location.href).searchParams.get("v") ?? "dev";
const cacheName = `signpost-${version}`;
const offlineURL = "/static/offline.html";

const precache = [
  `/static/app.css?v=${version}`,
  `/static/app.js?v=${version}`,
  `/static/htmx.min.js?v=${version}`,
  "/static/fonts/Geist-Variable.woff2",
  "/static/icons/icon.svg",
  "/static/icons/icon-192.png",
  offlineURL,
];

self.addEventListener("install", (e) => {
  e.waitUntil(
    caches
      .open(cacheName)
      .then((c) => c.addAll(precache))
      .then(() => self.skipWaiting()),
  );
});

self.addEventListener("activate", (e) => {
  e.waitUntil(
    caches
      .keys()
      .then((keys) => Promise.all(keys.filter((k) => k.startsWith("signpost-") && k !== cacheName).map((k) => caches.delete(k))))
      .then(() => self.clients.claim()),
  );
});

self.addEventListener("fetch", (e) => {
  const req = e.request;
  if (req.method !== "GET") return;
  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;

  if (url.pathname.startsWith("/static/")) {
    // Every /static/ URL is versioned or never changes, so cache-first is safe.
    e.respondWith(
      caches.open(cacheName).then(async (c) => {
        const hit = await c.match(req);
        if (hit) return hit;
        try {
          const res = await fetch(req);
          if (res.ok) c.put(req, res.clone());
          return res;
        } catch (err) {
          // Offline: any cached build of the file will do. This is how the
          // offline page, which links plain /static/app.css, gets styled.
          const any = await c.match(req, { ignoreSearch: true });
          if (any) return any;
          throw err;
        }
      }),
    );
    return;
  }

  if (req.mode === "navigate") {
    // Straight to the network, so Authelia's redirect to its login page still
    // works. Only a network failure gets the offline page.
    e.respondWith(
      fetch(req).catch(async () => (await caches.match(offlineURL)) ?? Response.error()),
    );
  }
  // Everything else (htmx under /ui/, /api, /export.csv) is left alone.
});
