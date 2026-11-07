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
      // A Review action moves its row out of To do, as the real server does.
      if (req.method === "POST" && name === "maintenance-queue" && world.api?.(name, url.searchParams, req.method) === undefined) {
        let body = "";
        for await (const chunk of req) body += chunk;
        try {
          const { id, action } = JSON.parse(body || "{}");
          world.reviewMoved ||= new Map();
          if (action === "restore") world.reviewMoved.delete(id);
          else if (action) world.reviewMoved.set(id, action === "dismiss" ? "dismissed" : "processed");
        } catch {}
        return json({ ok: true });
      }
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
    case "preferences": return { replace_when_done_default: false };
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
    case "maintenance-queue": {
      const moved = world.reviewMoved || new Map();
      const wanted = params.get("state") || "open";
      const rows = (world.review || reviewRows).filter(r => (moved.get(r.id) || "open") === wanted);
      const counts = { open: rows.length, processed: 3, dismissed: 1 };
      // An older server ignores search, sort and paging and answers everything.
      if (world.reviewLegacy) return { items: rows, total: rows.length, counts };
      const fold = text => String(text ?? "").normalize("NFD").replace(/[\u0300-\u036f]/g, "").toLowerCase();
      const q = fold(params.get("q") || "");
      const kind = params.get("issue_type") || "";
      let found = rows.filter(r => (!kind || r.issue_type === kind) && (!q || fold([r.title, r.year, r.folder, r.notes].join(" ")).includes(q)));
      const by = { size: (a, b) => (b.size_bytes || 0) - (a.size_bytes || 0), newest: (a, b) => String(b.created_at).localeCompare(String(a.created_at)), title: (a, b) => String(a.title).localeCompare(String(b.title)), priority: (a, b) => (b.priority || 0) - (a.priority || 0) }[params.get("sort") || "priority"];
      if (by) found = [...found].sort(by);
      const offset = Math.max(0, Number(params.get("offset") || 0)), limit = Math.min(1000, Math.max(1, Number(params.get("limit") || 200)));
      // A paging server: one page, where it starts, how many match, and whether that count stopped short.
      return { items: found.slice(offset, offset + limit), total: found.length, offset, truncated: offset + limit < found.length, total_capped: false, counts };
    }
    case "logs": return { stdout_tail: "frame= 1024 fps=61 q=28.0 size=  2048kB", stderr_tail: "" };
    default: return {};
  }
}

// The Review tab's entries: a movie in a browsable folder, a long one, a series, one outside the roots.
export const reviewRows = [
  { id: 55, title: "The First Slam Dunk", year: 2022, service: "radarr", media_type: "movie", media_id: "1202", issue_type: "oversized", priority: 30, status: "pending", state: "open", can_restore: false, size_bytes: 59651572940, notes: "Candidato a transcodificar (propuesto, NO iniciado). HEVC 2160p Remux, Dolby Vision HDR10, TrueHD Atmos, 64 Mbps, 125 min. Modo compact sugerido. Ahorro estimado ~52-57 GB. Conservar original hasta aprobar promocion.", folder: "/media/Movies/The First Slam Dunk (2022)", in_files: true, created_at: "2026-11-01T18:06:38Z", updated_at: "2026-11-01T18:06:38Z" },
  { id: 61, title: "Psycho-Pass: Sinners of the System - Case.1 Crime and Punishment With A Very Long Subtitle", year: 2019, service: "radarr", media_type: "movie", media_id: "506", issue_type: "oversized", priority: 27, status: "downloading", state: "open", size_bytes: 14132479759, notes: "Candidato a transcodificar.", folder: "/media/Movies/Psycho-Pass: Sinners of the System - Case.1 Crime and Punishment (2019)", in_files: true, created_at: "2026-11-01T18:07:14Z", updated_at: "2026-11-01T18:07:14Z" },
  { id: 70, title: "The Messiah", year: 2020, service: "sonarr", media_type: "series", media_id: "198", issue_type: "oversized", priority: 29, status: "pending", state: "open", size_bytes: 63511274131, notes: "7 episodios h265 2160p WEBDL.", folder: "/media/TV/The Messiah", in_files: true, created_at: "2026-11-01T18:12:20Z", updated_at: "2026-11-01T18:12:20Z" },
  { id: 80, title: "Elsewhere", service: "radarr", media_type: "movie", media_id: "8", issue_type: "missing_accessible_language", priority: 20, status: "pending", state: "open", notes: "", folder: "/other/Elsewhere", in_files: false, created_at: "2026-11-01T18:12:20Z", updated_at: "2026-11-01T18:12:20Z" },
  { id: 81, title: "Not in Radarr", service: "radarr", media_type: "movie", media_id: "99", issue_type: "oversized", priority: 10, status: "pending", state: "open", size_bytes: 1000, notes: "", in_files: false, created_at: "2026-11-01T18:12:20Z", updated_at: "2026-11-01T18:12:20Z" },
];
