# Transcode API v1 (read-only)

An HTTP API for everything the transcode side can tell you, served by the
Navigatorr server itself under `/transcode/v1`. This version (phase 1a) only
reads. Starting, holding, cancelling, pausing, draining, changing goals and
replacing files are not here yet: keep using the MCP tools for those.

The routes reuse the functions the maintenance console and the MCP tools
already call, so an answer here is the same answer they give. Transcode never
imports Nodes (invariant N2; `internal/importguard` checks it).

## Discoverability

`transcode` is registered as a service like any other, so an MCP client finds
and calls it with `list_services`, `search_api` and `call_api`:

```yaml
web:
  transcode_token_file: "/run/secrets/navigatorr-transcode-token"  # 24+ characters
services:
  transcode:
    url: "http://localhost:8080"          # this server's web address
    api_key: "<the transcode token>"
    api_version: "/transcode/v1"
    openapi_url: "http://localhost:8080/transcode/v1/openapi.json"
```

Example with `call_api` (service `transcode`):

```
search_api  service=transcode query="what is encoding now"
call_api    service=transcode method=GET path=/actions query={"status":"running"}
call_api    service=transcode method=GET path=/actions/{id}
```

## Authentication

- `GET /transcode/v1/openapi.json` is public.
- Everything else needs either the transcode API key (`X-Api-Key: <key>` or
  `Authorization: Bearer <key>`) or the web console's own sign-in (same rules
  as the console: GET needs only the credential).
- The key is read once at startup from `web.transcode_token_file`, compared by
  hash, and opens `/transcode/v1` and nothing else. It is not the builds token.
- With no key configured, only the console sign-in works.
- Any method but GET answers `405 method_not_allowed` (with `Allow: GET, HEAD`).

Errors are JSON `{"error", "code", "hint"}`. Codes: `invalid_request` (400),
`unauthorized` (401), `not_found` (404, plus `unknown_worker`, `action_not_found`,
`batch_not_found`, `recipe_not_found`, `unknown_podcast`), `method_not_allowed`
(405), `conflict` (409), `unavailable` (503, the capability is switched off),
`internal` (5xx).

## Paging

Every list is offset based: send `offset` (from 0) and, where it exists,
`limit`. The answer carries `has_more` and, except batch items, `next_offset`;
follow `next_offset` until `has_more` is false.

- Pages can be **short or empty and still have more**: `/actions` pages the
  engine and then keeps only transcode workflows, `/backups/candidates` pages
  by action. Never stop on an empty page; stop on `has_more: false`.
- `/batches/{id}/items` and `/review-queue` also report `total` and `offset`.
- Limits: `/actions` 1-100 (default 20), `/batches/{id}/items` 1-100 (default
  25), `/review-queue` 1-1000, `/recipes` 1-100 (default 50), `/history` 1-100.
- Bad values are `400 invalid_request` with a hint, never silently clamped.

## Routes

Ids: `{id}` is an action or batch id, `{name}` a worker or recipe name.

| Route | Replaces |
|---|---|
| `GET /workers`, `/workers/{name}` | `worker_health`, `/api/maintenance/workers` |
| `GET /workers/{name}/slots` | `/api/maintenance/worker-slots` |
| `GET /activity` | `/api/maintenance/worker-activity` |
| `GET /state` | `/api/maintenance/worker-node-state` |
| `GET /drain` | `/api/maintenance/worker-drain` (read) |
| `GET /pauses`, `/pauses/{name}` | `worker_pause` (read) |
| `GET /actions`, `/actions/{id}` | `action_list`, `action_status` |
| `GET /actions/{id}/detail/{section}` | `action_detail` (inputs, outputs, state, steps) |
| `GET /actions/{id}/podcast/blocks`, `.../blocks/{block_id}`, `.../artifacts/{artifact}` | `podcast_blocks`, `podcast_block`, `podcast_artifact` |
| `GET /history` | `action_history` |
| `GET /catalog` | `action_catalog` (transcode workflows only) |
| `GET /batches/{id}/items` | `/api/maintenance/batch-items` |
| `GET /batches/{id}/settings` | `/api/maintenance/batch-settings` |
| `GET /batches/{id}/candidates`, `/promotion-plan` | `/api/maintenance/batch-candidates` |
| `GET /batches/{id}/preview` | `/api/maintenance/batch-preview` |
| `GET /backups`, `/backups/candidates` | `transcode_backups` (list, candidates) |
| `GET /recipes`, `/recipes/{name}`, `/recipes/{name}/history`, `/recipe-bundle` | `recipe_list`, `recipe_get`, `recipe_history`, `recipe_status` |
| `GET /podcasts`, `/podcasts/{id}/local-library`, `/podcasts/{id}/ad-library` | `/api/maintenance/podcasts`, `/api/maintenance/podcast-local-library`, `podcast_ad_library` |
| `GET /review-queue` | `/api/maintenance/maintenance-queue` |

`/actions/{id}` and the batch routes answer 404 for an action that is not a
transcode one (for example a Sonarr action), so this API never becomes a back
door into other kinds of work.

## Left out of this version

- All writes: `action_run`, `action_cancel`, `action_resume`, `action_retry`,
  `decision_record`, `transcode_goal_change`, `worker_pause` (set), drain on or
  off, worker slot changes, `recipe_save`, `recipe_update`, `recipe_delete`,
  `recipe_rollback`, `recipe_reload`, `transcode_backups` clean, restore and
  discard, `podcast_classify`, `podcast_review`, the ad-library `revoke`, and
  closing a review-queue entry. They arrive in phase 1b.
- Things Transcode does not own: Nodes builds (`/nodes/v1`), file system tools
  (`fs_*`), `safe_replace`, media inspection, library scans, torrents, the
  generic operation ledger and the `maintenance_*` claim tools, which serve
  other areas.
- Reading a podcast block records the units delivered as read coverage (a later
  classification requires it); this is the one effect of a GET and it is
  documented on that route.
