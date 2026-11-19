# Nodes

Nodes is the build registry of Navigatorr: the record of what builds run on, or
are queued for, the machines ("nodes") that also transcode. It answers three
questions: which builds exist and what state are they in, which nodes accept new
builds, and what did a build print.

It is a service of its own, not a set of MCP tools. The MCP stays small and
generic: an agent finds Nodes with `list_services`, `search_api` and
`get_endpoint_details`, and uses it with `call_api` like any other API.

## What it owns, and what it does not

- Nodes owns the build records, their heartbeats and expiry, the per-node
  "builds paused" switch and the in-memory output of each build. They live in
  their own SQLite file (`database.nodes_path`, default `nodes.db` next to the
  main database).
- Nodes does **not** decide whether a node is held. The node worker is the only
  authority on a running lease (see
  [transcode/BUILD_LEASES.md](../transcode/BUILD_LEASES.md)). The server's
  record follows the worker's reports.

Invariants, and where each one is enforced:

1. N1: a running lease is decided only by the worker. Enforced by the node
   worker, and proved by its tests from phase 0a in `internal/transcodeworker`
   (`lease_test.go`, `lease_gate_test.go`, `hold_test.go`: a lease freezes the
   encoder and holds the queue, nothing new starts while it runs). The tests in
   `nodes/invariants_test.go` only show that the registry never changes a build
   because of what the transcode side says; they do not prove the hold.
2. N2: the transcode engine (`action`, `transcode`) never imports `nodes`, and
   `nodes` never imports them. Only `main` and `internal/nodeswire` join them,
   through interfaces. Proved by `nodes/invariants_test.go` (source level) and
   `internal/importguard` (the whole import graph).
3. N3: Nodes being down never stops a lease; the worker fails open. Enforced by
   the worker and proved by its phase 0a tests
   (`TestLeasePausedRefusesAndUnreachableDoesNot` in
   `internal/transcodeworker/lease_test.go`, and the lease report tests next to
   it). The registry needs nothing from the transcode side to work
   (`nodes/invariants_test.go`), which is the half of N3 that lives here.
4. N4: the legacy `/api/maintenance/builds*` routes are unchanged (they are thin
   aliases onto Nodes); `maintenanceui/nodes_v1_test.go` checks that both
   routes store the same record.
5. N5: a cancel through `/nodes/v1` reaches the worker exactly like a legacy
   one: the next forwarded heartbeat is answered "build already ended" with a
   cancelled build.

## API overview

Base path `/nodes/v1`. The OpenAPI 3 document is at
`GET /nodes/v1/openapi.json` (public, so a service registry can fetch it).
Every operation has a summary and a description written for an LLM reader.

| Method and path | What it does |
| --- | --- |
| `GET /nodes` | Configured nodes: builds paused, running builds, transcode state. |
| `GET /nodes/{name}` | One node. |
| `POST /nodes/{name}/builds-pause` `{paused}` | Refuse or accept new builds on the node. Idempotent. |
| `GET /builds` | List builds, newest created first, in one stable order whatever their status. Filters `node`, `status`, `source`; `limit` 1 to 500 (default 50, else 400); `cursor` is the `next_cursor` of the previous page (opaque, still valid if that build was pruned; anything else is 400 `invalid_cursor`). Returns `{builds, next_cursor}`; the last page has no `next_cursor`. |
| `POST /builds` | Register a build. `client_id` in the body makes a retry idempotent. |
| `GET /builds/{id}` | One build. |
| `POST /builds/{id}/heartbeat` | Keep it alive; optional `progress`, `ttl_seconds`, `log` lines. |
| `POST /builds/{id}/finish` | End it: `succeeded`, `failed` or `cancelled`, optional `exit_code`. |
| `POST /builds/{id}/cancel` | Cancel it (a POST, never a DELETE). Idempotent. |
| `GET /builds/{id}/log?after=N&generation=G` | Numbered output lines after line N. No long-poll. Numbers restart whenever the build's in-memory buffer is recreated (server restart, eviction, retention), so the answer carries a `generation`; send it back with `after`. When it differs, or `after` is past the last line, the answer starts from the first line held with `reset: true`: discard what you kept and continue from `next_after`. |

