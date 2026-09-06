# Navigatorr

An MCP (Model Context Protocol) server that gives AI assistants like Claude direct access to your *arr media stack and torrent clients. Browse API documentation, make authenticated calls, and manage downloads — all through natural language.

## What It Does

Navigatorr acts as a bridge between AI coding assistants and your self-hosted media services. Instead of manually navigating web UIs or crafting API calls, you describe what you want and the AI handles the rest through Navigatorr's MCP tools.

**Supported services:**
- **Sonarr** — TV show management
- **Radarr** — Movie management
- **Lidarr** — Music management
- **Readarr** — Book management
- **Chaptarr** — Ebook and audiobook management
- **Prowlarr** — Indexer management
- **Profilarr** — Quality profile and custom format management
- **Bazarr** — Subtitle management
- **Seerr** — Request management (formerly Jellyseerr)
- **Overseerr/Jellyseerr** — Request management (legacy names, still supported)
- **Audiobookshelf** — Audiobook management
- **Transmission** — Torrent client
- **qBittorrent** — Torrent client
- **SABnzbd** — Usenet downloader

## Architecture

```
Claude Code / MCP Client
        │
        ▼
   ┌─────────────┐
   │ Navigatorr  │  MCP Server (stdio transport)
   │              │
   │  ┌────────┐  │
   │  │ Tools  │  │  16 MCP tools exposed
   │  └───┬────┘  │
   │      │       │
   │  ┌───▼────┐  │
   │  │Registry│  │  Service registry + auth
   │  └───┬────┘  │
   │      │       │
   │  ┌───▼────┐  │
   │  │OpenAPI │  │  Spec parsing + caching
   │  │ Store  │  │
   │  └────────┘  │
   └──────┬───────┘
          │
          ▼
   *arr services + Transmission/qBittorrent
```

### Package Structure

| Package | Purpose |
|---------|---------|
| `main` | Entry point, config loading, MCP server setup |
| `config` | YAML config parsing with service defaults |
| `arrservice` | Service registry, HTTP client, auth strategies (header/query/basic) |
| `openapi` | OpenAPI spec fetching, parsing, caching, and search |
| `tools` | MCP tool registration and handlers |
| `transmission` | Transmission RPC client |
| `qbit` | qBittorrent Web API client |
| `store` | SQLite persistence: preferences, maintenance jobs, decisions, checks, audit log, blocklist |
| `maint` | Deterministic ranking, filename safety, language and oversize heuristics |
| `mediainspect` | Real-file inspection via ffprobe (no shell, fixed argv) plus sidecar detection |
| `fsop` | Root-confined filesystem ops (stat, list, hash, move, delete) |
| `internal` | Shared logging utilities |

### How It Works

1. **Config Loading** — Reads `~/.config/navigatorr/config.yaml` to discover services, API keys, and Transmission settings. Applies sensible defaults for known service types (API versions, auth methods, OpenAPI spec URLs).

2. **Service Registry** — Each configured service gets an authenticated HTTP client with the appropriate auth strategy (API key header, query parameter, or basic auth). The registry provides lookup by name.

3. **OpenAPI Spec Store** — On startup, fetches and parses OpenAPI specs from each service's official GitHub repo. Specs are cached to disk (`~/.cache/navigatorr/`) and indexed for fast endpoint lookup and full-text search.

4. **Tool Registration** — Four categories of tools are registered with the MCP server:
   - **API Documentation tools** — browse and search service endpoints without making calls
   - **API Call tool** — make authenticated requests with field selection, filtering, and pagination. Includes a configurable response size guard that catches oversized responses before they eat the LLM's context window, and optional destructive request protection that blocks DELETE calls unless explicitly enabled.
   - **Transmission tools** — manage torrents (list, add, start, stop, remove, verify, free space)
   - **qBittorrent tools** — manage torrents (list, add, pause, resume, delete, transfer stats)
   - **SABnzbd tools** — manage Usenet downloads (queue, history, add, pause, resume, delete, reprioritise, move)

5. **Stdio Transport** — Communicates with the MCP client over stdin/stdout using JSON-RPC, making it compatible with any MCP host (Claude Code, Cursor, etc.).

## MCP Tools

### API Documentation

| Tool | Description |
|------|-------------|
| `list_services` | List all configured services with URLs and connection status |
| `list_endpoints` | Browse API endpoints for a service, filterable by tag or HTTP method |
| `get_endpoint_details` | Full endpoint info including parameters, request body, and response schemas |
| `search_api` | Full-text search across all API specs |
| `refresh_api_specs` | Re-fetch OpenAPI specs from upstream |

