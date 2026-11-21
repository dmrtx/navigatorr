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
   encoder and holds the conversion lane, no new conversion starts while it runs;
   the podcast lane is never held or frozen by a build, see
   `docs/transcode/BUILD_LEASES.md`, "Lanes"). The tests in
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
| `GET /builds/{id}/log?after=N&generation=G&wait=S` | Numbered output lines after line N (the last 2000 are kept; a heartbeat may add up to 256). With `wait=S` (whole seconds, 0 to 25, default 0 = answer at once) and no line after N, the call is held until a new line arrives, the build ends, the buffer is dropped or recreated, or S seconds pass, then answers as without `wait` (empty `lines` on a timeout: ask again with the same `after`). At most 64 readers wait at once (429 `too_many_waiters`). Numbers restart whenever the build's in-memory buffer is recreated (server restart, eviction, retention), so the answer carries a `generation`; send it back with `after`. When it differs, or `after` is past the last line, the answer starts from the first line held with `reset: true`: discard what you kept and continue from `next_after`. |

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

## Running Nodes as its own process

By default Nodes runs inside the Transcode server and opens `nodes.db` itself.
It can instead run as a separate binary, `navigatorr-nodes` (`cmd/navigatorr-nodes`),
which is the only process that opens `nodes.db`. The image already contains it
(`/usr/local/bin/navigatorr-nodes`); run it with that entrypoint. The URLs that
workers, `nbuild`, the web UI and `services.nodes` use do not change: the
Transcode server still answers `/nodes/v1` and `/api/maintenance/builds*`, and
forwards them to Nodes after its own authentication.

### What the switch does

The switch is one config key on the Transcode server, `nodes.url`:

- `nodes.url` empty (the default): the registry runs in the server, as before.
- `nodes.url` set: the server never opens `nodes.db`, never runs the legacy
  import and never starts the reconciler. It needs `nodes.key_file` to be a
  readable key of at least 32 characters, and it **refuses to start** if it is
  not, or if `nodes.url` is not an absolute http(s) URL with a host. It never
  falls back to the in-process registry: a second writer on the same file
  because of a broken mount is worse than a server that does not start.

The two modes live in the same binary, so switching is a config change and a
restart.

### Config

One `config.yaml` can serve both binaries (the server ignores `nodes.listen` and
`nodes.transcode_*`; Nodes ignores `nodes.url`). See `config.yaml.example`.

```yaml
database:
  nodes_path: /path/to/<nodes.db dir>/nodes.db   # required by navigatorr-nodes

nodes:
  listen: 127.0.0.1:<port>              # Nodes only; default 127.0.0.1:8097
  key_file: /path/to/nodes.key          # both sides: the service key
  url: http://127.0.0.1:<port>          # Transcode server only: where Nodes is
  transcode_url: http://127.0.0.1:<port>  # Nodes only: the engine; empty = no transcode view
  transcode_key_file: /path/to/transcode.key  # Nodes only: sent to the engine as X-Api-Key
  allow_non_loopback: false
```

`navigatorr-nodes` refuses to start when:

- `nodes.key_file` is missing, unreadable or shorter than 32 characters;
- `database.nodes_path` is empty (it never derives the path from the main
  database, which it never opens);
- `nodes.listen` is not loopback and `nodes.allow_non_loopback` is not true;
- `nodes.transcode_key_file` is set without `nodes.transcode_url`;
- another process holds `nodes.db` (it takes a non-blocking lock on
  `<nodes.db>.lock`; the `.lock` file is never deleted, and the lock works
  between containers that mount the same directory).

Every route but `GET /healthz` needs the service key as `X-Api-Key` or
`Authorization: Bearer`, the OpenAPI spec included. The proxy in the Transcode
server removes the caller's credentials and identity headers and sends only this
key, so a builds token never reaches Nodes. Use a key that is different from
every other key in the deployment.

`nodes.transcode_url` and `nodes.transcode_key_file` let Nodes show each node's
transcode state (paused, mode, held by) by reading
`GET /transcode/v1/node-states` from the engine. The key is the engine API key
(`web.transcode_token_file`, see [transcode/API_V1.md](../transcode/API_V1.md)).
If the engine cannot be read, Nodes shows the nodes without a transcode block; it
never refuses anything because of it (N3).

### Deployment requirements

The repo ships the binary and the image; the deployment must provide the rest.
The deployment must:

