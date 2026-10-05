const CACHE = "navigatorr-shell-__ASSET_VERSION__";
const SHELL = [
  "/",
  "/app.js",
  "/app.css",
  "/pwa.js",
  "/manifest.webmanifest",
  "/icon-192.png",
  "/icon-512.png",
  "/apple-touch-icon.png",
];

self.addEventListener("install", (event) => {
  event.waitUntil(
    (async () => {
      const cache = await caches.open(CACHE);
      await cache.addAll(
        SHELL.map((path) => new Request(path, { cache: "reload" })),
      );
      await self.skipWaiting();
    })(),
  );
});
self.addEventListener("activate", (event) => {
  event.waitUntil(
    (async () => {
      for (const key of await caches.keys()) {
        if (key.startsWith("navigatorr-shell-") && key !== CACHE)
          await caches.delete(key);
      }
      await self.clients.claim();
    })(),
  );
});
self.addEventListener("fetch", (event) => {
  const request = event.request;
  const url = new URL(request.url);
  // No API, MCP, login, cookies, job history or mutation is ever cached/replayed.
  if (
    request.method !== "GET" ||
    url.origin !== self.location.origin ||
    url.search ||
    !SHELL.includes(url.pathname)
  )
    return;
  event.respondWith(
    (async () => {
      const cache = await caches.open(CACHE).catch(() => null);
      const controller = new AbortController();
      const timeout = setTimeout(() => controller.abort(), 5000);
      try {
        const response = await fetch(request, { signal: controller.signal });
        // A reachable reverse proxy can still report an unavailable backend.
        if (response.status >= 500) {
          const cached = await cache?.match(url.pathname);
          if (cached) return cached;
        }
        if (response.ok && cache)
          await cache.put(url.pathname, response.clone()).catch(() => {});
        return response;
      } catch (error) {
        const cached = await cache?.match(url.pathname);
        if (cached) return cached;
        throw error;
      } finally {
        clearTimeout(timeout);
      }
    })(),
  );
});