### API Calls

| Tool | Description |
|------|-------------|
| `call_api` | Make authenticated API calls to any service. Supports field selection (including nested array drilling like `records.title`), filtering (`field:op:value`), and result limiting. Includes a response size guard and optional DELETE protection. |

### Transmission

| Tool | Description |
|------|-------------|
| `transmission_list_torrents` | List all torrents with status, progress, and speeds |
| `transmission_add_torrent` | Add a torrent by magnet link or URL |
| `transmission_manage_torrent` | Start, stop, remove, or verify torrents |
| `transmission_free_space` | Check available disk space |

### qBittorrent

| Tool | Description |
|------|-------------|
| `qbit_list_torrents` | List all torrents with status, progress, and speeds |
| `qbit_add_torrent` | Add a torrent by magnet link or URL |
| `qbit_manage_torrent` | Pause, resume, delete, or delete with files |
| `qbit_transfer_info` | Global transfer speed and statistics |

### SABnzbd

SABnzbd has no OpenAPI spec and dispatches everything from a `mode` query parameter on a single endpoint, so it gets its own tools rather than going through `call_api`.

| Tool | Description |
|------|-------------|
| `sabnzbd_list_queue` | Active downloads with status, progress, and speed |
| `sabnzbd_history` | Finished and failed downloads, filterable by category, search term, or failures only |
| `sabnzbd_add_nzb` | Queue an NZB by URL, with optional name, category, and priority |
| `sabnzbd_manage_item` | Pause, resume, delete, reprioritise, or move a job by `nzo_id` |
| `sabnzbd_status` | Version, speed, disk space, paused state, and warning count |

Deleting is covered by `allow_destructive`. SABnzbd deletes are GET requests carrying `name=delete`, so the `call_api` DELETE guard does not apply to them and these tools check the setting themselves.

### Request Queue

A holding area for media requests that arrive while no agent is running. Something outside navigatorr — a phone shortcut, a webhook, the iMessage bridge in `scripts/` — posts free-form text over HTTP, and the next agent session drains it.

The queue stores the text verbatim rather than parsing it. Working out which show "boston legal" means, which quality profile fits, and whether it duplicates something already in the library is judgment work for the agent, not for a parser.

| Tool | Description |
|------|-------------|
| `queue_list` | List requests, defaulting to pending. Reports counts for the other statuses so a backlog parked in `claimed` is visible. |
| `queue_claim` | Claim a pending request before working it, so two agents do not double-add it |
| `queue_resolve` | Close a request as `done` or `failed` with a note describing what happened |
| `queue_release` | Return a claimed request to pending when an agent gives up without resolving |

Requests move `pending → claimed → done`/`failed`, and the transitions are enforced: an item cannot be claimed twice, resolved twice, or released unless it is currently claimed. Releasing a finished request would otherwise put completed work back in the queue for an agent to action a second time.

```
POST /request   {"text": "boston legal", "source": "imessage"}   -> 201
GET  /queue?status=pending                                       -> 200
GET  /healthz                                                    -> 200 (no auth)
```

**The HTTP endpoint is optional and requires a token.** The MCP tools work with `listen` unset, which keeps the queue agent-only with nothing listening. When `listen` is set, `token` is required and navigatorr refuses to start without one — text in this queue is later read and acted on by an agent holding write credentials to every configured service, so an unauthenticated endpoint is a way to drive that agent, not just a way to add spam. Prefer binding loopback and reaching it through a tunnel or a reverse proxy that terminates TLS; the bearer token crosses the network in clear text.

The queue file is held under an advisory lock for the life of the process. MCP servers are spawned per client, so without one, navigatorr running under two clients at once would give two processes independent copies of the same file and each would overwrite the other's requests.

### Maintenance Agent (persistent)

Beyond the request queue, Navigatorr keeps a **separate SQLite-backed maintenance state**: user preferences, a structured maintenance job queue, release-decision history, real-file inspections, an audit log and a release blocklist. It defaults to `~/.cache/navigatorr/navigatorr.db` (configurable via `database.path`) with schema versioning, WAL mode and indexes — no Redis, Postgres or vector DB required.

