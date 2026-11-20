# Transcode API v1

An HTTP API for everything the transcode side can tell you and do, served by the
Navigatorr server itself under `/transcode/v1`. Phase 1a added the reads; phase
1b (this version) adds the writes: starting, cancelling, resuming and retrying
actions, recording decisions, changing a batch goal, pausing and draining
workers, changing worker slots, recipes, backups, podcast review and the review
queue.

The server still registers the transcode MCP tools: the maintenance console
and these routes call them internally. A deployment that publishes the server
through a gateway with a tool allowlist can leave out the ones this API covers
completely (`recipe_*`, `worker_health`, `worker_drain`, `worker_pause`,
`transcode_backups`, `transcode_goal_change`, `podcast_*`) and have clients use
`call_api` instead; the reference deployment does that since phase 1c. Keep the
`action_*` tools: `POST /transcode/v1/actions` starts only the transcode and
podcast workflows, while `action_run` also starts every other workflow in the
catalog.

The routes reuse the functions the maintenance console and the MCP tools
already call, so an answer here is the same answer they give, and a write keeps
every safeguard of the function it calls (see "Writes"). Transcode never
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
  as the console: GET needs only the credential; a write also needs the
  console's write checks, below).
- The key is read once at startup from `web.transcode_token_file`, compared by
  hash, and opens `/transcode/v1` and nothing else. It is not the builds token.
- With no key configured, only the console sign-in works.
- A write made with the transcode key needs nothing else: the key is no ambient
  credential (a browser never attaches it by itself). A write made with the
  web sign-in (cookie, console bearer token, Cloudflare Access) goes through the
  console's own write rules, the same ones the `/api/` routes apply
  (`maintenanceui/server.go`, the handler wrapper; `transcodeAccess` in
  `maintenanceui/transcode_v1.go`): the header `X-Navigatorr-Request: 1` is
  required (`401` without it) and an `Origin`, when sent, must name this host
  (`403 forbidden` otherwise). A console write that sends no `Origin` at all
  is accepted: this is the rule of `/api/` too, where the
  `X-Navigatorr-Request` header is the CSRF defence (a cross-site form or
  fetch cannot add it) and `Origin` is checked whenever the browser sends it.
- A method a path does not serve answers `405 method_not_allowed` with an
  `Allow` header listing what it does serve (for example `GET, HEAD, PUT,
  DELETE` on `/pauses/{name}`, `POST` on `/actions/{id}/cancel`; `GET` alone on
  the podcast block route, which refuses HEAD). This holds for `/openapi.json`
  too: reading it is public, writing to it is a 405 (`Allow: GET, HEAD`).

