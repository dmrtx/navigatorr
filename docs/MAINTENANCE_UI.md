# Maintenance interface

The opt-in console is served at `/` by the existing persistent Navigatorr HTTP process; MCP stays at `/mcp`. It uses the same SQLite actions, profiles, permissions and HTTP transcode worker. No separate project, frontend build or second workflow implementation is needed.

## Enable

Add to the configuration used by the persistent server:

```yaml
mcp:
  transport: streamable-http
  listen: "127.0.0.1:8098"
web:
  enabled: true
  auth_mode: cloudflare_access
  cloudflare_access:
    team_domain: "https://your-team.cloudflareaccess.com"
    audience: "your-access-application-aud"
```

Cloudflare Access handles Google sign-in and registration policies. Configure its application to protect the whole origin, including `/api/maintenance/*` and `/mcp`. Navigatorr verifies the Access assertion signature, issuer, application audience and expiry using the team's public keys; it does not present a second login, ask for a token or offer local sign-out in this mode. The team domain and audience are server configuration, not credentials the browser user enters. Use HTTPS and keep the origin private behind the proxy/tunnel. Existing MCP authentication is unchanged.

For standalone use, omit `auth_mode` (defaults to `token`) and configure exactly one of `web.token_file` or `web.token` with a random token of at least 24 characters. This mode retains bounded 12-hour HttpOnly/SameSite sessions. Both modes preserve same-origin mutation checks; worker credentials and \*arr keys stay on the server. `web.enabled` defaults to false and requires `streamable-http`. Enabling the console does not enable destructive actions or audio-only transcoding. The SQLite migration is automatic (schema 8).

## Mobile and PWA

The mobile view opens on the queue, with fixed bottom navigation, compact savings and job telemetry, 44-pixel controls on touch-sized layouts (including landscape/tablet), 36-pixel desktop controls and safe-area spacing. Typography and spacing are deliberately dense: 13-pixel job titles, inline source/scope/profile rows and a single-line worker status/percentage. The original paper/forest colors and serif headings are retained. Zero-valued batch counters and redundant workflow steps move to detail; full source paths remain in job detail. Progress bars span the full job row and dialog actions keep content-sized widths. File selection remains directly available in **Files**, and less common profile controls open in a typed settings dialog. Returning to the foreground refreshes monitoring without discarding the current prepared conversion. The browser is not the transcoding worker: backgrounding or closing the PWA leaves durable jobs on the server.

On single-column widths (900 pixels and below, including phone landscape), Files has two steps: browse/select, then configure the transcode. Tapping a file advances directly; marked files use Continue, and Use folder configures the current folder. Back retains the prepared settings and selection. Wider desktop layouts keep the browser and form together. File checkboxes use a 14-pixel visual inside the same generous hit area and alignment column as folder icons. Open a path sits beside optional search in the source toolbar.

The same origin exposes a standalone manifest, maskable 192/512 icons, an Apple touch icon and an **Install** button. Android browsers can offer their installation prompt; iPhone/iPad use Share → Add to Home Screen. Installation requires HTTPS (localhost is sufficient for development). A plain HTTP LAN address can still use the web interface, but does not provide the secure context required for the service worker.

The versioned service worker caches only the public HTML/CSS/JS/icons. API responses, sessions, tokens, job history and mutation requests are never cached or replayed. With the server unreachable, a previously visited PWA can reopen its shell; it displays a disconnection notice and requires reconnection for job data or changes. In an already-open page, the last visible measurements are marked stale and mutation controls are disabled. The page automatically retries every five seconds; browser online/foreground events also revalidate the server/session. An uncertain submission keeps its existing durable receipt; reconnection does not automatically resubmit it. The public shell falls back to cache on network failure, proxy 5xx or a five-second network timeout; cache-write failure still permits the live response. Asset changes update the worker and retire only Navigatorr's old shell cache. Expired sessions dismiss stale approval/detail dialogs. Cloudflare mode directs the user to reload for Access authentication; standalone mode exposes its token sign-in.

## Workflow