- run a second container from the same image with entrypoint
  `navigatorr-nodes`, `read_only`, all capabilities dropped,
  `no-new-privileges`, a tmpfs `/tmp`, and a listen address on loopback only;
- run it as the user that owns `nodes.db` and its directory. Without
  `CAP_DAC_OVERRIDE`, that user reads only what it owns: the directory of
  `nodes.db` (mode 0700), `nodes.db` (0600), the service key and the copy of the
  engine key it mounts (0600) must all belong to it;
- mount only `config.yaml` (read-only), the two key files (read-only) and the
  directory of `nodes.db` (read-write). The directory holds `nodes.db` and
  nothing else of the server: never the directory of the main database. Put it
  in its own subdirectory and point `database.nodes_path` there, for both
  containers, from one source of truth;
- set `stop_grace_period` to at least 40 s (see "Restarts");
- use `GET /healthz` as the health check, for example
  `wget -q -O - http://127.0.0.1:<port>/healthz`. It answers 200 `ok` only when
  `nodes.db` is open and its `meta` table can be read; otherwise 503. It does
  not depend on Transcode;
- start Nodes only when the switch variable that enables the Nodes container is
  on, and remove the container (not just stop it) when it is off. Then a host
  reboot cannot bring it up against the Transcode server that has `nodes.url`
  empty and owns the lock;
- stop containers imperatively during the cutover (stop, wait for `exited`, run
  `check`, change the config, start), not through handlers that restart in
  another order.

### Restarts

Transcode and Nodes can start in either order. The server's client is lazy and
Nodes treats the engine view as optional. While Nodes is not up, the builds
routes answer 503 and the workers fail open (see N3 below).

Nodes shuts down on SIGTERM or SIGINT: it stops taking new requests, lets those
in flight finish for up to 35 s (a build-log `wait` holds for up to 25 s), then
always closes the server, stops the reconciler and closes the database, so the
database ends without a `-wal` file and the lock is released. It logs
`shutting down: draining requests in flight...` and, on a clean exit,
`nodes stopped cleanly`. Never SIGKILL it while it is draining: let the grace
period run out first.

### Rotating the service key

1. Write the new key to the key file (0600, at least 32 characters). Both the
   server and Nodes read it once at start.
2. Restart Nodes and the Transcode server.

Until both have restarted the keys disagree, Nodes answers 401, and the proxy
turns that into a 503 with `Retry-After: 5` and logs an ERROR
(`the Nodes service answered 401/403 ... its key does not match nodes.key_file`).
A 401 never reaches a worker or the UI. Workers tolerate those few seconds.

### What the server does when Nodes is down

- Builds routes (`/nodes/v1/*`, `/api/maintenance/builds*`, including
  `GET /api/maintenance/builds/nodes`) answer 503 with `Retry-After: 5` and
  `{"error":"the builds registry is not reachable","code":"unavailable",...}`,
  and the proxy logs a WARN (rate limited). Never a 400, 404 or 409 that Nodes
  did not send, and never `builds_paused: true` because the registry could not be
  read.
- If Nodes fails after it started to answer, the proxy aborts the connection;
  the caller sees a network error.