Errors are JSON `{"error", "code", "hint"}`. Codes include `invalid_request`
and `invalid_cursor` (400), `unauthorized` (401), `unknown_node` and
`build_not_found` (404), `builds_paused`, `node_lease_required`,
`build_id_taken` and `build_ended` (409; a heartbeat on a build that is over
carries the final record as `build`). The hint says what to do next.

`ttl_seconds` (create and heartbeat): omitted or 0 means the default (900
seconds on create; on a heartbeat, the window the build already has); a positive
value is used as given, with no minimum; more than 7200 is clamped to 7200, not
refused; negative is 400. Create answers 200, also when a repeated `client_id`
returns the existing build.

Creating a build whose target is a configured node needs `via: "node_lease"`
(only that node's worker sends it) or the answer is 409
`node_lease_required`. A target that is not a node is a plain record: the
registry just tracks it.

## Authentication

Send the builds token (`web.builds_token_file`) as `X-Api-Key` (what `call_api`
sends) or as `Authorization: Bearer`. A signed-in web session also works (a
change through a session needs the usual same-origin header). The builds token
opens `/nodes/v1` and the legacy builds routes and nothing else.

Two powers of the builds token are deliberate. It can toggle `builds-pause` on
a node (the operator's switch), and it can send `via: "node_lease"`, which lets
its holder register a record for a node target without being that node's worker.
Neither gives control of a node: a record holds nothing. The node's worker stays
the only hold authority (N1), so a spoofed or stale `node_lease` record is
bookkeeping only and no lease, freeze or queue hold follows from it. A pause
only makes the worker refuse new build leases.

## How another machine registers builds

Pick a `client_id` of the form `bld_` plus 8 to 59 letters, digits, `_` or `-`
and keep it for the whole build:

```sh
curl -sS -X POST "$BASE/nodes/v1/builds" -H "X-Api-Key: $TOKEN" \
  -d '{"client_id":"bld_job00001","target":"buildbox","source":"ci","title":"compile"}'
curl -sS -X POST "$BASE/nodes/v1/builds/bld_job00001/heartbeat" -H "X-Api-Key: $TOKEN" \
  -d '{"progress":"half","log":["line one"]}'
curl -sS -X POST "$BASE/nodes/v1/builds/bld_job00001/finish" -H "X-Api-Key: $TOKEN" \
  -d '{"status":"succeeded","exit_code":0}'
```

Retrying the create with the same `client_id`, target and source returns the
same build. A heartbeat answered 409 `build_ended` means the build was
cancelled, lost or finished: stop. A build that stops heartbeating expires and
is marked lost.

## How the MCP reaches it

Register Nodes as a service in the Navigatorr config (see `config.yaml.example`):

```yaml
services:
  nodes:
    url: "http://localhost:8080"       # this server's web address
    api_key: "<the builds token>"
    api_version: "/nodes/v1"
    openapi_url: "http://localhost:8080/nodes/v1/openapi.json"
```

Then an agent calls `list_services`, `search_api` (for example "cancel build")
and `call_api` on service `nodes`, for example `GET /nodes/v1/builds` or
`POST /nodes/v1/builds/{id}/cancel`.

The MCP tools `build_list`, `build_status` and `build_cancel` were removed. Their
replacements are `GET /nodes/v1/builds`, `GET /nodes/v1/builds/{id}` and
`POST /nodes/v1/builds/{id}/cancel` through `call_api`.

## Upgrading

At start the server copies the build rows and the builds pauses from the main
database into `nodes.db`, once. Whether it has done so is decided only by a flag
in `nodes.db`, written in the same transaction as the copy: a failed import
changes nothing, is logged as an error, and is retried at the next start (rows
already in `nodes.db` are kept, the old ones are added next to them). After a
successful import it never runs again, so records pruned later do not come back.
Old `waiting` rows are imported as they were and cancelled by the first
reconcile pass. The old tables are opened read-only by that import and then left
where they are, unused (they are not dropped).
