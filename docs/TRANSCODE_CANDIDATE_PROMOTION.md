# Promoting a transcode candidate

`transcode_media` remains candidate-only. To replace a Sonarr library file, run
the separate `promote_transcode_candidate` action with the completed
`transcode_action_id`, the Sonarr `series_id`, and optional `service` (default
`sonarr`). `allow_destructive` and filesystem write roots must already permit the
operation.

```json
{
  "action": "promote_transcode_candidate",
  "inputs": "{\"transcode_action_id\":\"act-transcode-media-example\",\"series_id\":42,\"service\":\"sonarr\"}"
}
```

The action verifies the original SHA-256, revalidates the complete candidate's
streams with the existing transcode validator, hashes the candidate, and resolves
every Sonarr episode which shares the original episodeFile. It then returns
`waiting_decision` with those exact paths and episode IDs. Resume that action with
`decision: "approve"` to perform the replacement. `reject` preserves both files.
Starting a transcode, enabling destructive operations, or polling a waiting
action does not grant this approval. Promotion does not accept replacement
inputs on resume, so the approval remains tied to the persisted plan.

After approval, the workflow:

1. Reserves the original Sonarr episodeFile for this promotion and creates a
   separate recovery copy with a verified original SHA-256.
2. Rechecks the original, candidate, episode associations and series path, then
   submits one Sonarr `ManualImport` with every affected episode ID.
3. Confirms all affected episodes use one new file ID, checks Sonarr's HEVC
   metadata and probes/hashes the actual file behind that ID. Its SHA-256 must
   equal the validated candidate.
4. Reloads the old file ID and episode associations immediately before any old
   file deletion. An ID whose path changed, an old ID still in use, or a path
   overlapping the new file stops the workflow with its recovery copy retained.
5. Runs `RenameFiles` when needed to move the active file outside
   `.navigatorr-candidates`, rescans the series, and verifies the original file
   ID is absent and every approved episode has the verified new file.
6. Removes any verified temporary candidate copy and recovery copy, then records
   the actual bytes saved. Successful completion never leaves the active file
   inside `.navigatorr-candidates`.

The recovery file is an independent copy, not a link to the original. While
promotion is in progress it temporarily requires approximately the original's
size in additional storage. It is retained on failures or uncertain outcomes,
and `recovery_path` identifies it. Savings are final only after successful
cleanup. Recovery paths are confined to write roots and symlink traversal is
rejected.

Recovery publication is itself restart-safe. Navigatorr persists the execution
owner before writing `original.bak.partial` and persists a separate
`recovery_copy_complete` checkpoint only after the copy has finished, synced,
and closed. A partial still owned by the current execution is never hashed,
unlinked, or restarted. If a later execution inherits the action after the old
lease is gone, it first detaches the abandoned partial pathname before rebuilding
it, so a lingering writer cannot modify the new partial being verified. Only a
complete partial with the expected size and SHA-256 is renamed to
`original.bak`; `recovery_verified=true` is persisted before ManualImport is
allowed.

## Why import alone is insufficient

Sonarr's
[ManualImportService](https://github.com/Sonarr/Sonarr/blob/develop/src/NzbDrone.Core/MediaFiles/EpisodeImport/Manual/ManualImportService.cs)
determines an existing library file from its relationship to the series path.
[ImportApprovedEpisodes](https://github.com/Sonarr/Sonarr/blob/develop/src/NzbDrone.Core/MediaFiles/EpisodeImport/ImportApprovedEpisodes.cs)
registers an existing library file in place; this explains why a candidate can
remain in the temporary directory until `RenameFiles` runs. For a new download,
[UpgradeMediaFileService](https://github.com/Sonarr/Sonarr/blob/develop/src/NzbDrone.Core/MediaFiles/UpgradeMediaFileService.cs)
removes the previous files before copying/moving the replacement, including
`importMode: copy`.

Promotion therefore requires the original and candidate to be physically
accessible inside the selected Sonarr series path, sends no download ID, and
preserves the recovery copy before calling Sonarr. It does not guess remote path
aliases. The copy also protects recovery if upstream import behavior differs
between versions.

## Timeouts, restarts and retry

Every external mutation has a persisted intent before its HTTP request. POSTs
are sent once, with automatic retries and redirects disabled. After a timeout,
the workflow checks episode adoption, physical content and the matching Sonarr
command; it does not send another import. `waiting_external` states reconcile
automatically after approval. `waiting_decision` is never automatically approved.

If Sonarr exposes neither a matching command nor a verifiable result after two
minutes, the action pauses for investigation with a `reconcile` choice. That
choice checks the recorded operation again and cannot authorize a blind replay.
Sonarr's command history is not a durable idempotency API, so absence from history
alone is not evidence that an earlier import was rejected.

`action_retry` can repeat a **confirmed failed** rename or rescan after checking
its observed effect; it preserves the successful import and deletion checkpoints.
A failed or uncertain import is reconciled without replaying it. Repeated calls
for the same source action reuse the same promotion, including terminal results.
Different candidates for the same old Sonarr file cannot promote concurrently;
the durable original-file reservation remains with its original promotion if an
outcome needs recovery.

These workflows are covered with a fake Sonarr server and temporary physical
files, including approval, multiple episodes per file, source/candidate changes,
lost import responses, ambiguous command histories, old-ID drift, failed rename
resumption, internal Sonarr deletion, cleanup checkpoints and redirect replay.
The tests do not modify a live Sonarr library.
