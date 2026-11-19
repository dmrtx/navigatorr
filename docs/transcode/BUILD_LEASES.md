# Build leases: local arbitration between builds and transcodes

**The node's worker is the only authority on who occupies a node.** The server
holds no build hold: its builds are a pure record (create, heartbeat,
finish/cancel, expiry to `lost`, history and prune, logs) plus the per-node
"builds paused" switch. A build record, running or not, never changes a pause,
a dispatch, a requeue, a hold or a freeze on the server. The worker reports its
active leases (id, title, source, status, started_at) in its scheduler
observation; the server uses that list only to word `suspended_by`.
The invariants N1 to N3 are in the doc comment at the top of `action/build_records.go` and of package `nodes` (`nodes/doc.go`); the worker enforces N1 and N3 (tests in `internal/transcodeworker`).

A create that targets a configured node must come from that node's worker: its
registration carries `"via": "node_lease"`. Without it the server answers 409
`node_lease_required` ("run nbuild on that node"). A target that is not a
configured node is a plain record and needs no `via`.

`via` is a marker, not authentication. The builds token is what authenticates a
client; `via` only says "this record is the worker's lease", so a client that
still asks the server to hold a node (an old `nbuild`, a script) is told where
to run. Anyone holding the builds token can send it, and that only creates a
record: the server holds nothing for it.

On the server, "held by builds" and `suspended_by` come only from the leases the
worker reports running in its observation (at most a few seconds old; an
unreachable worker reports none). A build record, running or not, is history and
a list entry: a running record with no reported lease does not show the node as
held. The POST and the GET of `/api/maintenance/worker-pause` answer in the same
shape, records and hold included.

On a Mac transcode node the worker is the single decider between a build and
an encode. A build asks the local worker for a lease over a Unix socket; the
worker freezes the encoders (or, for a low priority build, waits for an idle
node), holds its queue, and expires the lease by itself when the heartbeats
stop. The build still runs its own command in the caller's shell.

## Configuration (worker config)

