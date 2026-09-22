# Transcode and promotion lifecycle

The worker prepares the source, encodes and validates on its local disk. The NAS
receives the completed candidate through the existing no-clobber publisher.
Sonarr promotion is a separate approved operation; publishing a candidate does
not authorize replacement of the original.

## Integrity and restart boundaries

1. For external media, scratch must be outside external roots, including symlink
   aliases. Existing jobs use their persisted paths when resumed.
2. Verify staged source bytes against the coordinator's original SHA before
   probing or encoding, including cache-disabled and resumed staging.
3. Hash the local candidate before and after media validation. Persist its
   accepted size/SHA with `EncodeComplete` before publishing.
4. Recheck that local identity before and after publication, including retries.
   A changed modern checkpoint is retained for investigation, never re-encoded
   or silently accepted. Legacy records predating candidate attestations retain
   compatibility; promotion still performs the full media validation.
5. Promotion compares NAS bytes with the worker digest when supplied. The initial
   promotion validates media policy; the first adopted file receives a physical
   HEVC check. Later adoption checks use the persisted media proof and exact SHA
   equality instead of repeating ffprobe against the same content.
6. Preserve and verify an independent recovery copy before destructive Sonarr
   work. Persist command intent before sending. An uncertain import/rename is
   reconciled, never blindly resent.
7. A stale renamed path is accepted only for the exact approved temporary path
   and persisted new episodeFile ID. Without a durable final path, wait for Sonarr
   to expose one. Read-only rename polling does not hash multi-GB recovery files.
8. Before removing recovery, verify the final library file, identity, approved
   episode associations and absence of the old original. Cleanup checkpoints
   allow retry after recovery removal without replaying Sonarr commands.

Hash stability uses the opened descriptor's size and mtime; pathname stats prove
file identity. Delayed NAS pathname metadata therefore does not force another
full read of an otherwise stable, closed recovery copy. Real descriptor changes,
path replacement and digest mismatches remain errors.

## Artifact ownership

| State | Node | NAS |
| --- | --- | --- |
| Encoding/validating | Job-local source/candidate; bounded shared source cache | Original remains intact |
| Publication pending/failure | Validated candidate retained for resume | Only exact job-owned publication partial/destination handled by publisher |
| Published | Job scratch files and empty job directory removed; shared cache retained | Candidate waits for explicit promotion |
| Promotion rejected | Completed worker state | Original and candidate retained intentionally |
| Promotion failed/uncertain | Durable action/worker state | Recovery/candidate retained as needed for investigation or resume |
| Promotion completed | Durable history and bounded shared cache | Final episode; verified recovery removed; empty private recovery and candidate directories removed |

Cleanup never recursively removes the shared candidate directory, sibling jobs,
unknown files or shared source cache. Failed/rejected work is not garbage merely
because it is old; deleting its recovery requires resolving the action first.
Existing historical orphans are not swept by these changes.

## Expected I/O

Heavy encode, media probes and additional source/candidate checks run on local
worker bytes. Promotion still needs NAS integrity reads at its destructive
boundaries and an independent original recovery copy. There is no claim of zero
NAS reads or guaranteed speedup: measure production transfer time separately.