Errors are JSON `{"error", "code", "hint"}`. Codes: `invalid_request` (400),
`unauthorized` (401), `forbidden` (403, a cross-origin console write, or a
destructive change the server keeps switched off), `not_found` (404, plus `unknown_worker`, `action_not_found`,
`batch_not_found`, `recipe_not_found`, `unknown_podcast`), `method_not_allowed`
(405), `conflict` (409, the state, revision, digest or version is not what the
request expects: read the resource again), `payload_too_large` (413),
`unsupported_media_type` (415), `rejected` (422, a recipe bundle that did not
validate), `unavailable` (503, the capability is switched off;
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

## Routes (reads)

Ids: `{id}` is an action or batch id, `{name}` a worker or recipe name.

| Route | Replaces |
|---|---|
| `GET /workers`, `/workers/{name}` | `worker_health`, `/api/maintenance/workers` |
| `GET /workers/{name}/slots` | `/api/maintenance/worker-slots` |
| `GET /activity` | `/api/maintenance/worker-activity` |
| `GET /state` | `/api/maintenance/worker-node-state` |
| `GET /drain` | `/api/maintenance/worker-drain` (read) |
| `GET /pauses`, `/pauses/{name}` | `worker_pause` (read) |
| `GET /windows`, `/workers/{name}/window` | the engine's node windows (read); a node with no window is 404 `no_window` |
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
| `GET /queue` | the order conversions wait in for a worker (new; see "The queue") |
| `GET /queue/next` | what would start next on each node and why the others would not (new; see "What starts next") |

The writes are listed under "Writes". `/actions/{id}`, the batch routes and every
write under `/actions/{id}` or `/backups/{id}` answer 404 for an action that is not a
transcode one (for example a Sonarr action), so this API never becomes a back
door into other kinds of work.

## Writes

Every write has a JSON request body: a JSON object (`Content-Type:
application/json`, at most 1 MiB; `null`, arrays and scalars are 400). Unknown fields, a second JSON value, a wrong type or a missing required
field are `400 invalid_request`; a body over 1 MiB is `413 payload_too_large`;
any other content type is `415 unsupported_media_type`. A write that takes no
body (including every `DELETE` but `/recipes/{name}`) accepts an empty one or `{}`. Nothing is changed by a request that is refused.

Each write calls the same MCP tool or maintenance handler as the read it
completes, and passes its safeguards on: the API checks a few more things (that
the action is a transcode one, the body's shape) but never removes one. A
refusal of the tool is `404` when the thing does not exist, `409 conflict` when
the CURRENT STATE of the thing refuses it (a digest, revision or version that
is stale; "only failed actions can be retried (current status: ...)"; an
immutable or already existing recipe version; a rollback with no previous
bundle; a classification that is frozen or cuts not planned yet; a backup or
candidate in the wrong state; an action that already finished; a submission
key that belongs to different inputs), `400` for a bad or missing parameter (a
missing confirm flag, digest, version or key included), `503` when a worker or
service cannot take the request now (unreachable, offline or not ready, not
configured on this server; the message names the kind, never the cause, which
is logged), `500 internal` when the server itself failed
(a file or database error such as "could not remove candidate file: ..." or
"permission denied"; the cause is only in the log), `403` when it needs a switch the
server keeps off. The status comes from the words of the refusal with the
values the caller sent (anything quoted) left out, so a body value never
picks it (escaped quotes included); words match whole, so "unchanged" is not
"changed". Enumerated fields are checked before the tool is called: the
decision of a review-queue entry (`selected`, `rejected`, `alternate`), the
`mode` (`size`, `quality`, `x265_preserve`, `compact`) and `scope` (`all`,
`unfinished`) of a goal change, and the shape of an action's resume `decision`
(a lowercase name; the options differ by workflow, so the action judges which
one it accepts). A fault of the operating system or the database (including a
missing file, a full disk, "no such table") is a 500 even when the refusal
says "not found". Failures
of the server (5xx) never carry their cause.

| Route | Calls | Body | Safeguard |
|---|---|---|---|
| `POST /actions` | `action_run` (202) | `action`, `inputs`, `idempotency_key` | only the transcode workflows (`GET /catalog`); idempotency key |
| `POST /actions/{id}/cancel` | `action_cancel` | `reason` | transcode actions only (else 404) |
| `POST /actions/{id}/resume` | `action_resume` | `decision`, `inputs`, `plan_digest` | `plan_digest` of the promotion plan read with `GET /batches/{id}/promotion-plan`; a stale one is 409 |
| `POST /actions/{id}/retry` | `action_retry` | none | transcode actions only |
| `POST /actions/{id}/podcast/classify` | `podcast_classify` | the classification object | block ids and digests from `GET .../podcast/blocks` |
| `POST /actions/{id}/podcast/review` | `podcast_review` | `digest`, `approve`, `reclassify`, `offset` | exact `digest` of the review; stale is 409 |
| `GET /batches/{id}/goal` | `transcode_goal_change` without a mode | query `scope` | read-only preview: the selection and its `selection_version` |
| `POST /batches/{id}/goal` | `transcode_goal_change` (202) | `mode`, `selection_version`, `idempotency_key`, options | `selection_version` from the preview (stale is 409); idempotency key |
| `PUT /workers/{name}/slots` | `/api/maintenance/worker-slots` | `video_slots`, `podcast_slots` and `revision` (all required), `ceiling`, `x265_slots`, `core_budget` | `revision` from `GET .../slots` (`settings.revision`); stale is 409. `0` and `0` explicitly return the worker to shared slots; `ceiling` `0` (or left out) keeps the stored parallel-jobs ceiling, it never clears it; a request that leaves any of the three out is 400, because the console would read the gap as "clear the split" or "no revision check" |
| `PUT /drain` | `/api/maintenance/worker-drain` | `enabled` | read `GET /drain` for `ready_for_deploy` |
| `PUT /pauses/{name}` | `/api/maintenance/worker-pause` | exactly one of `mode` (`graceful`, `immediate`, `suspend`, `off`) and `builds_paused` | the two switches are independent, never both in one request |
| `DELETE /pauses/{name}` | same, mode `off` | none | |
| `PUT /workers/{name}/window` | the engine's node window | `until` (RFC3339, optional), `margin_minutes` (0 to 1440, default 0), `media_types` (subset of `tv`, `anime`, `movie`; empty is all), `revision` (optional) | see "Availability windows" below; a stale `revision` is 409 `stale_window_revision`, an unknown node 404 `unknown_worker` |
| `DELETE /workers/{name}/window` | same | optional query `revision` | removes the window; stale is 409 `stale_window_revision` |
| `POST /backups/{id}/clean`, `/restore`, `/discard-duplicate` | `transcode_backups` | none | the tool's own state checks (409) |
| `POST /backups/{id}/discard` | `transcode_backups` discard | `confirm: true` | removes a recovery copy without verifying its replacement: `confirm` is required here, and the tool needs the destructive switch (403 when off) |
| `POST /backups/candidates/{id}/discard` | `transcode_backups` discard_candidate | `confirm: true` | the tool refuses without `confirm`; destructive switch |
| `PUT /recipes/{name}` | `recipe_save` | `profile`, `description`, `source_action_id`, `expected_generation`, `expected_digest` | to replace a recipe, the generation and digest read with `GET /recipes/{name}`; stale is 409 |
| `DELETE /recipes/{name}` | `recipe_delete` | `expected_generation`, `expected_digest` (both required) | stale is 409 |
| `POST /recipe-bundle/reload`, `/update` | `recipe_reload`, `recipe_update` | none | a bundle that does not validate (schema, semantics, integrity) is 422; a version that already exists with other content is 409; an unreachable recipe source is 503 ("the recipe source could not be reached", the cause is logged); a fault of the server (I/O, a corrupt cache) or any refusal that is not a known validation failure is a 500 without its cause; the active bundle is unchanged in every case |
| `POST /recipe-bundle/rollback` | `recipe_rollback` | `expected_active_digest`, `expected_previous_digest` (both required) | digests from `GET /recipe-bundle`; either stale is 409 |
| `DELETE /podcasts/{id}/ad-library/{entry}` | `podcast_ad_library` revoke | none | |
| `POST /review-queue/{id}/close` | the console's queue handler | `outcome` (`processed` or `dismissed`), `note` | |
| `POST /review-queue/{id}/decisions` | `decision_record` (201) | `title`, `decision`, release fields, `reasons` | |
| `POST /queue/actions/{id}/hold`, `/release` | the engine's queue marks | `queue_revision` (optional) | a conversion that already has a worker is 409 (cancel it or pause its node) |
| `POST /queue/items/{id}/hold`, `/release` | same, for a queued batch item | `queue_revision` (optional) | `{id}` is the item number |
| `POST /queue/batches/{id}/hold`, `/release` | the batch pause and resume | `queue_revision` (optional) | |
| `POST /queue/{actions,items,batches}/{id}/move` | same | exactly one of `first`, `before`, `after`; `queue_revision` (optional) | 409 for a stale revision, a subject with a worker, or a reference that is not waiting; 400 for moving relative to itself |

### The queue

`GET /queue` lists what is waiting for a worker, in the order it will be served:
standalone conversions, waiting conversions of a batch and queued batch items
that have no conversion yet. Each entry has `ref` (`kind` `action`, `item` or
`batch`, and `id`: a string for an action or batch, a number for an item),
`position` (1-based, held entries included), `held` (and `held_by`:
`conversion` or `batch`), `path`, `media_type` when known, `batch_id`,
`waiting_condition` and `waiting_reason`; the answer carries `queue_revision`.

- Hold: the entry keeps its exact place, never starts and does not make anyone
  behind it wait (`waiting_condition` is `queue_held`). Holding a conversion of
  a batch does not stop the batch, and the hold of a queued item carries over to
  the conversion made for it. Holding a whole batch is the batch pause. Cancel
  stays the definitive way to stop a conversion.
- Move: `{"first": true}`, `{"before": {"kind": ..., "id": ...}}` or
  `{"after": {...}}`. Only the moved subject gets a new place; a batch moves as
  one block and the conversions already running are untouched.
- `queue_revision` (optional in the body of any of these) is refused with 409
  (`stale_queue_revision`) when the queue has changed since that revision. Every
  answer carries the entry (or the batch) after the change and the new
  `queue_revision`.

### What starts next

`GET /queue/next` answers, for each worker node, what would start there next
and why the other waiting conversions would not. It is computed by the code
dispatch itself (the placer, the queue claims, node pauses, availability
windows, batch ordering), from the last report of each worker: it asks no
worker, claims nothing and writes nothing, so it is safe to call repeatedly. It
is a prediction for the `queue_revision` it carries.

Each node has `worker`, `state` (`ready`, `busy`, `paused`, `closed`,
`offline` or `unknown`), `free_slots`, `next` (`ref`, `position`, `path`, `media_type`,
`batch_id`, and `estimate_sec` when known; `null` when nothing would start),
`why_none` (when `next` is null) and `skipped`: the conversions ahead of `next`
in queue order (the whole queue when `next` is null), at most 20, with
`skipped_truncated` set when there were more. Every reason is a `code` and a
sentence that names the endpoint that would change it.

Codes: `held`, `batch_held`, `batch_gating` (its batch starts nothing new while
a conversion ahead of it waits for a slot), `size_unknown`, `node_offline`,
`node_paused`, `node_on_hold` (the node's own report says it takes nothing
new), `node_unknown`, `not_polling` (nothing has polled the conversion for a
long time, so it holds no one back and is never named as next), `node_build`,
`no_disk_room`, `waiting_for_approvals`,
`too_big_for_node`, `recently_refused`, `no_free_slot`, `slot_taken_ahead`,
`starts_elsewhere`, the window codes `node_window`, `media_type_not_allowed`,
`would_not_fit_window`, `no_duration_estimate`, `no_encode_history` and
`window_unreadable`, and for `why_none` also `queue_empty`, `all_held` and
`nothing_eligible`.

Limits: queued batch items have no conversion yet, so they are placed by media
type and window only (no disk check); a node whose last report is missing or older
than 2 minutes (or whose slot pool was not read in that time) is `unknown`: no
free slots are claimed and nothing is asked of it. With a single node nothing is
`unknown`: dispatch needs no report from it, so the plan shows the last news it
has, whatever its age (its last pool, its last offline or busy report, or the
configured capacity when it never reported). That includes a hold: a node whose
last report said it is on hold or building stays `paused`
(`node_on_hold`/`node_build`) until a newer report says otherwise, even though
dispatch itself ignores a hold report older than 45 seconds. Each node carries
`report_at` (RFC3339 UTC; omitted when the node has not reported), the time of
that report, so the reader sees how old what the plan shows is. The state of a node (unknown,
offline, paused, closed, busy) is what `why_none` says even when the queue is
empty. A node that reports it is on hold or building shows as `paused` and takes
no queued item in the plan; batch dispatch still counts its free slot when it
decides to make a child for a queued item, and that child then waits for a slot
(it cannot start there), so the plan may say nothing starts while the batch
makes a waiting child (with two or more queued items and the other node free,
dispatch makes a child for each: the first takes the free node and the second
waits for a slot while the held node cannot take it, whereas the plan only
lists the first); and the walk stops once every node has its `next`, so it says
nothing about what would start after those.

### Confirmation flows

There is no confirmation code in this backend; its confirmations are a digest, a
version or a flag the caller must have read first, and the API requires the same
ones, so no write can be made blind:

- Goal change: `GET /batches/{id}/goal` previews the exact selection and gives
  `selection_version`; `POST` with a different version (the selection moved) is
  `409`, and without a `mode` it is `400`.
- Promotion: `GET /batches/{id}/promotion-plan` gives the plan and its digest;
  `POST /actions/{id}/resume` with a stale `plan_digest` is `409`.
- Podcast review: the `digest` of the review being answered must be sent.
- Recipes and the bundle: the expected generation and digests are required where
  the tool requires them, and a stale one is `409`; the change is not applied.
- Worker slots: send the `revision` read with the settings; a stale one is `409`.
- Discarding a backup or a candidate: `confirm: true`.

### Idempotency

`POST /actions` and `POST /batches/{id}/goal` take an idempotency key, as the
tools do: send it as the `Idempotency-Key` header or the `idempotency_key` body
field (both are fine when equal; different values are `400`). Repeating the same
request with the same key returns the first result instead of repeating the work.
The goal change requires a key.

## Left out of this version

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

## Availability windows

A node can be made available only until a time, and only for some media
types. `PUT /workers/{name}/window` stores one window per node (a restart keeps
it); `GET /windows` lists them all and `GET /workers/{name}/window` reads one.
Each answer carries `worker`, `until`, `margin_minutes`, `media_types` (empty
means all), `closes_at` (`until` less the margin), `state` (`open` or
`closed`) and `revision`.

- A conversion starts on the node only if its media type is allowed (a
  conversion with no media type is allowed only when `media_types` is empty)
  and, when `until` is set, now plus the estimated encode time on that node is
  not past `closes_at`. The estimate is that of the encoder family the plan
  chose, from the node's own history at the source's resolution class; no other
  family's history is borrowed. Before the family is known (while the
  conversion waits for a node) the x265 history counts, or the copy history for
  a conversion known to be audio-only. With no estimate the conversion does not
  start on a node whose window has an `until`; it can still go to other nodes.
  A conversion that is assigned to the node but not yet sent when the window
  changes, and no longer qualifies, goes back to the queue; one already sent
  keeps running.
- At or after `closes_at` the node takes nothing new, and stays closed until the
  window is changed or deleted. Running conversions are never touched.
- A conversion that no node accepts waits with the condition `node_window` and
  a reason naming the node and why. It does not count as `worker_busy` and does
  not hold back other conversions or later items of its batch. Changing or
  deleting a window wakes it.
- If the window cannot be read, the node takes nothing new until it can.
- `revision` only goes up per node and survives deleting the window. For a node
  with no window the expected `revision` is the current counter, which `GET
  /workers/{name}/window` returns in the `no_window` 404 body as `revision`
  (0 for a node that never had one). A blank entry in `media_types` is 400
  `invalid_request`, and so is a `?revision=` on DELETE that is empty or not an
  integer.
