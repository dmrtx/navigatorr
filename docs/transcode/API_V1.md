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
- Any method but GET answers `405 method_not_allowed` (with `Allow: GET, HEAD`,
  or `Allow: GET` on the podcast block route, which refuses HEAD). This holds
  for `/openapi.json` too: reading it is public, writing to it is a 405.

Errors are JSON `{"error", "code", "hint"}`. Codes: `invalid_request` (400),
`unauthorized` (401), `not_found` (404, plus `unknown_worker`, `action_not_found`,
`batch_not_found`, `recipe_not_found`, `unknown_podcast`), `method_not_allowed`
(405), `conflict` (409), `unavailable` (503, the capability is switched off;
every podcast route, `GET /podcasts` included, answers it when podcasts are
disabled, and 404
`unknown_podcast` only for a profile that does not exist or is off),
`response_too_large` (502, the answer would not fit: narrow the request with a
smaller `limit`, a later `offset`, a section or a key), `internal` (500, a fault
of the server; the cause is in its log, not in the answer). Every 5xx a
maintenance handler behind this API answers is presented the same way: the
original message is logged and the caller gets a generic one.

A 404 of a thing inside an existing action (a `key`, a step) is the generic
`not_found` with a hint, not `action_not_found`, which means the action itself.
Only clear "not found" refusals of the underlying tools are 404; a refusal about
a parameter is 400.

## Paging

Every list is offset based: send `offset` (from 0) and, where it exists,
`limit`. The answer carries `has_more` and, except batch items, `next_offset`;
follow `next_offset` until `has_more` is false. The exception is `/history`: the
audit log has no offset, so it answers `{count, records}` only (see below).

- Pages can be **short or empty and still have more**: `/actions` pages the
  engine and then keeps only transcode workflows, `/backups/candidates` pages
  by action. Never stop on an empty page; stop on `has_more: false`.
- `/batches/{id}/items` and `/review-queue` also report `total` and `offset`.
- Limits: `/actions` 1-100 (default 20), `/batches/{id}/items` 1-100 (default
  25), `/review-queue` 1-1000, `/recipes` 1-100 (default 50), `/history` 1-100.
- Bad values are `400 invalid_request` with a hint, never silently clamped:
  integers (`offset`, `limit`, `chunk`, `step_index`, `step_offset`,
  `step_limit` 1-50, ...) must be whole numbers in range, and booleans
  (`verbose`, `unknown_only`, `include_selection_details`) must be `true`,
  `false`, `1` or `0`. An empty value counts as absent.
- `/recipes/{name}/history` limit is 1-50.
- `/history` reads the newest 100 matching log entries (fewer when they would
  not fit in one response: it then retries with fewer, down to `limit`), keeps
  the transcode ones and cuts them to `limit`; it has no `offset`, `has_more` or
  `next_offset`. `action` set to a log event (`action_completed`, `action_failed`,
  `action_retry`, `action_cancelled`) narrows the read itself; a workflow name is
  applied after it. If even `limit` entries do not fit, it is a 502.

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
| `GET /history` | `action_history`, cut to transcode actions (see below) |
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
- `/history` only returns the audit entries of transcode actions (completed,
  failed, retried, cancelled), never other services' history. `action` is a
  transcode workflow name or one of those four events; anything else is 400.
  The log has no offset: the newest 100 entries matching `media` and `service`
  are read and then cut to the transcode ones, so a page can hold fewer than
  `limit` entries.
- Reading a podcast block records the units delivered as read coverage (a later
  classification requires it); this is the one effect of a GET and it is
  documented on that route. Because of it `HEAD` on that route answers
  `405` (`Allow: GET`) and records nothing; `HEAD` elsewhere behaves as `GET`.