- Browse configured filesystem read roots directly. File sizes come from disk; folder navigation with clickable breadcrumbs and continuous loading work without Sonarr/Radarr. Search is optional behind its icon. Optional Series/Movies catalogs add metadata and library import associations, rather than defining the transcode source.
- Select a single file, marked videos, the videos in a folder, or its subfolders. Filesystem batch admission freezes an explicit `paths` list (up to 1000 videos), resolves allowed roots and deduplicates canonical paths. An oversized selection fails instead of silently truncating. Series catalogs also support a season, whole series and selected-file intersection, including shared multi-episode files encoded once.
- Use a shared recipe, automatic selection or a per-job typed profile. The form covers x265 and VideoToolbox quality/bitrate, audio copy/compact, calibration, VMAF/SSIM, search values, samples, final validation, bit-depth preservation and size guardrails. Profiles exposes less common fields in a typed settings form; editing a shared profile preserves fields that are not changed.
- Preview a batch, benchmark one file or enqueue a conversion. Admission returns immediately after the action and retry receipt are committed; the process-owned reconciler performs preflight and execution. Closing the browser has no effect. Restarting Navigatorr recovers pending actions. Repeating the same receipt, even after completion, returns the original action; changing its inputs is rejected.
- Files, Queue, Profiles, Stats and Settings are separate workspaces. Queue and filesystem listings load continuously with a fallback continuation button. UI and MCP jobs expose inline retry, batch pause/resume, decisions and cancellation. Pausing stops new admission; admitted jobs continue. Cancellation propagates to children. Batch details show paginated native file rows and their child jobs, rather than requiring JSON inspection.
- Terminal batches with failed or cancelled files offer Reconfigure. The read-only form restores the frozen failed/cancelled subset and original settings, including custom profiles; completed candidates stay in the previous history. A new attempt requires explicit submission and fresh worker admission. Identical submission receipts prevent duplicates after a lost response. Changed encoding settings require new samples; old estimates are not assumed valid. Savings-gate failures are grouped by cause in the queue, with individual estimated growth/savings in file details. Processed counts include failures and skips, rather than implying successful conversion; failed processing and skipped replacement have distinct stages.
- Queue rows use one status and a concise type/result summary, with known source/candidate sizes and savings. Failed jobs show a shortened error reason when available. Decision rows show the next action instead of stale worker speed or progress. Worker measurements refresh every five seconds while the page is visible: phase, percentage, queue position, speed, fps and stale-observation markers. Queue telemetry appears only during active execution or worker waits. ETA is an approximation only during encoding with recent progress, source duration and measured speed. Process-owned reconciliation remains authoritative; browser polling neither starts nor owns encoding.
- The dedicated **Stats** section distinguishes estimates (sample benchmark or profile heuristic), validated candidate savings, and completed replacement savings. The persistent action ledger supplies cumulative net content bytes and a chronological trace; batches and duplicate replacement claims are not counted twice. Originals and retained recovery copies exclude a job from realized savings. The figure is file-content reduction, not a measurement of filesystem free space or allocation.
- Conversion produces a candidate. A separate replacement action reviews its exact paths and requires approval before mutation, retaining verified recovery on uncertain outcomes. Import routing comes from the admitted job or parent batch; historical jobs without that context offer an explicit replacement method. Existing replacement actions are reopened, and consumed candidates are no longer offered for replacement. Filesystem replacement uses the original basename with `.mkv`, refuses collisions and verifies the published file before recovery cleanup. Sonarr/Radarr remain optional library adoption integrations; existing series batch adoption keeps one exact batch approval.
- **Profiles** uses the existing managed recipe store, with revision checks, history and immutable admitted plans. **Settings** calls the configured recipe collection Shared presets: Refresh presets reloads its configured builtin/file/remote source, and Restore previous restores the preceding validated cache. The previous Reload and Update controls called the same operation and are now one action. These changes affect new jobs; admitted plans remain immutable. Verified recovery cleanup also uses normal controls. No generic JSON console is shown; the restricted MCP adapter remains available internally. Generic `call_api` and unrelated destructive filesystem tools are excluded.

## Boundaries and extension

`maintenanceui` owns HTTP/session handling, filesystem navigation, normalized optional catalog reads and embedded browser assets. `action` owns durable admission and workflow policy; `transcode` owns the worker protocol; recipe policy remains typed and versioned. The UI is an adapter over registered MCP handlers, so future maintenance operations can be deliberately exposed without cloning policy.

Audio streams inside videos retain the existing copy/compact support. Audio-only files remain a later feature: introduce a typed audio plan/recipe and executor validation in the domain first, then add its controls/navigation in this adapter. The current bootstrap explicitly advertises only `asset_kinds: [video]`.

Continuous library navigation uses bounded pages in the adapter (100 records maximum per page); Sonarr/Radarr still return their underlying collection to the server. Operations paginate after filtering the shared workflow ledger; batch items paginate independently. Savings history returns the latest 25 replacements while the total includes all verified records. Standalone web sessions are process-local, so a server restart requires signing in again while jobs continue. Cloudflare Access sessions remain managed by Cloudflare across Navigatorr restarts.

## Validation

Backend regressions cover durable admission/restart/cancel/retry receipts, authenticated same-origin requests, restricted tools, bounded library reads, selected-file/season and filesystem filtering, worker log errors, filesystem/Radarr approval/recovery/identity drift, savings accounting/restart and UI response-race regressions. Existing Sonarr promotion and batch tests remain in the suite. Manual browser verification uses an isolated synthetic Sonarr/Radarr library and temporary database; it does not replace acceptance against a live NAS/worker.

