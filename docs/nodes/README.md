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
  listen: 127.0.0.1:<nodes port>        # Nodes only; default 127.0.0.1:8097
  key_file: /path/to/nodes.key          # both sides: the service key
  url: http://127.0.0.1:<nodes port>    # Transcode server only: where Nodes is
  transcode_url: http://127.0.0.1:<server port>  # Nodes only: the engine; empty = no transcode view
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
- give both containers one network namespace that neither of them owns (both
  on the host network, or both joining a third container that is always up), so
  `127.0.0.1` in `nodes.url` and `nodes.transcode_url` reaches the other
  container. Never make one join the other's namespace: the runbook stops and
  starts them independently, and the joined one cannot run while the other is
  stopped. On separate networks loopback is each
  container's own, every builds call answers 503 and Nodes shows no transcode
  state. If they cannot share one, Nodes needs `nodes.allow_non_loopback: true`
  and a listen address on a private network that only the server can reach;
- run it as the user that owns `nodes.db` and its directory. Without
  `CAP_DAC_OVERRIDE`, that user reads only what it owns: the directory of
  `nodes.db` (mode 0700), `nodes.db` (0600), the service key and the copy of the
  engine key it mounts (0600) must all belong to it;
- run the Transcode server as that same user. It opens `nodes.db` itself before
  the cutover and after a rollback, and the `.lock`, `-wal` and `-shm` files it
  creates must stay readable by Nodes;
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
files with the wrong owner. `check` exits 0 and prints `ok` only when the lock
is free, there is no `-wal` file (unless `--checkpoint` folded it in), the
integrity check passes and the `legacy_import` mark is present.

`<nodes.db>` is the path in `database.nodes_path`.

**Baseline.** Step 1 saves the responses of `GET /nodes/v1/nodes`,
`GET /nodes/v1/builds` and `GET /api/maintenance/builds/nodes`. "Matches the
baseline" means: the same node names, the same `builds_paused` per node, and
every build of the baseline still present with the same `status`. Builds that
the steps below create are expected on top of it. Compare meaning, not bytes:
ignore timestamps and the `transcode` block.

**Step 0. Quiet window.** Before the cutover, and again before steps 2 and 4
and before any rollback:

- stop whatever starts builds on its own (scheduled `nbuild` runs, scripts) and
  start none by hand until the window ends;
- no running build in the registry (`GET /nodes/v1/builds?status=running` is
  empty);
- no lease on any worker: `held_by` is empty for every node in the Transcode
  state (`GET /transcode/v1/node-states`, or the workers UI). This comes from the
  worker, so it also covers a lease that was never registered.

If one appears, wait for it to end. The window is a precaution, not a lock:
nothing stops a lease that starts anyway. Such a lease is never stopped or lost
on the node (N3); while the registry is down it may just go unrecorded, or
register again when Nodes is back (see step 6).

When Nodes is down (a rollback after a failure), the registry cannot be read:
rely on `held_by` alone.

*Check:* no running build, `held_by` empty on every node, and the automatic
builds are stopped.

**Step 1. Image with the new binary, `nodes.url` still empty.**

1. Deploy an image that contains `navigatorr-nodes` and the proxy/client code,
   with `nodes.url` empty. The server behaves as before.
2. Save the baseline, with the builds token.

The `legacy_import` mark cannot be read here (the server holds the lock); the
`check` in step 2 requires it. If that `check` reports `legacy_import: (missing)`,
start the server once more in this mode (it runs the import and logs an ERROR if
it fails), and repeat step 2 from the start.

*Check:* the three baseline requests answer 200 and are saved.

**Step 2. Check, back up, and move `nodes.db` into its own directory.** Only the
move (item 5) is skipped when `nodes.db` already is in a directory only Nodes
will mount; the check and the backup are never skipped.

1. Repeat step 0.
2. Stop the Transcode server and wait for `exited`.
3. Run `navigatorr-nodes check --path <nodes.db>`. It must print `ok`. If it
   fails because `<nodes.db>-wal` exists, run it again with `--checkpoint`
   (everything is stopped, so it can fold the WAL in). Do not copy or move
   anything while a `-wal` or `-shm` file exists.
4. Make a backup of the closed `nodes.db` and its `sha256` in a place outside
   both directories.
5. Move `nodes.db` to the new directory and set `database.nodes_path` to the
   new path in the config.
6. Run `check --path <nodes.db>`: it must print `ok`, and its `file owner` and
   `dir owner` lines must show the owner Nodes will run as, with modes 0600 and
   0700. Confirm with `stat`.
7. Start the Transcode server (still `nodes.url` empty). Confirm the `.lock` it
   created next to `nodes.db` has the same owner, and that no `nodes.db*` file
   appears again in the old directory.

*Check:* the baseline matches.

**Step 3. Rehearse Nodes against an empty database.** This checks keys, ports,
mounts and the lock before touching the real data. Use a throwaway directory
with the same owner and modes as the real one.

1. Start `navigatorr-nodes` with the real config but `database.nodes_path` in
   the throwaway directory (Nodes creates the database there).
2. `GET /healthz` returns 200 `ok`. Without the key, `GET /nodes/v1/nodes` and
   `GET /nodes/v1/openapi.json` return 401; with the key both return 200.
3. Nodes can read the engine: the nodes come from the configured topology, not
   from the database, so `GET /nodes/v1/nodes` lists them even here, and with
   `nodes.transcode_url` set each one carries a `transcode` block. If none
   does, the engine cannot be read (wrong URL, key or network).
4. Start a second `navigatorr-nodes` on the same throwaway directory with another
   `nodes.listen` port: it exits with `nodes.db is held by another process`.
   Remove that container.
