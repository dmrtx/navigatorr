# Maintenance console

The opt-in console is served at `/` by the existing persistent Navigatorr HTTP process; MCP stays at `/mcp`. It uses the same SQLite actions, profiles, permissions and HTTP transcode worker. No separate project, frontend build or second workflow implementation is needed.

## Enable

Add to the configuration used by the persistent server:

```yaml
mcp:
  transport: streamable-http
  listen: "127.0.0.1:8098"
web:
  enabled: true
  token_file: "/run/secrets/navigatorr-web-token"
```

Create the token file with a random token of at least 24 characters, readable only by the server. `web.token` is an alternative to `token_file`; configure exactly one. Restart Navigatorr and open its HTTP origin. `web.enabled` defaults to false and requires `streamable-http`; it cannot run in the lifecycle of a stdio MCP client. The SQLite migration is automatic (schema 8).

Use HTTPS through the existing reverse proxy for remote access. The web token protects `/api/maintenance/*`, with bounded, 12-hour HttpOnly/SameSite sessions and same-origin mutation checks. The worker token and *arr API keys stay on the server. Existing `/mcp` authentication is unchanged: protect that endpoint separately at the proxy. Static assets contain no credentials. Enabling the web console does not enable destructive actions or audio-only transcoding.

## Workflow

- Browse **Series / Sonarr** or **Movies / Radarr**, search titles, open their files and inspect a selected source with ffprobe. *arr file metadata is labeled as library metadata, rather than a live probe. Local browsing uses the configured read roots.
- Select one source file, a whole series, a season or marked episode files. Season and selected-file filters intersect; shared multi-episode files are encoded once. `max_items` retains its existing MCP semantics. Movie jobs operate on the selected movie file.
- Use a shared recipe, automatic selection or a per-job typed profile. The form covers x265 and VideoToolbox quality/bitrate, audio copy/compact, calibration, VMAF/SSIM, search values, samples, final validation, bit-depth preservation and size guardrails. The full JSON profile editor exposes the remaining typed MCP options. A nonempty JSON editor overrides the basic controls.
- Preview a batch, benchmark one file or enqueue a conversion. Admission returns immediately after the action and retry receipt are committed; the process-owned reconciler performs preflight and execution. Closing the browser has no effect. Restarting Navigatorr recovers pending actions. Repeating the same receipt, even after completion, returns the original action; changing its inputs is rejected.
- **Queue and history** lists both UI and MCP actions, including older records with unknown origin. Open a job for worker progress, decisions, pause/resume batch admission, cancellation, safe retry, paginated steps, chunked state/outputs and a bounded worker log tail. Pausing a batch stops new admission while already admitted jobs continue. Cancellation propagates to admitted children.
- Conversion produces a candidate. Preparing a replacement creates `promote_transcode_candidate`; the exact source/candidate and recovery copy are reviewed before **approve**. Sonarr series/season batches can request the existing batch promotion approval. Radarr uses `service=radarr,movie_id=<id>` for single-movie promotion. Both retain recovery on uncertain outcomes and verify identity, content, adoption, cleanup and final paths before removing recovery.
- **Profiles** uses the existing managed recipe store, with revision checks, history and immutable admitted plans. **Tools** exposes the relevant transcode, recipe, inspection and recovery MCP operations through a restricted adapter. Generic `call_api` and unrelated destructive filesystem tools are excluded.

## Boundaries and extension

`maintenanceui` owns HTTP/session handling, normalized library reads and embedded browser assets. `action` owns durable admission and workflow policy; `transcode` owns the worker protocol; recipe policy remains typed and versioned. The UI is an adapter over registered MCP handlers, so future maintenance operations can be deliberately exposed without cloning policy.

Audio streams inside videos retain the existing copy/compact support. Audio-only files remain a later feature: introduce a typed audio plan/recipe and executor validation in the domain first, then add its controls/navigation in this adapter. The current bootstrap explicitly advertises only `asset_kinds: [video]`.

Library navigation is paginated in the adapter (100 records maximum per page); Sonarr/Radarr still return their underlying collection to the server. History retains the existing MCP pagination. Web session state is intentionally process-local, so a server restart requires signing in again while jobs continue.

## Validation

Backend regressions cover durable admission/restart/cancel/retry receipts, authenticated same-origin requests, restricted tools, bounded library reads, selected-file/season filtering, worker log errors, and Radarr approval/recovery/identity drift. Existing Sonarr promotion and batch tests remain in the suite. Manual browser verification uses an isolated synthetic Sonarr/Radarr library and temporary database; it does not replace acceptance against a live NAS/worker.
