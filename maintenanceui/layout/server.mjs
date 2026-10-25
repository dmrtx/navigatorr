// Serves the real console assets and a scripted API, so the layout checks run
// the same HTML, CSS and JS the browser gets. Nothing here talks to Navigatorr.
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { extname, join, normalize } from "node:path";
import { fileURLToPath } from "node:url";

const assets = fileURLToPath(new URL("../assets/", import.meta.url));
const types = { ".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8", ".png": "image/png", ".webmanifest": "application/manifest+json" };

export const bootstrap = {
  roots: ["/media/TV", "/media/Movies"],
  services: [{ name: "sonarr" }, { name: "radarr" }],
  tools: ["recipe_list", "action_run", "action_resume", "action_cancel", "action_retry"],
  allow_destructive: true,
  transcode_enabled: true,
};

// `world` is what the API answers with; a check changes it between polls.
export async function startServer(world) {
  const server = createServer(async (req, res) => {
    const url = new URL(req.url, "http://x");
    const json = (body, status = 200) => {
      res.writeHead(status, { "Content-Type": "application/json" });
      res.end(JSON.stringify(body));
    };
    if (url.pathname.startsWith("/api/maintenance/")) {
      const name = url.pathname.slice("/api/maintenance/".length);
      const reply = world.api?.(name, url.searchParams, req.method) ?? defaults(name, url.searchParams, world);
      return json(reply ?? {});
    }
    const file = url.pathname === "/" ? "index.html" : normalize(url.pathname).replace(/^\/+/, "");
    try {
      const data = await readFile(join(assets, file));
      res.writeHead(200, { "Content-Type": types[extname(file)] || "application/octet-stream" });
      res.end(data);
    } catch {
      res.writeHead(404);
      res.end("not found");
    }
  });
  await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
  return { server, origin: `http://127.0.0.1:${server.address().port}`, close: () => new Promise(resolve => server.close(resolve)) };
}

function defaults(name, params, world) {
  switch (name) {
    case "auth-info": return { auth_mode: "cloudflare_access", session: "cloudflare_access" };
    case "bootstrap": return bootstrap;
    case "workers": return { nodes: [], workers: [] };
    case "worker-activity": return { nodes: [], active: [], slots: {} };
    case "keep-languages": return { languages: ["spa", "eng", "jpn"] };
    case "operations": {
      const id = params.get("id");
      if (id) return { jobs: world.jobs.filter(job => job.id === id), total: 1 };
      return { jobs: world.jobs, total: world.jobs.length, has_more: false, active_count: 0, decision_count: 0, savings: {} };
    }
    case "batch-items": {
      const items = world.items?.[params.get("id")] || [];
      const offset = Number(params.get("offset") || 0), limit = Number(params.get("limit") || 100);
      return { items: items.slice(offset, offset + limit), total: items.length, has_more: offset + limit < items.length };
    }
    case "logs": return { stdout_tail: "frame= 1024 fps=61 q=28.0 size=  2048kB", stderr_tail: "" };
    default: return {};
  }
}
