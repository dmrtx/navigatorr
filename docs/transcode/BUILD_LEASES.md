# Build leases: local arbitration between builds and transcodes

On a Mac transcode node the worker is the single decider between a build and
an encode. A build asks the local worker for a lease over a Unix socket; the
worker freezes the encoders (or, for a low priority build, waits for an idle
node), holds its queue, and expires the lease by itself when the heartbeats
stop. The build still runs its own command in the caller's shell.

## Configuration (worker config)

| key | meaning |
| --- | --- |
| `build_node` | this machine's name in the server's node list. Without it the worker offers no leases. |
| `build_socket` | socket path. Default `<state_dir>/.builds/lease.sock` (directory 0700, socket 0600). A Unix socket path must stay under about 100 bytes. |
| `builds_url`, `builds_token_file` | server base URL and a builds-only token file, so the worker can report leases to the server. Optional: without them leases work and the server does not see them. |

## Socket protocol

HTTP over the socket, in the shape of the server's builds API, so a client that
speaks that one only changes where it sends. No token: reaching the socket is
the permission.

- `GET /v1/node` gives `{"node": NAME, "leases": true}`.
- `POST /api/maintenance/builds` creates a lease (`target` must be the node,
  else 409 `wrong_node`). 200 with the build record; `status` is `running`
  (normal, encoders already frozen) or `waiting` (low priority). 409
  `builds_paused` when the operator paused builds on the node.
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
2. `running`: the queue is held. A normal lease (`suspend`, `immediate`) also
   freezes the encoders; a `graceful` or low one does not.
3. Ended: `succeeded`, `failed`, `cancelled`, or `lost` when the heartbeats
   stopped past `ttl_seconds`. Ended leases stay on record for an hour so a late
   heartbeat is told it is over.

The state is `build-leases.json` in the state directory, written atomically.
Runners read it like the suspend record, with the expiry evaluated at read time:
an expired lease never freezes anything, even before the worker marked it lost.
The freeze is the union of the server's suspend record and the leases.

## How the server learns about it

In the background, never on the lease's path: the worker registers the lease
with the existing builds API (so Queue rows, `build_list`, `build_status`,
`build_cancel`, Pause builds and `/builds/nodes` keep working), forwards each
heartbeat's progress and log lines, and finishes it (a lost lease is finished as
`failed`). The one synchronous call is the registration of a normal lease, with a
3 second timeout: a 409 `builds_paused` refuses the lease, anything else
(unreachable, error) grants it and the registration is retried in the
background. A build cancelled in the server answers the forwarded heartbeat as
cancelled; the lease then ends and the build learns it from its next heartbeat.

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
- Low lease promoted while the server is down: granted without the pause check.