| key | meaning |
| --- | --- |
| `build_node` | this machine's name in the server's node list. Without it the worker offers no leases. |
| `build_socket` | socket path. Default `<state_dir>/.builds/lease.sock` (the worker creates that directory 0700; socket 0600; the length limit below applies to it too, so `<state_dir>` must stay under about 70 bytes). A path of your own must be in an existing directory that belongs to the worker's user and that others cannot write to; the worker never changes it and refuses to start the socket otherwise. A Unix socket path must stay under about 100 bytes, and its directory under about 88 bytes less the length of the socket's name (for `lease.sock`, about 78: the socket is bound in a temporary directory beside it named after it, given its mode there and then linked into place; a file there that is not a socket is never replaced). The worker holds `<build_socket>.lock` beside it while it serves the socket, so a second worker on the same path starts no socket and never touches the first one's (a `build_socket` ending in `.lock` is refused). |
| `build_socket_group` | a group whose members may also use the socket: the socket becomes 0660 with that group. Needs `build_socket` in a directory that group can enter (for example owned by the worker's user, group set to it, 0750). The worker's user must be in the group. An unknown group, a group the worker's user cannot set, or a group without `build_socket`, starts no socket and leaves an earlier socket file as it was (the worker runs without leases and logs why). Reaching the socket is the permission, so the group's members can hold builds on this node. |
| `builds_url`, `builds_token_file` | server base URL (`http://host:port`) and a builds-only token file, so the worker can report leases to the server. A URL that already ends with `/api/maintenance/builds` is read as the same server. Optional: without them leases work and the server does not see them. A registration the server refuses or cannot be reached for is logged once for each cause (`build lease … not registered with the server (…)`), and the lease still runs. |

## Socket protocol

HTTP over the socket, in the shape of the server's builds API, so a client that
speaks that one only changes where it sends. No token: reaching the socket is
the permission.

- `GET /v1/node` gives `{"node": NAME, "leases": true}`.
- `POST /api/maintenance/builds` creates a lease (`target` must be the node,
  else 409 `wrong_node`). Creates run one at a time. 200 with the build record;
  `status` is `running` (normal) or `waiting` (low priority). A normal lease is
  answered only once every running encode reported itself frozen (up to 10 s);
  if one did not, the lease ends as cancelled and the answer is 503, with no
  call to the server on that path. 409 `builds_paused` when the operator paused
  builds on the node. An optional `id` (`bld_` plus 8 to 56 letters, digits,
  `_` or `-`) names the lease: asking again returns what that id got (the same
  lease, the same 503), never `running` for a freeze not yet confirmed.
- `POST /api/maintenance/builds/{id}/heartbeat` extends the lease and carries
  `progress`, `ttl_seconds` and `log` lines. 409 with `{"build": {...}}` when
  the lease is over (`cancelled`, `lost`, ...), 404 when unknown.
- `POST /api/maintenance/builds/{id}/finish` ends it (`succeeded`, `failed`,
  `cancelled`, `exit_code`); finishing an ended lease answers 200 unchanged.
- `GET /api/maintenance/builds/{id}` reads one.

## Lease lifecycle

1. `waiting` (low only) until the worker has no running or queued job, no hold
   or suspend and no other lease; the worker promotes the oldest one under the
   capacity lock, so no job starts between the check and the grant.
2. `running`: the queue is held, every process start is refused by the one
   worker gate (`queueHeld()` for new jobs; post-encode recovery and publication
   have their own gates, see "What a lease gates"),
   and queued jobs that have not started carry the hold refusal on their
   record so the coordinator takes them back. A normal lease (`suspend`,
   `immediate`) also freezes the encoders; a `graceful` or low one does not.
   `immediate` is an alias of `suspend`: it is stored and answered as given,
   and the worker never requeues or cancels anything for it.
3. Ended: `succeeded`, `failed`, `cancelled`, or `lost` when the heartbeats
   stopped past `ttl_seconds`. A lease found past its expiry
   by a read or a finish is marked lost on the spot; a finish then cannot claim
   success. A new normal lease's ttl starts when its freeze is confirmed. Ended
   leases stay on record for an hour once the server has the end (or when there
   is no server), at most a day otherwise.

### What a lease gates

A lease is not an operator pause, so the gate differs by path:

- New jobs: held by an operator hold or drain, a suspend, or a lease
  (`queueHeld()`).
- Post-encode recovery (`ResumePostEncode`): held by a running lease only.
  An operator pause or a drain still lets a finished encode go on to be saved.
- Publication of a retained candidate (`PublishRetained`): held by the drain
  hold or a running lease. An operator suspend does not hold it.

An operator suspend does not stop post-encode copy/verify or publication
(intended): only leases and the drain hold do.

Both post-encode paths check the lease once before the capacity lock and again
under it. Leases are granted under that same lock, so a lease granted between
the first check and the lock is seen, and no process starts after the grant.
`PublishRetained` takes the lock without waiting: if the node is busy it
refuses with a transient "the node is busy, ask again" instead of blocking
under the job lock.

A server that still has `waiting` rows from before this change (the server
used to queue low builds) cancels them on its next reconcile pass, with the
progress text "server-side waiting builds were removed; retry" and a log line `build ID on TARGET: ...`. Nothing promotes a
waiting row any more.

The state is `build-leases.json` in the state directory, written atomically.
Runners read it like the suspend record, with the expiry evaluated at read time:
an expired lease never freezes anything, even before the worker marked it lost.
The freeze is the union of the server's suspend record and the leases.
Only a record that is empty or not JSON is replaced by an empty one (by
whichever lease call reads it first, or the tick, which runs even if the
socket cannot open); any other read error fails
closed and is retried. A worker without `build_node` ignores the file.

## How the server learns about it

In the background, never on the lease's path: the worker registers the lease
with the existing builds API (so Queue rows, the Nodes API (`GET /nodes/v1/builds`,
`POST /nodes/v1/builds/{id}/cancel`, reached through `call_api` on the service `nodes`), Pause builds and
`/builds/nodes` keep working; the MCP tools `build_list`, `build_status` and `build_cancel` no longer exist), forwards each
heartbeat's progress and log lines (with the lease's ttl, and an idle lease is
kept alive at a third of it, at most every 20 s), and finishes it (a lost lease is finished as
`failed`). The one synchronous call is the registration of a normal lease, with a
3 second timeout: a 409 `builds_paused` refuses the lease, anything else
(unreachable, error) grants it and the registration is retried in the
background. A build cancelled in the server answers the forwarded heartbeat as
cancelled. A 404 (the server forgot the build) only registers it again. The
tick and the report pass run apart, every second. A low lease is registered as
`normal` priority with `graceful` mode, only once the worker promotes it: the
server never queues a build, so a low create through the server comes back
`running`. Up front, though, a low lease asks the server whether builds are
paused on the node (a read, nothing registered): paused refuses it with 409
`builds_paused` and stores nothing; an unreachable server fails open and the
lease waits; a lease already waiting is not affected by a later pause. The
registration always carries `"via": "node_lease"`.

## Deploy order and version skew

Deploy the workers and the server together. The server decodes request bodies
with `DisallowUnknownFields`, on the old and on the new version:

- A new worker to an old server: the registration carries `via`, which the old
  server does not know, so it answers 400. The report pass logs that cause once
  and keeps trying.
- An old worker to a new server: the registration has no `via`, so the new
  server answers 409 `node_lease_required`.

In both cases the lease is still granted locally, because only the builds pause
ever stops a lease; the server's record merely lags until both sides are
deployed. Nothing is lost on the node.

## Failure modes

- Worker restart mid-lease: the freeze and the queue hold are in the record, so
  they stay; leases still alive get at least 30 s of grace, the socket comes
  back at the same path and the build's next heartbeat succeeds. A lease that
  expired while the worker was down stays expired.
- The build is killed (kill -9): no more heartbeats, the lease expires after its
  ttl, the encoders thaw, the worker finishes it in the server as `failed`.
- Server unreachable: leases are granted, heartbeated and expired as usual;
  reports are retried and the server's record catches up. The server's own
  expiry may mark the build lost while the worker still holds it.
- Low lease promoted while the server is down: granted; the pause was checked (and failed open) when it was asked for.
- Low lease promoted, then the registration is refused as paused (the operator
  paused builds after the lease asked): the lease keeps running, it is marked
  unrecorded and is not registered again, so the worker does not retry a refusal
  forever. It is not shown in the server's records. A pause never stops a lease
  that already runs.
- `RetentionBlocked` and `RoomNote`, which a held queue writes on queued jobs,
  are cleared when the lease ends.
- A normal lease is registered with the server before it is stored (the pause
  check). If storing then fails, or the answer to the registration is lost, that
  server build exists only there and expires by its ttl. A lease that ends
  without a freeze stays on record, so the reporter finishes it with retries.
- The socket has no token and no peer check: the 0700 directory and the 0600
  socket are the whole permission.
