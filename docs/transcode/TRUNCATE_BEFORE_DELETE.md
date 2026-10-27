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
  (`removeOwnCandidateFile`).

It does not apply to the recovery copy or the replaced original (planned
separately), to `fs_safe_delete` (the owner's own deletes keep the recycle bin
as their safety net), or to anything Sonarr, Radarr or the worker deletes.

## Rules

* Only a regular file reached through the same root and symlink checks the
  removal already uses. The file is opened without following symlinks, and its
  identity (same file, size, modification time, one link) is checked on the
  open descriptor before it is emptied.
* A file with more than one link, or whose link count is unknown, is unlinked
  without being emptied: the other name keeps the bytes, and emptying would
  destroy them.
* An empty file is just unlinked.
* Emptying never fails a cleanup. If the file cannot be opened for writing, or
  changed since it was listed, it is unlinked as before and the reason is kept.
* The outcome is recorded in the attempt's `candidate_cleanup` as
  `truncate: "emptied"` or `truncate: "skipped: <reason>"`; it is absent when
  the setting is off.

## Interrupted removal

Emptying happens just before the unlink. If the process stops between the two,
an empty file is left under the same name. It holds no space and the leftover
list does not show it, but nothing can use it: a promotion refuses it (size and
hash no longer match), and approving the discard of that attempt again removes
it, because that call looks at the disk.

The temporary candidate a promotion leaves beside the imported file is removed
by the finalize step. It saves `leftover_removal_started` before removing, and
when it resumes and finds an empty file there with that mark set, it finishes
the removal instead of failing on the digest.