Mobile browser checks cover 320, 360, 390 and 430-pixel widths, tablet (768), landscape (844×390), desktop (1280), file/profile selection, batch preview/details, inline retry, replacement-plan review, advanced controls and actual server-stop/offline reload/automatic recovery. PWA regressions cover public routes/icon dimensions, cache isolation, network-first fallback, mutation blocking and preservation of prepared inputs on reconnect. Installation and keyboard behavior on physical iOS/Android devices remain device acceptance checks.

The [adversarial review](maintenance-ui/ADVERSARIAL_REVIEW.md) records the latest fixes, validation and remaining external acceptance checks. Deployment is cancelled.

## Design reference

The compact English interface follows the [Navigatorr Stitch project](https://stitch.withgoogle.com/projects/6967843794345897924): paper/forest theme, outline icon tabs and actions, icon-only refresh, no marketing header or duplicate queue, and structured profile controls. Generated examples are design references; savings and job states always come from the real ledger. Installation instructions appear only on request in a dismissible mobile dialog. Desktop installation is offered only after the browser emits its native installation event.

Queue and Profile actions use uniform icon controls with accessible names, differentiated primary/review/retry/cancel treatments, and shared desktop/touch dimensions. Mobile Profiles uses one native selector. Desktop checkboxes prepare the selected job immediately; successful encoding clears the submitted selection while retaining preferences.


### Process progress and retained copies

Batch bars retain the exact processed-file count. Active overall progress is marked `~`: equal workflow-step weights plus measured partial worker work or source-hash bytes, not elapsed-time prediction. Nested benchmarks contribute once through their parent file. Requested replacements reserve 15% of estimated work until replacement checks finish; active coordinators never report 100%. Failed and skipped files remain resolved work, not successful conversions.

Recovery inventory lists owned artifacts currently present on disk, with the last recorded error. Final-checkpoint cleanup retains all library, SHA-256 and ownership checks. A separate `transcode_backups(mode=discard_duplicate)` can discard redundant owned recovery artifacts from a failed promotion only while its unchanged original and full backup match the recorded digest, no delete/publication intent exists, and all recorded library commands resolved. It claims the original action's execution lease, repeats no conversion/import/rename/rescan, preserves failed status/checkpoint/error, and does not count a successful replacement or realized savings. Equal sizes only enable verification; they never authorize deletion.

Cleanup publishes durable phases and byte observations, at most once per second while hashing. Clients poll them while the cleanup HTTP request is still pending, retain the lock on a lost response with ongoing observations, and show the terminal result. The confirmation groups file, removable bytes and collapsed locations; the actual outcome may retain the copy on verification failure.

Queue accounting rereads authoritative SQLite rows but memoizes bounded JSON projections. Complete input/output/state strings are compared before reuse, including same-size writes in the same timestamp; status, checkpoints and metadata are always fresh. The memo has a 64 MiB limit and drops removed rows. Full action history remains unchanged. Independent queue/detail/worker/cleanup refreshes run together.

Candidate validation review offers acceptance, rejection (keep original), deferral and a new configuration for the reviewed batch file only. Sizes and filename come from that child, not aggregate batch totals. Decisions bind to the exact durable candidate version under the execution lease. Reconfiguration first validates settings/readiness, rejects that candidate, then admits one frozen-file attempt with a stable receipt; other batch items are excluded and replacement approval is not inherited.

Recovery inventory also offers explicit Remove copy for finished jobs. This removes only owned regular backup/partial artifacts without checking the replacement or replaying promotion, and retains job history. A concise irreversible-removal confirmation is required; active jobs, symlinks and altered ownership remain blocked. Verify & clean up remains an independent alternative.


### Responsive controls and batch attempts

The browser submits media controls and recovery cleanup with `background:true` and a stable receipt key. HTTP returns a durable `maintenance_command` identifier immediately; the reconciler performs the work independently of the browser. `/api/maintenance/commands` exposes its pending/running/completed/failed state. Controls give immediate busy feedback, poll the receipt every second, and reuse it after a lost response. These receipts are excluded from the media queue and catalog. Synchronous MCP behavior is preserved.

`GET/POST /api/maintenance/batch-settings` changes scheduling intent within the existing batch coordinator. Scope is this reviewed candidate, unfinished files, or the full frozen file selection. It preserves the batch ID/number and original immutable inputs. Per-item settings, attempt generations and prior child actions are durable; item resets and the coordinator journal commit atomically under its execution lease. Active attempts finish before another attempt for that file is admitted. Reconfiguration does not delete candidates, expand the library selection, or reuse a replacement approval. Files with replacement jobs or an existing replacement review cannot be reconfigured through this route. Old API clients may still explicitly request a separate batch via the legacy batch-reconfigure route.
