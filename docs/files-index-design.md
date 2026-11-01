# Files tab: an index with change tokens (design, not built)

Status: proposal for the owner to choose from. The quick fixes in this change
(a short-lived copy of the *arr collections, file marks that do not wait for the
read, stat of only the page's entries, `Server-Timing`) are built and described in
`MAINTENANCE_UI.md`. Nothing here is implemented.

## Where the time went

Measured on synthetic data (a CI-class container, not the homelab; the shape is
what matters, the milliseconds will be larger on a small server):

| Step on every Files request | Cost | Notes |
|---|---|---|
| Radarr `/movie` or Sonarr `/series`, whole collection | the *arr's own time + 4.7 MB transfer | before: once per 100-item page |
| Parse and project 796 titles | 30-40 ms | before: once per page |
| `fileStatuses()` SQL join over every batch item ever run | 350-400 ms per 10,000 files, store mutex held | before: every 3 s while the tab polls; it also blocks worker heartbeats while it runs |
| Folder listing by name | one `EvalSymlinks` (an `lstat` per path component) + two `stat` per entry, before slicing to 100 | before: every entry of the folder, not just the 100 shown; each is a round trip on SMB |

The query itself is already indexed (`EXPLAIN` shows one scan of the items and
primary-key / index seeks for the rest); its cost is rows times JSON columns,
so it grows with history. Guarding the JSON parse with a substring test made it
slower on this driver, and was dropped.

## What "ask for what changed" would need

One idea, three different sources of change, three different answers:

### 1. Library titles (*arr): ETag, not a delta

The whole projected list is small: 796 titles at ~150 bytes is ~120 KB, ~25 KB
gzipped. A delta protocol for that is more machinery than the data deserves.

- `GET library?service=radarr&view=index` returns every title once, compact
  (id, title, year, path, size, episode count) with `ETag: "<rev>"`, where `rev`
  is a counter that moves when the projected list differs from the last one.
- The app sends `If-None-Match`; unchanged is a 304 with no body. The app holds
  the list and does search, sort and paging locally, so typing in the search box
  makes no request. (This also removes the 100-item paging for the library tabs.)
- Server side the list is refreshed by a short poll (30-60 s) or, better, by the
  *arr's own webhook (Connect > Webhook: on import, rename, delete, upgrade) which
  tells the server to refresh now and bump `rev`.
- Effort: small. Risk: low. Compatible with the current endpoint.

### 2. File marks (`nav_status`): a real delta, kept by the server

The marks change when a batch item, an attempt or a replacement changes, and all
of those are written through `store`. So the index can be maintained instead of
rebuilt:

- A table `file_status_index(path PRIMARY KEY, state, rev)` and a counter
  `rev`. On start (or when absent) it is built once with today's query.
- The `store` mutation points that can change a mark (batch item insert/update,
  action instance status/outputs for `transcode_media` and
  `promote_transcode_candidate`) re-evaluate the one affected item and write
  `state, rev=rev+1` only when the state changed. Cost per change is a few rows,
  not the whole history.
- `GET file-marks?since=<rev>` returns `{rev, changed: [{path, state|null}]}`.
  `null` is a tombstone (a file no longer marked). A token older than the
  retained window (say 10,000 changes) answers `410` and the app asks for the
  full set once. The folder and library endpoints keep returning `nav_status` as
  today for older clients.
- The app polls this instead of re-reading visible rows every ten seconds; an
  answer with no changes is a few bytes.
- Effort: medium. Risk: medium. Correctness depends on every mutation point
  being covered, so a periodic full rebuild (every few minutes, compared with the
  maintained table, logging any drift) is part of the design, and the unit test
  holds the maintained result equal to the rebuilt one.

### 3. Folder contents on the NAS: this is where the helper can help

The helper (`navigatorr-nas-helper`) is read-only, stateless, runs with no
capabilities and, by its own design, "measures, and the caller decides". The
coordinator reaches the library through an SMB mount, where each `stat` is a
round trip. What the helper can add without changing that stance:

- **`POST /v1/list {path}`**: readdir plus `lstat` of every entry, done on the
  NAS's local disk and returned in one answer (name, type, size, mtime). This
  turns "N round trips over SMB" into one request and is the single biggest win
  for big folders. It needs no state and no extra privilege. Effort: small.
- **A change hint per directory**: the helper already reports a directory's
  mtime stamp for folder sizes (`directories` map). A listing can carry the
  directory's stamp, and the app / server compare it: same stamp, same entries.
  (Directory mtime moves on add, remove and rename, but not when a file's content
  or size changes; a file's own size and mtime come from its `lstat`.)
- **A change feed (inotify)** held in the helper's memory: recursive watches over
  the library are possible (the kernel is 4.4, which has inotify; thousands of
  watches need `fs.inotify.max_user_watches` raised on the NAS, ~1 KB each), and
  changes made through SMB are ordinary VFS operations on the same filesystem so
  they are seen. The helper would keep a ring buffer of `(rev, path)` events and
  answer `GET /v1/changes?since=<rev>`; on restart the ring is empty, so its
  `instance_id` changes and every consumer does a full resync, which the helper's
  design already uses for jobs. Effort: medium. Risk: medium (watch limits,
  overflow events, missed events during restarts, which must be treated as
  "resync", never as "nothing changed").
- **btrfs generations** (`btrfs subvolume find-new`, the TREE_SEARCH ioctl)
  would give an exact "everything changed since transaction N" for the whole
  subvolume with no watches at all, but it needs `CAP_SYS_ADMIN` on this kernel,
  which breaks the helper's no-capabilities rule. Not recommended.

Should the helper hold the *index* itself (the sorted listing, sizes, revision
numbers persisted)? Not recommended: it would need a writable volume and a schema,
and it is the one component deliberately kept unable to write. The index and its
revisions belong in the coordinator (it already stores `folder_size_snapshots` in
SQLite, owns the auth and the *arr settings, and is the thing the app talks to).
The helper is the fast, local source of metadata and change hints; the
coordinator turns them into `rev` and tombstones.

## API shape common to all three

- A token is an opaque string `"<instance>.<rev>"`; the instance part changes when
  the index was rebuilt from scratch, so a client never mixes two histories.
- `since=<token>` returns changes and the new token; a token the server cannot
  continue from answers `410 Gone` with `{"resync": true}` and the client reloads
  the whole thing. A client that never sends `since` keeps today's behaviour.
- Deletions are tombstones inside the retained window.
- Everything is additive; nothing in the current endpoints changes meaning.

## Options, with what each buys

| Stage | What | Removes | Effort | Needs NAS change |
|---|---|---|---|---|
| 0 (this change) | short-lived *arr copy, non-blocking marks, page-only stat, `Server-Timing` | repeated *arr fetches, waiting on the ledger, SMB stat of unseen entries | done | no |
| 1 | `view=index` + ETag/304 for titles, local search and sort in the app | paging and search round trips; most of the data | S | no |
| 2 | maintained `file_status_index` + `file-marks?since=` | the full ledger scan and the polling of visible rows | M | no |
| 3a | helper `POST /v1/list` | SMB round trips per folder entry | S | helper release |
| 3b | helper `GET /v1/changes` (inotify) + coordinator folder index | re-listing unchanged folders | M | helper release, sysctl on the NAS |

Recommendation: do stage 1 and 3a next (small, independent, and aimed at what is
visible on a phone), measure with `Server-Timing`, and do stage 2 only if the
ledger scan is still showing up in `status;dur` once the history grows. Stage 3b
is the only one that adds a moving part on the NAS, so it should wait for a
measurement that says folder listing, after 3a, is still the slow step.