- The workers UI and `worker_health` show **Builds registry unavailable**
  (`builds_unavailable: true`), never "paused". `worker_pause` with
  `builds_paused` returns an error ("builds registry unreachable; nothing
  changed", or "did not answer; the pause may or may not have changed, check
  again").
- Workers keep granting leases (N3).

### The two invariants

- **N2.** The transcode engine (`action`, `transcode`) and the Nodes registry
  never import each other. Only `main` and `internal/nodeswire` join them, by
  interfaces. This holds across processes too: `internal/nodesclient` and
  `internal/nodesproxy` do not import `nodes`, `action` or `transcode`, and
  `cmd/navigatorr-nodes` does not import the engine; node pauses cross as HTTP
  (`/transcode/v1/node-states`). Build records only feed screens and tools,
  never dispatch, scheduling, draining or freezing. `internal/importguard`
  proves it.
- **N3.** Nodes being down, hung or restarting never stops or refuses a worker
  lease. Everything fails open: a worker refuses a lease only when Nodes
  answers that builds are paused (a 409 `builds_paused` on registration, or
  `builds_paused: true` on the pause read), and every other answer or error (503,
  timeout, reset) grants it. The routes workers call have a 3 s timeout, so a hung Nodes
  costs a worker at most 3 s and is treated as "not reachable".

See [transcode/BUILD_LEASES.md](../transcode/BUILD_LEASES.md) for the worker side.

## Cutover runbook

Moves the registry from the Transcode server to the separate process. Follow it
in order and do not start a step until the previous one's check passes. Every
step that stops a container waits until it is `exited`. Run `navigatorr-nodes
check` as the user that owns the files (see the deployment requirements), in a
container of the image, never as another user from the host: it would create
files with the wrong owner.

`<nodes.db>` is the path in `database.nodes_path`.

**Step 0. Quiet window.** Before the cutover, and again before steps 2, 4 and
any rollback:

- no running build in the registry (`GET /nodes/v1/builds?status=running` is
  empty);
- no `nbuild` starting on any node;
- no lease on any worker: `held_by` is empty for every node in the Transcode
  state (`GET /transcode/v1/node-states`, or the workers UI). This comes from the
  worker, so it also covers a lease that was never registered.

If one appears, wait for it to end. When you compare responses below, compare
meaning, not bytes: the set of build ids with their `status`, `builds_paused` per
node, and the node names. Ignore timestamps and the `transcode` block.

*Check:* all three are empty.

**Step 1. Image with the new binary, `nodes.url` still empty.**

1. Deploy an image that contains `navigatorr-nodes` and the proxy/client code,
   with `nodes.url` empty. The server behaves as before.
2. Check the `legacy_import` mark exists in `nodes.db`. The separate binary
   never runs the legacy import and does not read the main database, so the
   cutover is refused if it was never done. It is created by the in-process
   registry the first time that import succeeds; if the mark is missing, run the
   current server once (it logs an ERROR if the import fails) and check again.
   The `check` in step 2 reports it as `legacy_import: ...` or `(missing)`.
3. Save the baseline: the responses of `GET /nodes/v1/nodes`,
   `GET /nodes/v1/builds` and `GET /api/maintenance/builds/nodes`, with the
   builds token.

*Check:* the server is healthy, the baseline is saved.

**Step 2. Move `nodes.db` into its own directory.** Skip the move if it already
is in a directory only Nodes will mount.

1. Repeat step 0.
2. Stop the Transcode server and wait for `exited`.
3. In a container of the image, as the file owner, run
   `navigatorr-nodes check --path <nodes.db>`. It must print `ok`. If it fails
   because `<nodes.db>-wal` exists, run it again with `--checkpoint` (everything
   is stopped, so it can fold the WAL in). Do not move anything while a `-wal`
   or `-shm` file exists.
4. Make a backup of the closed `nodes.db` and its `sha256` in a place outside
   both directories.
5. Move `nodes.db` to the new directory. Run `check --path <new nodes.db>`: it
   must print `ok`, and its `file owner` and `dir owner` lines must show the
   owner Nodes will run as, with modes 0600 and 0700. Confirm with `stat`.
6. Set `database.nodes_path` to the new path in the config and start the
   Transcode server (still `nodes.url` empty). Confirm the `.lock` it created in
   the new directory has the right owner, and that nothing is using the old
   directory.

*Check:* the baseline responses are the same as in step 1.

**Step 3. Rehearse Nodes against an empty database.** This checks keys, ports and
mounts before touching the real data.

1. Start `navigatorr-nodes` with the real config but `database.nodes_path` in
   another, empty directory (Nodes creates the database).
2. `GET /healthz` returns 200 `ok`; any other route without the key returns 401
   (also the OpenAPI spec); with the key it returns 200.
3. Nodes can read the engine: with `nodes.transcode_url` set, a `GET /nodes/v1/nodes`
   with the key shows a `transcode` state for the nodes (it is absent if the
   engine view cannot be read).
4. The owners and modes of the mounted key files and directory are right.
5. Stop it, wait for `exited`, and delete the test database.

*Check:* all of the above pass, and the throwaway directory is gone.

**Step 4. Cut over (short window).**

1. Repeat step 0.
2. Stop the Transcode server and wait for `exited`. Workers keep their leases and
   fail open.
3. Run `navigatorr-nodes check --path <nodes.db>`: `ok`, no `-wal`, lock free.
4. Start `navigatorr-nodes` on the real directory. Wait for `GET /healthz` 200.
   With the key, `GET /nodes/v1/builds` must equal the baseline.
5. Set `nodes.url` (and `nodes.key_file`) in the server's config and start the
   Transcode server.

*Check:* in the server log, `builds registry is the separate Nodes process at ...`
appears and no `nodes database at ...` line (that one is only logged by the
in-process registry); there is no ERROR about node names
(`node names differ between this server ... and the builds registry`) or about
the service key. A second Nodes started on the same directory exits with
`nodes.db is held by another process`.

**Step 5. Verify, still in the quiet window.**

1. The baseline is identical.
2. Run a real build with `nbuild` on a node: it registers, heartbeats, shows its
   log (including a log read with `wait` up to 25 s) and finishes.
3. Pause and resume builds on a node, from the UI and with `worker_pause`
   (`builds_paused`): new builds get 409 `builds_paused`, then are accepted
   again.
4. `call_api` on service `nodes` works, and the OpenAPI spec is reachable
   through the server.
5. The workers UI shows no error.

*Check:* all five pass.

**Step 6. Failure injection.**

1. Start a build with `nbuild` on a node, then stop Nodes.
2. The workers UI and `worker_health` show **Builds registry unavailable**, never
   "paused".
3. A worker still grants a new lease (start a build on a node with its
   own worker, for example, and see it run).
4. `GET /nodes/v1/builds` answers 503 with `Retry-After: 5`;
   `worker_pause` with `builds_paused` returns a visible error.
5. The `nbuild` already running keeps going or recovers. This cannot be tested
   from this repo; test the tool by hand here, before any cleanup.
6. Start Nodes again. Heartbeats resume, and leases still alive register again
   within the worker's 5 s retry.

A lease that started and ended while Nodes was down is never recorded. That is
accepted: the registry is history and the worker is the authority.

*Check:* all of the above, and the baseline is the same once Nodes is back.

## Rollback

### Before the in-process registry is removed from the server

Today the in-process registry still exists in the server, so going back is
configuration only. Do this in the quiet window (step 0):

1. Switch off the variable that enables the Nodes container, and empty `nodes.url`
   in the server config.
2. Stop Nodes first and wait for `exited` (clean shutdown). Run
   `navigatorr-nodes check --path <nodes.db>` and require `ok`, no `-wal` and the
   lock free.
3. Restart the Transcode server. It opens the same `nodes.db` in the same
   directory: there is no copy and never two writers.
4. Check that a build created while Nodes ran is readable from the server
   (`GET /nodes/v1/builds/<id>`), the baseline matches, and the log shows
   `nodes database at <nodes.db>`.

Rehearse this once after step 5 and cut over again.

### After the cleanup that removes it

Once the in-process registry and the legacy import are gone from the server,
going back is not only configuration. The cleanup's pull request records the
last image tag from before it. To roll back, do the same sequence as above
(switch off the Nodes container, stop it and check it with `check`, require the
lock free), deploying that older image with `nodes.url` empty.

That only works if `nodes.db` is still readable by the older image. No schema
change to `nodes.db` may ship around the cleanup unless it is backward
compatible; one that is not closes this way back and must say so in its pull
request.

### Losing the Nodes volume

Restore the backup from step 2 after checking its `sha256` and running `check`
on it. Builds recorded after that backup are lost from the history. Workers
register their live leases again (a heartbeat answered 404 means "it ended", and
the next report registers the lease anew). The registry is history; the worker is
the authority.

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

The flag is checked again inside the import's transaction, so two imports at
once (two processes on one file) copy only once. On a retry, the old builds are
placed before the ones `nodes.db` already holds, so the history pages after the
newer builds. One case is accepted on purpose: if the first import failed, an
operator resumed a node, and then the retry ran, the node's old builds pause
comes back (the registry keeps a row only while a node is paused, so there is
nothing to tell the import to skip it). It fails safe: the node refuses builds
until it is resumed again, and the retry logs the name of each pause it brings
back.

Running Nodes as its own process changes this: `navigatorr-nodes` never runs
this import and never opens the main database. The `legacy_import` mark must
already be in `nodes.db` before the cutover (the cutover runbook checks it with
`navigatorr-nodes check`, which fails when the mark is missing). With
`nodes.url` set, the Transcode server does not run the import either. If the
mark is missing, start the server in its in-process mode once, let the import
succeed, and then follow the runbook.