5. `stat` the mounted key files and the directory: they belong to the user
   Nodes runs as, with modes 0600 and 0700.
6. Stop Nodes, wait for `exited`, remove the container, and delete the
   throwaway directory with everything in it.

*Check:* all of the above pass, and the throwaway directory is gone.

**Step 4. Cut over (short window).** `nodes.key_file` is already in the config
(step 3 needed it); only `nodes.url` changes.

1. Repeat step 0.
2. Stop the Transcode server and wait for `exited`. Workers keep their leases and
   fail open.
3. Run `navigatorr-nodes check --path <nodes.db>`: `ok` (with `--checkpoint` if
   a `-wal` was left, as in step 2).
4. Switch on the variable that enables the Nodes container and start
   `navigatorr-nodes` on the real directory. Wait for `GET /healthz` 200. Ask
   Nodes directly (its `nodes.listen` address, with the service key; the server
   is still stopped): `GET /nodes/v1/builds` has every baseline build with the
   same `status`.
5. Set `nodes.url` in the server's config and start the Transcode server.

*Check:* in the server log, `builds registry is the separate Nodes process at ...`
appears and no `nodes database at ...` line (that one is only logged by the
in-process registry); there is no ERROR about node names
(`node names differ between this server ... and the builds registry`) or about
the service key. Through the server, the three baseline requests match the
baseline.

**Step 5. Verify, still in the quiet window.**

1. Run a real build with `nbuild` on a node: it registers, heartbeats, shows its
   log (including a log read with `wait` up to 25 s) and finishes.
2. On a node whose builds are not paused, pause and resume builds, from the UI
   and with `worker_pause` (`builds_paused`): new builds get 409
   `builds_paused`, then are accepted again. Leave it resumed, as in the
   baseline.
3. `call_api` on service `nodes` works, and the OpenAPI spec is reachable
   through the server.
4. The workers UI shows no error.

*Check:* all four pass and the baseline matches (the builds of items 1 and 2
are extra).

**Step 6. Failure injection.**

This needs two nodes, or one with a worker free for a second lease.

1. Start a build with `nbuild` on a node, then stop Nodes.
2. The workers UI and `worker_health` show **Builds registry unavailable**, never
   "paused".
3. A worker still grants a new lease (start a build on the other node, for
   example, and see it run).
4. `GET /nodes/v1/builds` answers 503 with `Retry-After: 5`;
   `worker_pause` with `builds_paused` returns a visible error.
5. The `nbuild` already running keeps going or recovers. This cannot be tested
   from this repo; test the tool by hand here, before any cleanup.
6. Start Nodes again. Heartbeats resume, and leases still alive register again
   within the worker's 5 s retry.
7. Hang it instead of stopping it: freeze the Nodes container (pause it, do not
   stop it) and start another lease. It is granted after at most the 3 s
   timeout. Unfreeze the container.
8. Wait for the leases started here to end.

A lease that started and ended while Nodes was down is never recorded. That is
accepted: the registry is history and the worker is the authority.

*Check:* all of the above, and once Nodes is back the baseline matches; the
builds started in this step are present or, if they started and ended while
Nodes was down, absent.

**Step 7. Rehearse the rollback, still in the quiet window.** Do the rollback
below once (before the cleanup), check it, then cut over again from step 4.

*Check:* the rollback's checks pass, and step 4's check passes again.

End the quiet window: start the automatic builds again.

## Rollback

### Before the in-process registry is removed from the server

Today the in-process registry still exists in the server, so going back is
configuration only. Change the config only after Nodes is gone, so no restart in
between can open `nodes.db` from the server while Nodes holds it:

1. Repeat step 0.
2. Stop Nodes and wait for `exited` (clean shutdown). Switch off the variable
   that enables the Nodes container and remove the container, so a reboot cannot
   bring it back.
3. Stop the Transcode server and wait for `exited`. Run
   `navigatorr-nodes check --path <nodes.db>` and require `ok` (with
   `--checkpoint` if Nodes died and left a `-wal`).
4. Empty `nodes.url` in the server config and start the Transcode server. It
   opens the same `nodes.db` in the same directory: there is no copy and never
   two writers.
5. Check that a build created while Nodes ran is readable from the server
   (`GET /nodes/v1/builds/<id>`), the baseline matches, and the log shows
   `nodes database at <nodes.db>`.
6. Unless you are rehearsing (step 7), end the quiet window: start the
   automatic builds again.

### After the cleanup that removes it

Once the in-process registry and the legacy import are gone from the server,
going back is not only configuration. The cleanup's pull request records the
last image tag from before it. To roll back, follow the same steps, deploying
that older image (with `nodes.url` empty) in item 4.

That only works if `nodes.db` is still readable by the older image. No schema
change to `nodes.db` may ship around the cleanup unless it is backward
compatible; one that is not closes this way back and must say so in its pull
request.

### Losing the Nodes volume

Back up the directory of `nodes.db` on the same schedule as the main database,
from a stopped Nodes or with SQLite's online backup (never a plain copy of a
database that is open). To recover: stop Nodes and wait for `exited`; check
the backup's `sha256`; remove every `nodes.db*` file left in the directory (an
old `-wal` would be replayed over the restored copy), keeping the directory's
owner and mode 0700; restore the newest backup (or the one from step 2) as
`nodes.db` with mode 0600; run `check --path <nodes.db>`; start Nodes. Builds recorded
after that backup are lost from the history, and a builds pause set or cleared
after it comes back as it was then: check the pauses of every node before
resuming work. Workers
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
