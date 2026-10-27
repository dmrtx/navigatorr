# Truncate before delete

Some shares keep what an SMB client deletes in a recycle bin (Synology's
`#recycle`, for example). Every discarded transcode candidate then stays on the
volume, whole, until the bin is emptied. `cleanup.truncate_before_delete`
empties a throwaway file just before it is unlinked, so the bin keeps an empty
file and the space comes back at once.

The setting is off by default. Emptying the recycle bin on the share, or turning
it off, achieves the same without code and is the simpler choice when the share
holds nothing else worth recovering.

## What it applies to

Files Navigatorr created and nothing else depends on:

* a transcode attempt's candidate and its worker partial, when they are
  discarded, retired (rejected, cancelled, replaced) or removed as a leftover
  (`removeOwnCandidateFile`);
* a promotion's recovery copy (or the original it moved aside) and its
  partial, when cleanup or a discard unlinks it (`recovery_truncate`);
* the original a promotion replaced, when the promotion itself unlinks it
  (`original_truncate`, below).

It does not apply to the temporary candidate that promotion leaves beside the
imported file. Sonarr may hardlink the import, so that name can be the library
file itself, and emptying it would destroy the library copy for no gain (the
recycle bin would only keep another name for it). Finalize unlinks it as before.

It does not apply to `fs_safe_delete` (the owner's own deletes keep the recycle bin
as their safety net), or to anything Sonarr, Radarr or the worker deletes.

## The replaced original

Emptying the original destroys the only copy if anything is wrong, so it
happens only when its bytes are provably safe elsewhere. The promotion's
recovery mode decides:

| Recovery mode | Original handling | Emptied here? |
| --- | --- | --- |
| `copy` | unlinked in place after the replacement is verified | yes, when every condition below holds |
| `move_aside` | renamed into the recovery directory; the recovery cleanup empties it (`recovery_truncate`) | not here |
| legacy (empty) or any unknown mode | unlinked as before | no |

A promotion that proves the original by size and modification time
(`original_stat_mod_time`) read the recovery copy only by sampled ranges, so
its original is not emptied either. Sonarr's and Radarr's own `DELETE` of the
old file is not covered; only the removal of an orphan the library left on
disk and the filesystem promotion's removal are.

Conditions, all checked with the file's lstat just before it is opened and
again when it is opened: the replacement was verified against its recorded
digest in this step; a recovery copy made by a full read of the original is
published and has the recorded size; neither is the original's inode; the
original is the recorded size and its only link. The intent
(`original_empty_started`) is persisted before the file is emptied; a resumed
step that finds a zero-byte original with that intent finishes the unlink
without hashing it, while a short original without it still fails
verification. A failed emptying falls back to the plain unlink.

## Rules

* Only a regular file reached through the same root and symlink checks the
  removal already uses. The file is opened without following symlinks, and its
  identity (same file, size, modification time, one link) is checked on the
  open descriptor before it is emptied. Over SMB these checks are the best
  available, not a guarantee: inode numbers and link counts come from the
  client and may be cached, so a file with a second name that the mount does
  not report would still be emptied.
* A file with more than one link, or whose link count is unknown, is unlinked
  without being emptied: the other name keeps the bytes, and emptying would
  destroy them.
* An empty file is just unlinked.
* Emptying never fails a cleanup. If the file cannot be opened for writing, or
  changed since it was listed, it is unlinked as before and the reason is kept.
* The outcome is recorded in the attempt's `candidate_cleanup` as
  `truncate: "emptied"` or `truncate: "skipped: <reason>"`; it is absent when
  the setting is off.
* If the file was emptied but the unlink then failed, the outcome is `kept`
  with `truncate: "emptied"` and `bytes_emptied`, and the note says "emptied
  but kept": the bytes are gone and only the empty name remains.

## Interrupted removal

Emptying happens just before the unlink. If the process stops between the two,
an empty file is left under the same name. It holds no space and the leftover
list does not show it, but nothing can use it: a promotion refuses it (size and
hash no longer match), and approving the discard of that attempt again removes
it, because that call looks at the disk.
