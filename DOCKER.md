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

If the GHCR package is private, authenticate Docker/Portainer to `ghcr.io` before using the image. If you make the package public after its first successful publish, no registry credentials are needed to pull it.

## Updating

Because the MCP command uses `--pull always`, a newly published `latest` image is picked up the next time `tunnel-client` starts Navigatorr. For a reproducible deployment, replace `latest` with a `sha-*` tag instead.
