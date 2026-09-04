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

## Docker Compose

A ready-to-use `compose.yaml` is included in the repository. It uses the published GHCR image, host networking, a read-only config mount, and a persistent cache volume.

Pull the latest image:

```bash
docker compose pull
```

Because Navigatorr is an MCP stdio server, start it attached to stdin/stdout with:

```bash
docker compose run --rm -T navigatorr
```

`-T` is important because MCP JSON-RPC should not run through a pseudo-TTY. Do not use `docker compose up -d` for normal MCP use; a detached container has no MCP client attached to its stdio transport.

The included Compose service is equivalent to:

```yaml
services:
  navigatorr:
    image: ghcr.io/dmrtx/navigatorr:latest
    pull_policy: always
    network_mode: host
    stdin_open: true
    tty: false
    volumes:
      - /home/david/.config/navigatorr/config.yaml:/root/.config/navigatorr/config.yaml:ro
      - navigatorr-cache:/root/.cache/navigatorr
    restart: "no"

volumes:
  navigatorr-cache:
```

## Test the image directly

```bash
docker run --rm -i \
  --pull always \
  --network host \
  -v /home/david/.config/navigatorr/config.yaml:/root/.config/navigatorr/config.yaml:ro \
  ghcr.io/dmrtx/navigatorr:latest
```

The `-i` flag is required because MCP uses stdin/stdout. Do not add `-t`, because a pseudo-TTY can interfere with JSON-RPC framing.

## Use it from tunnel-client

Use the Docker invocation itself as the MCP command so the Navigatorr binary never runs directly on the host:

```bash
docker run --rm -i --pull always --network host \
  -v /home/david/.config/navigatorr/config.yaml:/root/.config/navigatorr/config.yaml:ro \
  ghcr.io/dmrtx/navigatorr:latest
```

You can also point a wrapper command at the Compose invocation:

```bash
docker compose -f /path/to/navigatorr/compose.yaml run --rm -T navigatorr
```

For `tunnel-client`, the direct `docker run` form is still the simplest because it does not depend on the repository being checked out on the server.

If the GHCR package is private, authenticate Docker/Portainer to `ghcr.io` before using the image. If you make the package public after its first successful publish, no registry credentials are needed to pull it.

## Updating

The Compose file uses `pull_policy: always`, so it checks for a newer `latest` image whenever the service is run. The direct Docker command likewise uses `--pull always`.

For a reproducible deployment, replace `latest` with a `sha-*` tag instead.