| Tool group | Tools |
|------------|-------|
| Preferences | `memory_set`, `memory_get`, `memory_list`, `memory_search`, `memory_delete` (scoped: `global`, `anime`, `movies`, `project:<name>`, `media:<service>:<id>`; `ttl_seconds` for expiring facts) |
| Maintenance jobs | `maintenance_add` (idempotent), `maintenance_list`, `maintenance_next`, `maintenance_get`, `maintenance_update`, `maintenance_claim`, `maintenance_release`, `maintenance_resolve`, `maintenance_reopen` (blocked/failed → researching/pending) |
| Decisions | `decision_record`, `decision_list` (why-did-we-pick-this history) |
| Inspection | `inspect_media` (ffprobe + sidecar subs + `*arr` fallback), `qbit_list_files` (torrent-content safety gate), `scan_dangerous_files` |
| Ranking | `rank_releases` (deterministic scoring: codec, group, subs, seeders, size reduction) |
| Workflow | `safe_replace` (step machine), `cleanup_imported_downloads` (evidence-gated), `scan_library_issues` (dry-run by default), `get_context` (compact LLM briefing) |
| Blocklist | `block_release`, `block_list` |
| Scoped filesystem | `fs_stat`, `fs_list`, `fs_hash`, `fs_safe_move`, `fs_safe_delete` (all confined to `allowed_read_roots` / `allowed_write_roots`, symlinks resolved) |

Jobs move `pending → researching → candidate_found → downloading → downloaded → verifying → importing → replacing → done` (plus `blocked`/`failed`); illegal jumps are rejected, and `done` is only reachable from `replacing` — i.e. after verification **and** a confirmed Sonarr/Radarr import. The original file is never deleted on 100% download alone.

**Safe-replacement example (Fate/strange Fake, 21 GB → Judas 6 GB):**

```
maintenance_add  media_type=series service=sonarr media_id=100
                 title="Fate/strange Fake" issue_type=oversized
safe_replace     id=1 step=plan
rank_releases    media_type=anime current_size=21000000000 candidates=[...]
safe_replace     id=1 step=select release_guid=<judas> title="..." size=6000000000 seeders=100 reasons=[multi_subs,hevc_10bit]
safe_replace     id=1 step=add_torrent url="magnet:?xt=urn:btih:..."
qbit_list_files  hash=<hash>                          # safety gate: rejects *.mkv.exe
safe_replace     id=1 step=torrent_check               # blocked+blocklisted if dangerous
inspect_media    path=/downloads/...                   # real audio/subs check
safe_replace     id=1 step=verify complete=true audio_langs=Japanese sub_langs=eng,spa
# ... trigger + confirm the Sonarr import ...
safe_replace     id=1 step=import_confirm new_file_id=99
safe_replace     id=1 step=delete_original via=arr confirm=true   # only now
safe_replace     id=1 step=finish notes="Judas 1080p HEVC live"
```

**After a restart:** the jobs, preferences and decisions are still in the database. Call `maintenance_list` (or `maintenance_next`) and `get_context scope=anime` to resume — every `safe_replace` step is idempotent, so re-issuing the current step continues instead of duplicating work.

**Inspecting preferences:** `memory_get scope=anime` shows the anime rules; stored values override the `maintenance:` config defaults. Temporary facts (e.g. seeder counts) use `memory_set ... ttl_seconds=3600` and are never returned once expired.

## Setup

### Prerequisites

- Go 1.25+
- Running *arr services with API keys
- (Optional) Transmission and/or qBittorrent torrent client
- (Optional) SABnzbd for Usenet

### Option 1: Build from Source

```bash
go build -o navigatorr .
```

### Option 2: Docker

```bash
docker build -t navigatorr .
```

### Configure

Copy the example config and fill in your values:

```bash
mkdir -p ~/.config/navigatorr
cp config.yaml.example ~/.config/navigatorr/config.yaml
```

Edit `~/.config/navigatorr/config.yaml` with your service URLs and API keys. You can find API keys in each service's Settings > General page.

**Core Global Settings:**

| Setting | Default | Description |
|---------|---------|-------------|
| `allow_destructive` | `false` | Central safety gate (MUST be a top-level YAML key). When false, blocks all DELETE requests through `call_api`, prevents destructive torrent/usenet removals (`qbit_manage_torrent`, `transmission_manage_torrent`, `sabnzbd_manage_item`), blocks `fs_safe_delete`, and halts the final replacement step in `safe_replace`. Note: Navigatorr does not read environment variables; configure this exclusively in `config.yaml`. |
| `max_response_size_kb` | `50` | Response size guard threshold in KB. API responses exceeding this are rejected with a hint to use field selection/filtering instead of consuming the LLM's context window. |
| `concurrency.max_api_simultaneous` | `3` | Maximum simultaneous upstream HTTP calls to protect *arr services from being overwhelmed. |
| `concurrency.max_inspect_simultaneous` | `2` | Maximum concurrent ffprobe media inspections to protect NAS disk I/O and CPU. |

