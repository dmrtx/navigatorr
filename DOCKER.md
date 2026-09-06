# Docker deployment

This fork publishes a multi-architecture Navigatorr image to GitHub Container Registry (GHCR):

```text
ghcr.io/dmrtx/navigatorr:latest
```

The GitHub Actions workflow publishes `linux/amd64` and `linux/arm64` images on every push to `main`, on `v*` tags, and when run manually. It also publishes a `sha-*` tag for each build.

## Important: Navigatorr uses MCP stdio

Navigatorr is an MCP stdio server, not a long-running HTTP MCP server. The intended deployment is for the MCP host (for example `tunnel-client`) to start the Docker container as its MCP command and keep stdin/stdout attached.

Do not run it as a normal detached Portainer service unless you are intentionally using only the optional request queue endpoint. Without an MCP client attached to stdio, the main MCP transport is not usable.

## Configuration

Create the config on the Docker host:

```bash
mkdir -p /home/david/.config/navigatorr
cp config.yaml.example /home/david/.config/navigatorr/config.yaml
chmod 600 /home/david/.config/navigatorr/config.yaml
```

For qBittorrent, include:

```yaml
qbittorrent:
  url: "http://localhost:8080"
  username: "your-qbittorrent-username"
  password: "your-qbittorrent-password"
```

When `--network host` is used on Linux, `localhost` inside Navigatorr refers to the Docker host, which is convenient when Sonarr, Radarr, Prowlarr, Bazarr, qBittorrent, and the other services publish their ports on that host.

## Deployment & Execution Modes

Navigatorr supports two distinct operational models depending on your infrastructure topology:

### Mode A: Updating a Persistent Background Service (Compose / Portainer)

If you run Navigatorr as a continuous service (for example, to serve the HTTP request queue ingest endpoint on `:8099` or managed as a long-running container in Portainer):

```bash
# Pull the latest image built by CI
docker compose pull

# Recreate and restart the persistent container
docker compose up -d --force-recreate
```

*In Portainer:* Navigate to **Stacks** > select your Navigatorr stack > toggle **Pull latest image** > click **Update the stack**.

> **Data Persistence:** The `navigatorr-cache` volume mounted at `/root/.cache/navigatorr` holds the SQLite database (`navigatorr.db`) and spec cache. It survives restarts and container recreation without data loss.

### Mode B: Executing Navigatorr via MCP stdio

Navigatorr communicates with AI assistants via MCP stdio (JSON-RPC over stdin/stdout). When used as an MCP server, the MCP host (such as `tunnel-client`, Claude Desktop, or Claude Code) launches the container per session and keeps `stdin` and `stdout` attached:

**Option 1: Using Docker Compose**
```bash
docker compose run --rm -T navigatorr
```
*Key flags:*
* `-T`: Disables pseudo-TTY allocation so raw JSON-RPC framing is not corrupted.
* `--rm`: Removes the ephemeral container process once the MCP session disconnects, while all state remains persisted in `navigatorr-cache`.

**Option 2: Using direct Docker command (Recommended for MCP hosts like `tunnel-client`)**
```bash
docker run --rm -i --pull always --network host \
  -v /path/to/config.yaml:/root/.config/navigatorr/config.yaml:ro \
  -v navigatorr-cache:/root/.cache/navigatorr \
  ghcr.io/dmrtx/navigatorr:latest
```
*Key flags:*
* `-i`: Keeps `stdin` open for interactive JSON-RPC communication. Do NOT pass `-t`.

If the GHCR package is private, authenticate Docker/Portainer to `ghcr.io` before deploying. Once the package is public, no registry credentials are required.

## Persistent maintenance database

The `navigatorr-cache` volume already mounted at `/root/.cache/navigatorr` now holds two things: the OpenAPI spec cache and the SQLite maintenance database (`navigatorr.db`). No extra mount is needed — but do not remove that volume, or preferences, maintenance jobs, release decisions and the audit log go with it.

```bash
# inspect the live database from the host (read-only peek)
docker compose run --rm -T navigatorr ls -la /root/.cache/navigatorr
```

To start from a clean slate, stop the container and delete only the db file inside the volume (or `docker volume rm navigatorr_navigatorr-cache` to drop the spec cache too). To back it up, copy `navigatorr.db`, `navigatorr.db-wal` and `navigatorr.db-shm` together while the container is stopped.

If Navigatorr must see your media files (for `inspect_media` / `fs_*` tools), mount them alongside the config:

```yaml
volumes:
  - /home/david/.config/navigatorr/config.yaml:/root/.config/navigatorr/config.yaml:ro
  - navigatorr-cache:/root/.cache/navigatorr
  - /volume1/Media/Movies:/media/Movies:ro
  - /volume1/Media/Anime:/media/Anime:ro
  - /volume1/Media/Downloads:/media/Downloads:rw
```

and list the **container target paths** (`/media/Movies`, `/media/Anime`, `/media/Downloads`) under `media.allowed_read_roots` (and `/media/Downloads` under `allowed_write_roots`) in `config.yaml`.

---

## Host vs Container Path Mapping

When running Navigatorr inside a Docker container, the application only sees the container's isolated filesystem namespace:

```text
Host System (e.g. Synology NAS)                Navigatorr Container
┌───────────────────────────────┐              ┌───────────────────────────┐
│ /volume1/Media/Movies         │ ── mount ──> │ /media/Movies             │
│ /volume1/Media/Downloads      │ ── mount ──> │ /media/Downloads          │
│ /home/david/.config/...       │ ── mount ──> │ /root/.config/...         │
└───────────────────────────────┘              └───────────────────────────┘
```

### Common Mistake: Using Host Paths in `config.yaml`
If your `config.yaml` contains:
```yaml
media:
  allowed_read_roots:
    - "/volume1/Media/Movies"   # ❌ WRONG! Does not exist inside container!
```
Navigatorr will fail to find or inspect files, and on startup will log:
```text
WARN: configured read root "/volume1/Media/Movies" does not exist inside container (verify Docker volume mounts)
```

### Correct Configuration:
Use the **container mount path**:
```yaml
media:
  allowed_read_roots:
    - "/media/Movies"           # ✅ CORRECT: Matches container mount point
    - "/media/Downloads"
  allowed_write_roots:
    - "/media/Downloads"
```

You can verify the status of configured roots anytime by running the `diagnostics` MCP tool and checking `effective_config.root_validation`.

---

## `allow_destructive` Troubleshooting

The `allow_destructive` setting is Navigatorr's central safety gate.

### 1. Where it MUST be located
`allow_destructive` is a **top-level (root) key** in `config.yaml`:
```yaml
# config.yaml (Top Level)
allow_destructive: false   # or true
concurrency:
  max_api_simultaneous: 3
media:
  ...
```

> **Warning:** If you accidentally indent `allow_destructive` under `services:`, `media:`, or `maintenance:`, YAML unmarshaling will silently ignore it and it will remain `false`!

### 2. Environment Variables DO NOT Work
Navigatorr does **NOT** read environment variables for application configuration. Setting:
```yaml
# docker-compose.yml
environment:
  - ALLOW_DESTRUCTIVE=true  # ❌ HAS NO EFFECT!
```
will be ignored by Navigatorr. The setting **must** be set inside `config.yaml`.

### 3. Verification
To verify the active setting:
- **Startup logs:** Check the first line output by Navigatorr:
  ```text
  config loaded from ... (destructive=false, read_roots=3, write_roots=1, state=...)
  ```
- **Diagnostics Tool:** Call the `diagnostics` MCP tool. Inspect:
  ```json
  "effective_config": {
    "allow_destructive": false,
    "config_file_loaded": "/root/.config/navigatorr/config.yaml"
  }
  ```

---

## Permissions & Safety Gate Matrix

| Tool / Action | Required Read Root | Required Write Root | Authorizing Job State | `allow_destructive: true` Required? |
|---|:---:|:---:|:---:|:---:|
| `call_api` (GET, POST, PUT) | — | — | — | No |
| `call_api` (DELETE) | — | — | — | **YES** |
| `inspect_media` | `media.allowed_read_roots` | — | — | No |
| `fs_stat` / `fs_list` / `fs_hash` | `media.allowed_read_roots` | — | — | No |
| `fs_safe_move` | `media.allowed_read_roots` (source) | `media.allowed_write_roots` (destination) | — | No |
| `fs_safe_delete` | — | `media.allowed_write_roots` | Job in `replacing` | **YES** |
| `qbit_manage_torrent` (pause/resume) | — | — | — | No |
| `qbit_manage_torrent` (delete/delete_files) | — | — | — | **YES** |
| `transmission_manage_torrent` (remove/remove_data) | — | — | — | **YES** |
| `sabnzbd_manage_item` (delete) | — | — | — | **YES** |
| `safe_replace` (`delete_original` via arr/fs) | — | `media.allowed_write_roots` (if via fs) | Job in `replacing` | **YES** |

> **Note on Filesystem Permissions:** `allowed_write_roots` does **NOT** imply read permission. If you want a directory to be both inspected and cleaned/replaced, it must be listed in **both** `allowed_read_roots` and `allowed_write_roots`.

---

## Adding Access to a New Directory

To grant Navigatorr access to an additional media directory (e.g., `/volume1/Media/Music`):

1. **Mount it in Docker (`compose.yaml`):**
   ```yaml
   volumes:
     - /volume1/Media/Music:/media/Music:ro
   ```
2. **Add to `config.yaml`:**
   ```yaml
   media:
     allowed_read_roots:
       - "/media/Anime"
       - "/media/Movies"
       - "/media/Downloads"
       - "/media/Music"       # Added here
   ```
3. **Restart / Rerun Navigatorr container.**
4. **Verify:** Check logs or call `diagnostics` to ensure `effective_config.root_validation` has 0 warnings for `/media/Music`.

---

## Configuration Precedence

When Navigatorr starts, configuration is resolved in this order:
1. **Flag `-config <path>`** (if specified, loads config from given path).
2. **Default Config File:** `~/.config/navigatorr/config.yaml`.
3. **Dynamic Memory Store:** Values stored dynamically via `memory_set` in the SQLite database take precedence over static defaults in the `maintenance:` section per scope.
4. **Environment Variables:** No overrides supported by design.

---

## Updating

The Compose file uses `pull_policy: always`, so it checks for a newer `latest` image whenever the service is run. The direct Docker command likewise uses `--pull always`.

For a reproducible deployment, replace `latest` with a `sha-*` tag instead.

