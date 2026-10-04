import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source = await readFile(
  new URL("./assets/sw.js", import.meta.url),
  "utf8",
);
function worker() {
  const handlers = new Map(),
    writes = [],
    deletes = [],
    events = [];
  const cache = {
    addAll: async (requests) => writes.push(...requests.map((r) => r.url)),
    put: async (key, value) => writes.push({ key, value }),
    match: async () => "offline shell",
  };
  const context = vm.createContext({
    URL,
    AbortController,
    setTimeout,
    clearTimeout,
    Request: class {
      constructor(url) {
        this.url = url;
      }
    },
    fetch: async () => ({ ok: true, clone: () => "fresh shell" }),
    caches: {
      open: async () => cache,
      keys: async () => [
        "navigatorr-shell-old",
        "navigatorr-shell-test",
        "other-app",
      ],
      delete: async (key) => deletes.push(key),
    },
    self: {
      location: { origin: "https://navigatorr.example" },
      addEventListener: (name, handler) => handlers.set(name, handler),
      skipWaiting: async () => events.push("skip"),
      clients: { claim: async () => events.push("claim") },
    },
  });
  vm.runInContext(source.replaceAll("__ASSET_VERSION__", "test"), context);
  return { handlers, writes, deletes, events, context, cache };
}
test("install caches only public shell and activation preserves other applications", async () => {
  const w = worker();
  let pending;
  w.handlers.get("install")({ waitUntil: (p) => (pending = p) });
  await pending;
  assert.deepEqual(w.writes, [
    "/",
    "/app.js",
    "/app.css",
    "/pwa.js",
    "/manifest.webmanifest",
    "/icon-192.png",
    "/icon-512.png",
    "/apple-touch-icon.png",
  ]);
  w.handlers.get("activate")({ waitUntil: (p) => (pending = p) });
  await pending;
  assert.deepEqual(w.deletes, ["navigatorr-shell-old"]);
  assert.deepEqual(w.events, ["skip", "claim"]);
});
test("worker never intercepts API, MCP, mutation or cross-origin traffic", () => {
  const w = worker();
  for (const [path, method] of [
    ["/api/maintenance/operations", "GET"],
    ["/api/maintenance/login", "POST"],
    ["/mcp", "POST"],
    ["/", "POST"],
    ["/app.js?private=1", "GET"],
    ["https://other.example/app.js", "GET"],
  ]) {
    w.handlers.get("fetch")({
      request: {
        url: new URL(path, "https://navigatorr.example").href,
        method,
      },
      respondWith: () => assert.fail(path),
    });
  }
  assert.equal(w.writes.length, 0);
});
test("public shell is network first, and opens from cache when the server is unreachable", async () => {
  const w = worker();
  let pending;
  const event = {
    request: { url: "https://navigatorr.example/", method: "GET" },
    respondWith: (p) => (pending = p),
  };
  w.handlers.get("fetch")(event);
  assert.equal((await pending).ok, true);
  assert.deepEqual(w.writes, [{ key: "/", value: "fresh shell" }]);
  w.context.fetch = async () => {
    throw new Error("offline");
  };
  w.handlers.get("fetch")(event);
  assert.equal(await pending, "offline shell");
  assert.equal(w.writes.length, 1);
  w.context.fetch = async () => ({ ok: false, status: 503 });
  w.handlers.get("fetch")(event);
  assert.equal(await pending, "offline shell");
  assert.equal(w.writes.length, 1);
});

test("storage failure cannot replace a successful online response with stale content", async () => {
  const w = worker();
  let pending;
  const event = {
    request: { url: "https://navigatorr.example/app.js", method: "GET" },
    respondWith: (p) => (pending = p),
  };
  w.cache.put = async () => {
    throw new Error("quota");
  };
  w.handlers.get("fetch")(event);
  assert.equal((await pending).ok, true);
  w.context.caches.open = async () => {
    throw new Error("storage unavailable");
  };
  w.handlers.get("fetch")(event);
  assert.equal((await pending).ok, true);
});

test("a stalled shell request aborts and falls back to cache", async () => {
  const w = worker();
  let expire, pending;
  w.context.setTimeout = (callback) => {
    expire = callback;
    return 1;
  };
  w.context.clearTimeout = () => {};
  w.context.fetch = async (request, { signal }) =>
    new Promise((resolve, reject) =>
      signal.addEventListener("abort", () => reject(new Error("timeout"))),
    );
  w.handlers.get("fetch")({
    request: { url: "https://navigatorr.example/", method: "GET" },
    respondWith: (p) => (pending = p),
  });
  // Allow caches.open to settle before triggering the deterministic timer.
  await new Promise((resolve) => setImmediate(resolve));
  expire();
  assert.equal(await pending, "offline shell");
});