> ℹ️ **Container Deployments & Path Mapping:** Paths configured under `media.allowed_read_roots` and `media.allowed_write_roots` must match paths **inside the Docker container**, not on the host. See [DOCKER.md](DOCKER.md) for full Docker Compose recipes, path mapping diagrams, and the safety permissions matrix.

### Connect to Claude Code

**Using the binary directly:**

```json
{
  "mcpServers": {
    "navigatorr": {
      "type": "stdio",
      "command": "/path/to/navigatorr",
      "args": []
    }
  }
}
```

**Using Docker:**

```json
{
  "mcpServers": {
    "navigatorr": {
      "type": "stdio",
      "command": "docker",
      "args": [
        "run", "--rm", "-i",
        "-v", "~/.config/navigatorr/config.yaml:/root/.config/navigatorr/config.yaml:ro",
        "--network", "host",
        "navigatorr"
      ]
    }
  }
}
```

> **Note:** `--network host` is used so the container can reach your *arr services on the local network. If your services are on a remote host, you can use the default bridge network instead. The `-i` flag is required for stdio transport. Do not use `-t` as it interferes with the JSON-RPC communication.

**Custom config path:**

```json
{
  "mcpServers": {
    "navigatorr": {
      "type": "stdio",
      "command": "/path/to/navigatorr",
      "args": ["-config", "/path/to/config.yaml"]
    }
  }
}
```

## Usage Examples

Once connected, you can ask Claude things like:

- "What TV shows do I have in Sonarr?"
- "Search for a new series and add it"
- "Show me all movies missing from Radarr"
- "What torrents are currently downloading?"
- "Add this magnet link to qBittorrent"
- "How much free disk space do I have?"
- "Delete a series and re-add it with a different quality profile"
- "Unmonitor all movies in a franchise except the recent ones"

The AI uses Navigatorr's tools behind the scenes to browse API docs, discover the right endpoints, and make authenticated calls on your behalf.

### Demo Walkthrough

**1. Discover your services**
```
> "What services do I have?"
→ Tool: list_services
```
```
sonarr    → http://your-server:8989 (235 endpoints)
radarr    → http://your-server:7878 (238 endpoints)
lidarr    → http://your-server:8686 (236 endpoints)
prowlarr  → http://your-server:9696 (129 endpoints)
seerr     → http://your-server:5055 (212 endpoints)
...
```

**2. Search API endpoints without reading docs**
```
> "How do I manage quality profiles in Sonarr?"
→ Tool: search_api → query: "quality", service: "sonarr"
```
Returns 11 matching endpoints — GET, POST, PUT, DELETE for quality profiles and definitions. No digging through API docs.

**3. Browse endpoints by category**
```
> "What can I do with series?"
→ Tool: list_endpoints → service: "sonarr", tag: "Series"
```
```
GET    /api/v3/series       — List all series
POST   /api/v3/series       — Add a series
GET    /api/v3/series/{id}  — Get series details
PUT    /api/v3/series/{id}  — Update a series
DELETE /api/v3/series/{id}  — Delete a series
```

**4. Make authenticated API calls**
```
> "Show me all my TV shows"
→ Tool: call_api → service: "sonarr", path: "/series"
```
Returns full JSON with all series, episodes, and quality info. Handles auth headers, API versioning, and URL construction automatically. Supports field selection, filtering, and result limiting.

**5. Manage torrents**
```
> "What's downloading right now?"
→ Tool: transmission_list_torrents
```
```
Some.Show.S01E01.720p → downloading (45.2%)
Another.Show.S03.Pack → seeding (100%)
```

**6. Chain it all together**

The real power is that Claude chains tools automatically. Say:

> "Delete a series and re-add it with a different quality profile"

Claude will:
1. `call_api` GET /series → find the series ID
2. `call_api` DELETE /series/{id} with deleteFiles=true
3. `search_api` → discover the quality profile endpoints
4. `call_api` GET /qualityprofile → list available profiles
5. `call_api` POST /series → re-add with the new profile
6. `call_api` POST /command → trigger a search

All from one sentence.

## Dependencies

| Dependency | Purpose |
|------------|---------|
| [mcp-go](https://github.com/mark3labs/mcp-go) | MCP server framework |
| [kin-openapi](https://github.com/getkin/kin-openapi) | OpenAPI 3.x spec parsing |
| [yaml.v3](https://gopkg.in/yaml.v3) | YAML config parsing |
| [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) | Pure-Go SQLite driver (keeps the `CGO_ENABLED=0` Docker build) |

## Built With

Code intelligence powered by [CartoGopher](https://cartogopher.com)

## License

MIT
