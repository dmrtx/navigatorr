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

## Mobile and PWA

The mobile view opens on the queue, with fixed bottom navigation, compact savings and job telemetry, 44-pixel action targets (including landscape/tablet), compact 40-pixel form fields and safe-area spacing. Typography and spacing are deliberately dense: 13-pixel job titles, inline source/scope/profile rows and a single-line worker status/percentage. The original paper/forest colors and serif headings are retained. Zero-valued batch counters and redundant workflow steps move to detail; full source paths remain in job detail. File selection remains directly available in **Files**, and advanced profile/size controls expand in place. Returning to the foreground refreshes monitoring without discarding the current prepared conversion. The browser is not the transcoding worker: backgrounding or closing the PWA leaves durable jobs on the server.

The same origin exposes a standalone manifest, maskable 192/512 icons, an Apple touch icon and an **Install** button. Android browsers can offer their installation prompt; iPhone/iPad use Share → Add to Home Screen. Installation requires HTTPS (localhost is sufficient for development). A plain HTTP LAN address can still use the web interface, but does not provide the secure context required for the service worker.

The versioned service worker caches only the public HTML/CSS/JS/icons. API responses, sessions, tokens, job history and mutation requests are never cached or replayed. With the server unreachable, a previously visited PWA can reopen its shell; it displays a disconnection notice and requires reconnection for job data or changes. In an already-open page, the last visible measurements are marked stale and mutation controls are disabled. **Reconnect** and browser online/foreground events revalidate the server/session. An uncertain submission keeps its existing durable receipt; reconnection does not automatically resubmit it. The public shell falls back to cache on network failure, proxy 5xx or a five-second network timeout; cache-write failure still permits the live response. Asset changes update the worker and retire only Navigatorr's old shell cache. Expired sessions dismiss stale approval/detail dialogs and expose sign-in.

## Workflow

- Browse configured filesystem read roots directly. File sizes come from disk; folder navigation, search and pagination work without Sonarr/Radarr. Optional Series/Movies catalogs add metadata and library import associations, rather than defining the transcode source.
- Select a single file, marked videos, the videos in a folder, or its subfolders. Filesystem batch admission freezes an explicit `paths` list (up to 1000 videos), resolves allowed roots and deduplicates canonical paths. An oversized selection fails instead of silently truncating. Series catalogs also support a season, whole series and selected-file intersection, including shared multi-episode files encoded once.
- Use a shared recipe, automatic selection or a per-job typed profile. The form covers x265 and VideoToolbox quality/bitrate, audio copy/compact, calibration, VMAF/SSIM, search values, samples, final validation, bit-depth preservation and size guardrails. The full JSON profile editor exposes the remaining typed MCP options. A nonempty JSON editor overrides the basic controls.
- Preview a batch, benchmark one file or enqueue a conversion. Admission returns immediately after the action and retry receipt are committed; the process-owned reconciler performs preflight and execution. Closing the browser has no effect. Restarting Navigatorr recovers pending actions. Repeating the same receipt, even after completion, returns the original action; changing its inputs is rejected.
- The library and operations queue share one screen; the queue is also available on its own. Both UI and MCP jobs expose inline retry, batch pause/resume, decisions and cancellation. Pausing stops new admission; admitted jobs continue. Cancellation propagates to children. Batch details show paginated native file rows and their child jobs, rather than requiring JSON inspection.
- Worker measurements refresh every five seconds while the page is visible: phase, percentage, queue position, speed, fps and stale-observation markers. ETA is an approximation only during encoding with recent progress, source duration and measured speed. Process-owned reconciliation remains authoritative; browser polling neither starts nor owns encoding.
- Savings distinguish estimates (sample benchmark or profile heuristic), validated candidate savings, and completed replacement savings. The persistent action ledger supplies cumulative net content bytes and a chronological trace; batches and duplicate replacement claims are not counted twice. Originals and retained recovery copies exclude a job from realized savings. The figure is file-content reduction, not a measurement of filesystem free space or allocation.
- Conversion produces a candidate. A separate replacement action reviews its exact paths and requires approval before mutation, retaining verified recovery on uncertain outcomes. Filesystem replacement uses the original basename with `.mkv`, refuses collisions and verifies the published file before recovery cleanup. Sonarr/Radarr remain optional library adoption integrations; existing series batch adoption keeps one exact batch approval.
- **Profiles** uses the existing managed recipe store, with revision checks, history and immutable admitted plans. **Tools** exposes the relevant transcode, recipe, inspection and recovery MCP operations through a restricted adapter. Generic `call_api` and unrelated destructive filesystem tools are excluded.

## Boundaries and extension

`maintenanceui` owns HTTP/session handling, filesystem navigation, normalized optional catalog reads and embedded browser assets. `action` owns durable admission and workflow policy; `transcode` owns the worker protocol; recipe policy remains typed and versioned. The UI is an adapter over registered MCP handlers, so future maintenance operations can be deliberately exposed without cloning policy.

Audio streams inside videos retain the existing copy/compact support. Audio-only files remain a later feature: introduce a typed audio plan/recipe and executor validation in the domain first, then add its controls/navigation in this adapter. The current bootstrap explicitly advertises only `asset_kinds: [video]`.

Library navigation is paginated in the adapter (100 records maximum per page); Sonarr/Radarr still return their underlying collection to the server. Operations paginate after filtering the shared workflow ledger; batch items paginate independently. Savings history returns the latest 25 replacements while the total includes all verified records. Web session state is intentionally process-local, so a server restart requires signing in again while jobs continue.

## Validation

Backend regressions cover durable admission/restart/cancel/retry receipts, authenticated same-origin requests, restricted tools, bounded library reads, selected-file/season and filesystem filtering, worker log errors, filesystem/Radarr approval/recovery/identity drift, savings accounting/restart and UI response-race regressions. Existing Sonarr promotion and batch tests remain in the suite. Manual browser verification uses an isolated synthetic Sonarr/Radarr library and temporary database; it does not replace acceptance against a live NAS/worker.

Mobile browser checks cover 320, 360, 390 and 430-pixel widths, tablet (768), landscape (844×390), desktop (1280), file/profile selection, batch preview/details, inline retry, replacement-plan review, advanced controls and actual server-stop/offline reload/reconnect. PWA regressions cover public routes/icon dimensions, cache isolation, network-first fallback, mutation blocking and preservation of prepared inputs on reconnect. Installation and keyboard behavior on physical iOS/Android devices remain device acceptance checks.
