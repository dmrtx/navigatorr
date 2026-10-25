# NAS helper: promotion without moving data through the network

Status: design v2, reviewed twice. Delivered in stages (see Staging); stage 1 is the candidate checks, the recovery stage needs the redesign listed under Staging.

## Why

Replacing a file ("promotion") runs on the coordinator, which reaches the
library through an SMB (CIFS) mount. Measured on production (CIFS 3.0 mount of
a btrfs volume on the NAS):

| Operation | Through the mount | Inside the NAS |
|---|---|---|
| Clone / reflink of a file | refused (`operation not supported`) | 0.01 s |
| Rename | instant | instant |
| `copy_file_range` | NAS copies internally, no network, but real disk work: ~55 s for 2.6 GB with four at once | not needed |
| Full hash of a file | whole file over the network (1 GiB in 9 s, 1071 MiB received) | local disk read |

A promotion of a hash-once job (`action/promote_cheap.go`) never reads the
original, but it still

1. copies the original into a recovery file (`copy_file_range`: ~55 s of NAS
   disk work for 2.6 GB, heavy when four run together), because move-aside is
   refused: the coordinator's mount cannot prove, without reading the file,
   that a renamed file is the same file (`promotionProbeMount`); and
2. reads the converted file (the candidate) in full over the mount more than
   once: plan hash, preserve after the approval wait, and the final checks.

Rule from the owner: the original is read once (to encode) and the converted
file is written once (published by the worker); nothing else may copy or
re-read them over the network. The NAS has what is needed: a btrfs volume
with real inode numbers, and Container Manager.

## What

A small static Go binary, `navigatorr-nas-helper`, in a container on the NAS.
**Read-only**: the library is bind-mounted `ro`, so a bug or a compromise of
the helper cannot write, replace or delete library files. It reports what is
true on the NAS's own filesystem and does local reads there.

| Operation | Purpose |
|---|---|
| `GET /v1/health` | version, instance id (random per start), roots, resolver (`openat2` or `walk`) |
| `POST /v1/identity` `{path}` | `{fsid, fs_type, subvol, generation, ino, size, mtime_ns, ctime_ns, complete}` taken from an open descriptor (stat the descriptor, `FS_IOC_GETVERSION` for the generation). `fsid` is `statfs` `f_fsid`, which on btrfs is the UUID folded with the subvolume id, so it is compared as an opaque value; `complete` is false when the generation or the subvolume could not be read |
| `POST /v1/hash` `{path}` → job; `GET /v1/jobs/{id}` | SHA-256 and byte count of a file read from local disk, with progress; identity is taken from the descriptor before and after the read and the result is refused if it moved. The helper is never given the expected value |
| `POST /v1/sample` `{path}` | the same sampled-range digest as `promotionSampleDigest` (1 MiB head, tail and 6 interior ranges), for a cross-check against the same digest taken over SMB |

The coordinator keeps every mutation: rename, delete, import, the library
service calls. Those are metadata operations over SMB, already audited.

### Where it plugs in

* **Recovery = move-aside, not a copy.** `promotionChooseRecovery` chooses
  move-aside again for a hash-once promotion when the helper is healthy,
  using the helper's identity instead of the mount's. The original is
  identified by the helper before the rename and the renamed file (the
  recovery file) after it: same `fsid`, `subvol`, `ino`, `generation`,
  `size`, `mtime_ns`. `ctime` is expected to change on rename and is recorded,
  not compared across it. Nothing is copied or read. The pinned SHA-256 from
  staging and the size + mtime check at plan stay the proof of what the
  original is. If the helper is unavailable, or any identity field disagrees,
  the promotion uses the existing copy path before anything has moved, and the
  reason is recorded.
* **Candidate, at plan.** The helper hashes the published candidate locally.
  The worker's attested `candidate_sha256` / `candidate_size_bytes` stay the
  baseline: using the helper requires them (no attestation, no helper hash),
  the comparison happens in the coordinator, and the helper is never told the
  expected value. A cheap SMB cross-check (`promotionSampleDigest`, about
  8 MiB) must equal the helper's `sample` digest: it catches a wrong path or
  an echoing helper at almost no cost.
* **Candidate, between waits.** Checks that precede a destructive step but do
  not delete anything (preserve, import, rename) use the helper's identity
  recorded at plan, stored as its own type `helperFingerprint` with its source
  (instance id, fsid, subvolume, generation); it is never compared with a
  `promotionFingerprint` from the mount, and a fingerprint from another
  helper instance proves nothing.
* **Finalize keeps its full read of the final file before any recovery data
  is removed** (`cleanup_progress.go:36-41`, `promote_move_aside.go:379-384`):
  the read is done by the helper on local disk, hash compared with the
  attested one. Zero network, a few seconds of local disk. The leftover
  candidate read before it is removed (`promotionRemoveLeftoverCandidate`)
  goes the same way.
* **Original**: unchanged (size + mtime against what staging recorded). It is
  never read. The helper only adds the identity used by move-aside.

### What is not done

* No data is copied, cloned or written by the helper.
* Rename, delete, import and the library-service calls stay in the
  coordinator.
* Sonarr/Radarr copying the candidate on import (`importMode: "copy"`) is
  outside this change; the filesystem service has no such copy. Noted as a
  separate item.
* The worker's own staging and publication are unchanged.

## Failure model

* Every helper error, timeout, disagreement (identity field, digest, instance
  id, path) ends in the current path **before** the first mutation, never in
  "skip the check". After the first mutation (the original has been moved), a
  helper failure leaves the promotion waiting or failed with its recovery
  file in place; it does not silently weaken a later check. A restore needs
  only the SMB rename.
* The degradation is visible: each promotion records `verification_path`
  per check (`helper`, `smb_full`, `smb_fallback:<reason>`) and the reason
  move-aside was or was not used; the UI lists the count of fallbacks.
* Resume: `RecoveryCopy`/recovery mode, the helper fingerprint with its source,
  and `RecoverySample` (kept for copy mode) are saved before the step that
  relies on them. A helper identity from a different instance, filesystem or
  subvolume is discarded and the file is read in full. Nothing from the helper
  enters `planDigest`: the digest says what is touched, not how it is checked.
* Path mapping is explicit in configuration, tested by checking that one file
  gives the same size and sampled digest over SMB and from the helper.

## Security

* Bearer token (Docker secret, constant-time compare), bound to the NAS LAN
  address, token kept in a secret file on the coordinator. Never in the repo.
* Roots allowlist; each request resolves the path through the descriptor with
  `openat2(RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS)`, so there is no
  validate-then-open race; non-regular files are refused. The NAS kernel is
  4.4.302, which has no `openat2` (5.6+), so the helper probes it at start and
  otherwise walks the path one component at a time, each opened with
  `O_NOFOLLOW` relative to the previous directory descriptor and never through
  `..`. Both refuse symbolic links and cannot leave the root; neither detects a
  mount nested inside a root, so the bind mount is `ro,nodev,nosuid` and single.
  `/v1/health` reports which one is in use.
* Container: unprivileged uid with read access, read-only root filesystem, no
  capabilities, one read-only bind mount of the single shared folder (one
  mount point, so identity is one filesystem), no Docker socket.
* Load: at most two concurrent hashes and 32 jobs queued or running, low I/O
  priority (a thread is taken only once a job has its slot), cancelled requests
  stop reading; a sample does not take a hash slot. The helper must be a better citizen of the NAS than SMB reads.

## Rollout

1. `cmd/navigatorr-nas-helper` + tests (temp dirs; `FS_IOC_GETVERSION` and
   subvolume id behind an interface; an integration test on the real NAS).
2. Image published by the existing `docker-publish` workflow, second target.
3. Coordinator client behind config (`nas_helper: {url, token_file,
   path_mappings}`); off when unset. Identity provider interface used by
   move-aside, with the local stat and the helper as implementations.
4. Ansible role to run the container on the NAS (separate, private repo).
5. Measure on the real NAS first: whether rename keeps `ino` and
   `generation` and which fields change `ctime` (rename over SMB, Sonarr
   renames, ACL inheritance, reads); that a file created over SMB is visible
   to the helper at once. Then a real "Replace file" with network and NAS disk
   I/O compared before and after.

## Plan B, only if measurement says so

If rename over SMB turns out not to keep inode and generation, or the
identity proves too unstable, the helper can be given a single write
operation, a reflink into the exact recovery path the coordinator derived,
with the destination recomputed, never replacing, and an unprivileged uid with
write access only under the candidates directory. That is the v1 design and
carries the ACL and shared-extent issues listed in the first review; it is
not part of this change.

## Questions for review

1. With move-aside on helper identity, which fields must match across the
   rename for "this recovery file is the original", and is reading nothing
   defensible given the staging-time SHA-256 and the size + mtime check?
2. Is the full local read by the helper at finalize an acceptable substitute
   for the coordinator's own read under the rule in `cleanup_progress.go`?
3. The leftover/partial recovery files from earlier attempts and the existing
   `Restore` flow (`transcode_backups`): do they still make sense when the
   recovery file is the original itself?
4. Resume across a helper restart in the middle of a move-aside promotion.
5. Anything in the current move-aside code that assumes the mount's own
   fingerprints and cannot take an identity from another source.

## Measurements (production, 2026-10-25)

Taken with a 64 MiB scratch file under a scratch directory (removed), comparing
what the coordinator's CIFS mount reports with `stat` run inside the NAS:

* The CIFS mount's `ctime` is not the file's change time: after a rename the
  NAS's `ctime` moved and the mount's did not (it equals `mtime`). That is why
  the mount probe refuses move-aside, and why the real `ctime` from inside the
  NAS is the missing evidence.
* Inode numbers differ between the two views (a Samba mapping), so identities
  from the mount and from the helper are separate types and are never compared.
* `mtime` differs below 100 ns (SMB truncates to 100 ns units).
* The NAS kernel is 4.4.302+: no `openat2`, so the component walk is the resolver there.
* A rename keeps the NAS inode; no-replace rename over SMB works (`EEXIST` on
  an existing destination); reading over SMB does not change `ctime`.

## Deployment and measurements on the NAS (2026-10-25)

Deployed by Ansible (`playbooks/navigatorr/nas-helper.yml` in the private
repository): uid of the SMB account, read-only root, `cap_drop ALL`, library
mounted read-only, bound to the LAN address. Health reports `resolver: walk`
(kernel 4.4); a request without the token gets 401; the helper's hash of a real
file equals the hash read over SMB; the idle I/O class (3) is applied inside the
container (Docker's default seccomp allows `ioprio_set`); cancelling a job stops
the read.

Throughput: the Go standard library hashes at ~98 MB/s on the NAS's Celeron
J4125, which has SHA extensions but no AVX2 (Go needs both). `sha256-simd`
reaches 555 MB/s on the same machine, so the helper uses it; end to end, a
470 MB file hashes at 100-140 MB/s with the disk read at idle priority and no
bytes on the network.

## Staging

1. Shared sampled-range digest package (no behaviour change).
2. The read-only helper binary (`identity`, `hash`, `sample`).
3. Coordinator client and configuration (off when unset). Done: `nas_helper`
   config block, `internal/nashelper` client (restart detection by instance id,
   no redirects, job cancelled when the caller gives up), a `nas_helper`
   section in `diagnostics`; nothing else reads it yet.
4. Candidate checks through the helper: plan hash against the worker's
   attestation (required), SMB sample cross-check, identity between waits,
   full local read by the helper before recovery data is removed. Any helper
   failure falls back to the full SMB read, recorded as a degradation.
5. Recovery stage (separate design, from the second review): a dispatch of
   "prove the original" by mode, a matrix of move-aside state against helper
   state (same / restarted / down), the helper's full identity including
   `ctime` before the rename, a copy fallback that does no SMB hash, a
   restore/discard/clean/duplicate rework (discard of a moved recovery must
   require the helper's hash of the replacement to equal the recorded one),
   and the tool descriptions. Not started.

Client contract for stage 4 (from the stage 3 review): `IsAnswer(err)` is true only
for `not_found`, `not_regular_file` and `changed_while_reading`; every other
failure (unreachable, the client's own timeout, a path the helper does not serve,
`permission_denied`, a restart, an unknown code) satisfies `ShouldFallBack(err)`
and the file is read through the mount, recorded as a degradation. The caller's
own cancellation is neither. A `not_found` is only as good as the path mapping, so
absence is confirmed with a `stat` on the mount before any destructive decision.
`Identity`, `Sample` and `Hash` return the helper's instance id with the result so
a stored identity is never compared with one from another instance. `Hash` has no
timeout of its own (the caller passes a deadline), tolerates two failed polls in a
row, and cancels its job on every exit before a result. `Sample` always needs a
deadline from the caller. A caller that wraps a helper call in a shorter deadline of
its own gets its own deadline back, not a fallback. The helper stops a hash job
nobody has asked about for two minutes (a caller that died, or never received the
id), so an abandoned job cannot keep reading or hold a slot.

### Stage 4 design: full hashes by the helper

Where the cost is: `promotionHash` (action/promote_files.go) is the one place that
reads a whole media file through the mount; the plan, preserve, promote and final
verification steps all go through it (`verifyPromotionHash*`), and the
per-execution cache (`promotionVerifiedFiles`) is dropped at every wait, so a
promotion that waits for approval reads the converted file again. Each read is the
file's size over SMB.

Change: `promotionHash` asks the helper to do the full read when a helper is
configured and the path is mapped, and keeps every other check as it is.

1. The mount-side metadata checks stay exactly as they are (Lstat, open, SameFile,
   size, mtime before and after), so the verified-file cache, the stable-retry
   loop and the change detection behave as before. Only the byte loop is replaced.
2. The helper hashes the file (`Client.Hash`, idle I/O priority on the NAS, no data
   on the network). Its answer must agree with the mount: the byte count equals the
   size the mount reports, and the helper's identity is complete and unchanged
   between the hash and a sample taken right after it.
3. Cross-check against the mount: the sampled-range digest of the file read over
   the mount (about 8 MiB, the function recovery already uses) must equal the
   helper's sample digest. A mismatch means the helper and the mount are not
   looking at the same bytes (a wrong mapping, a stale mount cache); it is an
   integrity failure, never a fallback.
4. Any helper failure for which `ShouldFallBack` is true (unreachable, timeout,
   restart, a path it does not serve, refusal) falls back to the full read through
   the mount, and the integrity record says so. An answer (`not_found`,
   `not_regular_file`, `changed_while_reading`) maps to the error the mount read
   would have produced (not-exist, not a regular file, file changed, which the
   stable loop retries).
5. The plan step still compares the digest with the worker's attested
   `candidate_sha256` and size, so the helper's hash is checked against a value it
   never saw; and the candidate that was probed is the one hashed, as before.
6. The integrity record (`integrity_checks`) gets `via`: `nas_helper` or
   `smb_fallback` (with the helper error class), so a promotion that read over the
   network says why.
7. The same substitution serves every path that goes through `promotionHash`: the
   candidate, the recovery copy being verified, the final file, and a
   not-hash-once original. It does not change which steps run or in what order, so
   a recovery stays what it is today (stage 5).

Out of scope here: identity comparison across waits (needs the stage 5 recovery
design), the recovery copy's NAS disk work, `validateCandidateDetailed`'s probe.

Required from the second review for the helper's API: no expected value is
ever sent to it; a job is created for the step that uses it; the sampling
function is shared so the digests are identical.

#### As built (after the Opus review)

Only the byte loop of `promotionHash` changed (`action/promote_helper.go`). The
mount's `Lstat`/open/`SameFile`/size/mtime checks and the verified-file cache are
untouched.

- Order: mount open + fstat, helper `Hash` (identity A), the mount's sampled ranges
  read from the same open descriptor, helper `Sample` (instance and identity must
  equal A, digest must equal the mount's), final mount fstat.
- Identity or size drift is `promotionFileChangedError` (the stable loop re-reads).
  A sample digest mismatch is `promotionHelperDisagreeError`: almost always a wrong
  `path_mappings` entry, never retried and never answered from the mount.
- Anything the helper cannot answer reads through the mount, and the execution
  state's `integrity_reads` records `via` (`nas_helper` / `smb_fallback` / `smb`)
  with the class: `helper_down`, `helper_unavailable`, `helper_disagrees`
  (helper says not_found for a file the mount just opened), `helper_busy`,
  `helper_restarted`, `stalled`. The record is kept by flows that force full reads
  and does not depend on the cache.
- A helper restart or a job the helper cancelled is asked once more; `busy` is
  retried briefly; an unreachable helper trips a 2 minute breaker; a hash with no
  progress for 5 minutes is cancelled and read through the mount.
- The helper path takes no engine hash slot but yields the reconcile slot before
  it waits. `promotionReadHook` counts mount bytes only.
- `Identity.Complete` is recorded now and becomes a requirement in stage 5.

Where the earlier text of this design differs from the above, the above wins:
`Identity.Complete` is recorded, not required (stage 5 requires it); a helper
`not_found` is a fallback class (`helper_disagrees`), never a not-exist answer; and
the audit trail is `integrity_reads` (last read per path), next to the cache's
`integrity_checks`. A hash that stalls after it started reading opens the breaker;
one still queued does not. A file whose size differs from the mount's, while the
helper's identity held steady, is a disagreement, not a retryable change.

### Stage 5 design: recovery by moving the original aside, proved by the helper

Where the cost is: a promotion keeps the original recoverable by copying it into
its private recovery directory (`promotionMakeRecoveryCopy`: a kernel copy plus a
full read-back). On the production NAS that is about 18 s of NAS disk work for
2.2 GB and a second full file on the volume until the promotion ends. The
alternative already exists: `move_aside` renames the original into the recovery
path right before the library file is removed, so no byte is copied. It is never
chosen on the SMB mount because `promotionProbeMount` fails
(`rename_changed_identity fs=smb2 changed: mtime,ctime`): the mount does not report
a stable identity across a rename, so every mount-side proof (`OriginalFingerprint`,
`BackupFingerprint`, `sameFileAfterRead`) is unreliable there. The helper sees the
NAS filesystem itself (btrfs fsid, subvolume, generation, inode, size, mtime, ctime),
which a rename keeps except for ctime.

Change: when the original's last full hash was made by the helper with a complete
identity, `move_aside` is chosen without the mount probe and proved by helper
identity. Nothing else about the workflow changes: same steps, same state machine,
same fallback to a copy.

State (all new fields `omitempty`; a promotion recorded before this has none and
keeps its mount proof):

- `RecoveryProof`: `"helper"` or empty (the mount proof that exists today).
- `OriginalHelper`, `BackupHelper`: `{identity, sample, process}`; `identity` is a
  `nashelper.Identity` (every field, ctime included) and `sample` the sampled-range
  digest the helper returned with it. `process` is `promotionProcess`, like the mount
  fingerprints: a proof from an earlier process proves nothing and the file is read
  in full again.

1. `promotionHelperHash` already holds identity A and the sample digest of the file
   it hashed. It hands them to `promotionRememberHash`, which stores them in the
   per-execution entry (`promotionVerifiedFile.helper`), next to the mount
   fingerprint, which may now be absent. Only an identity with `Complete` is kept.
2. `promotionChooseRecovery`: after the existing exclusions (forced copy, original not
   read again, leftovers), if the original has a helper proof and the helper maps the
   recovery directory, choose `move_aside` with `RecoveryProof = "helper"` and skip
   the mount probe. With no helper proof the existing logic (fingerprint, probe) runs
   unchanged. `stepPromotePreserve` stores `OriginalHelper` from the execution's entry
   (as it does `OriginalFingerprint`), and forces a copy when a helper-proof
   promotion has none and no move aside began.
3. Before the rename, `promotionMoveAside` for a helper proof:
   a. If `OriginalHelper` is from this process, ask the helper to `Sample` the
      original: identity equal to the recorded one in every field including ctime,
      and the same digest. ctime moves on every write, chmod, link or rename, so equal
      ctime since the hash means nothing touched the file. Otherwise read it in full
      (`verifyPromotionHashStable`, through the helper) and take the proof that read
      produces. A path the helper cannot hash (it fell back to the mount) leaves no
      helper proof: return `errPromotionMoveAsideUnsupported` before any intent is
      saved, and the existing fallback switches the promotion to a recovery copy.
   b. Save the intent (`MoveAsideStartedAt`, `OriginalHelper`), then the existing
      no-replace rename.
4. After the rename the helper `Sample`s `BackupPath`. It is the same file when
   fsid, subvolume, generation, inode, size and mtime equal the recorded ones (ctime
   may differ) and the sample digest is equal. Then set `MovedAside`, `BackupVerified`,
   `RecoveryVerifiedBeforeReplacement` and `BackupHelper` (the new identity). No mount
   fingerprint is consulted. If the identity differs (the server implemented the
   rename as a copy), or the helper cannot answer, the file at `BackupPath` is read in
   full and accepted only if it equals `OriginalSHA`; otherwise the promotion stops
   with the recovery retained, as today.
5. Later verification (`promotionVerifyRecovered`, restore, retry): for a helper proof
   in the same process, the helper `Sample` of `BackupPath` must equal `BackupHelper`
   entirely (ctime too: the recovery directory is private and nothing should touch it);
   any difference or an earlier process reads the file in full. Before the move the
   check is the full read of the original after any wait, as now. The mount
   `Lstat`s that tell which of the two paths exists stay.
6. Crash states are the ones the mount proof already has; only the proof differs:
   - intent saved, rename not done: original present, nothing at `BackupPath`; a restart
     re-verifies (full read) and renames.
   - rename done, checkpoint not saved: original absent, file at `BackupPath`,
     `MoveAsideStartedAt` set; the full read of `BackupPath` equal to `OriginalSHA` is the
     proof (no identity comparison is needed after a read in full).
   - moved: as 5.
7. Move back (`promotionMoveBack`, `RestoreTranscodeBackup`, a failed same-name publish)
   keeps the no-replace rename and the current guards. `promotionFinishMoveBack` accepts
   the file at the library path when the helper `Sample` shows `BackupHelper`'s inode,
   size, mtime and digest, else reads it in full.
8. Not changed: discard, clean-up and `promotionRecoveryIsOnlyOriginal` (they look at
   paths and sizes, not identities); the duplicate-copy discard (copy mode only); the
   copy mode itself, which stays the fallback for a refused rename, a missing helper
   proof, a leftover recovery and a forced copy.
9. Audit: the step output gains `recovery_proof` (`helper`/`mount`); the integrity
   record for `BackupPath` says `helper_identity` when the helper proved it.

Residual risks, stated:
- Between the helper's pre-rename `Sample` and the rename, someone could rewrite the
  file in place and restore its mtime. The pre-check and the rename are milliseconds
  apart, the library does not write files in place, and the post-rename sample digest
  covers the sampled ranges and the size; a full read is not repeated.
- The rename is atomic on the NAS, but the library path is empty from the rename until
  the candidate takes the name. That is how `move_aside` already works.
- A btrfs ctime has jiffy granularity; the proof relies on the hash having been taken
  much later than the last modification, so a later write gets a later ctime.

Questions for the review:
- Is `Sample` (identity plus about 8 MiB) the right pre- and post-rename proof, or
  should the post-rename check be a full helper hash of the recovery file (one NAS-local
  read instead of an 18 s copy plus its read-back)?
- Should a proof from an earlier process also count when the helper identity is equal in
  every field, ctime included? It would remove the full read after each wait; the mount
  proofs were never allowed that, because the mount's identity is synthetic. This design
  does not allow it.
- Anything that makes the helper proof weaker than the mount probe it replaces.

#### Stage 5 revision after the Opus review

Verdict was "approve with changes"; where this differs from the text above, this wins.

1. One proof, two variants. With `RecoveryProof = "helper"` no mount fingerprint is
   required or consulted anywhere: `BackupFingerprint` stays nil; `OriginalFingerprint`
   is not demanded by `stepPromotePreserve`, `promotionMoveAside` or
   `promotionVerifyRecovered`; `promotionFinishMoveBack` compares helper identities and
   then stores the fresh identity in `OriginalHelper` (the second rename moved ctime)
   and clears `BackupHelper`. A full read equal to `OriginalSHA` is a proof by itself
   wherever an identity comparison is not possible, including the restart that finds the
   rename done (the old `sameFileAfterRead` check after a full read is not applied to
   the helper proof). `promotionRememberHash` keeps the helper proof even when the
   mount fingerprint is unusable; `promotionKnownHelper(ctx, path, sha)` is separate
   from `promotionKnownFingerprint` and never returns a zero value.
2. No copy fallback for a missing proof. Whatever cannot be proved by identity is
   proved by a full read: before the rename the original is read in full (a
   helper hash, or the mount when the helper is down), and after it the file at
   `BackupPath` is read in full and must equal `OriginalSHA`. The copy fallback stays
   for a refused rename only, as today; a mode never changes once the intent is saved.
3. A helper answer never decides that a file is gone. `not_found` or any error of the
   pre-rename `Sample` just means "read it in full"; `errPromotionOriginalGone` still
   comes only from the mount's own not-exist. The helper and the mount must agree on
   which of the two paths exists; the stage 4 hash already falls back to the mount on
   `helper_disagrees`.
4. The mount probe is replaced by a probe the helper observes, in the recovery
   directory, cached per process and directory (definitive verdicts only): create a
   file through the mount and read its helper identity (must be `Complete`); rename it
   no-replace and require the same fsid, subvolume, generation, inode, size and mtime
   with a changed ctime; a no-replace rename onto an existing file must be refused;
   rewrite it in place with mtime restored and require the helper's ctime to move. The
   original's fsid and subvolume must equal the probe file's (else the rename would be
   `EXDEV`), checked per promotion. A probe the helper cannot run falls through to the
   existing mount probe, then to a copy.
5. `duplicateBackupGuard` refuses any promotion that moves aside (`movesAside()`,
   `MovedAside`, `MoveAsideStartedAt`, `MoveBackStartedAt`).
6. Audit: `integrity_reads` gets `via: nas_helper_identity` with the changed fields when
   the identity moved (`identity_moved:ctime`), and `recovery_proof` is also in the
   step output.
7. Questions 1-3 answered: `Sample` is enough for the post-rename check (identity is the
   proof; a full helper hash adds nothing that protects data); a proof from an earlier
   process does not count in this stage; the helper proof is weaker than the mount
   probe only by losing its empirical check, which item 4 restores.

#### As built (stage 5)

`action/promote_helper_proof.go` holds the proof (`helperProof`, `promotionKnownHelper`,
`promotionVerifyByHelper`, the helper-observed probe); `promotionConfirmMovedByHelper`
is in `promote_move_aside.go`. State: `recovery_proof`, `original_helper`,
`recovery_helper`. A promotion with no `recovery_proof` keeps the mount proof unchanged.

- `promotionChooseRecovery` takes the helper proof when the original's last full hash
  came through the helper with a complete identity and `promotionProbeHelper` passes for
  the recovery directory. A definitive probe failure chooses a copy (reason
  `mount_probe_failed`, with the probe's step as detail); a probe that could not run falls
  through to the mount probe.
- The probe verdict is cached per process and recovery directory; the check that the
  original lives on the same filesystem and subvolume is made per promotion.
- Before the rename: the helper's `Sample` of the original must equal the proof of this
  process in every field (ctime included) and in digest, else the original is read in full.
  After it: the same inode (ctime may move) and digest, else the moved file is read in full
  and must equal `OriginalSHA`. A restart that finds the rename done reads the file in
  full once and accepts that read.
- Later checks, restore and move back use `promotionVerifyByHelper`; a helper that is down
  or an earlier process means a full read (through the mount if the helper is gone).
- `duplicateBackupGuard` refuses any promotion that moves aside.
- A proof from the same process also counts across waits, as the mount fingerprints do.

### Stage 5b design: move aside for promotions that never read the original

Found on the first real Replace file after stage 5 was deployed (Monogatari S02E02,
1.78 GB, filesystem service, 2026-10-25): `recovery_mode: copy`,
`recovery_mode_reason: original_not_read_again`, a 6 s `copy_file_range` copy on the NAS.
Stage 5 never engaged. Every promotion admitted by the hash-once flow
(`originalByMetadata`, `OriginalStatMod != ""`) returns early in `promotionChooseRecovery`
because no full read of the original happens in the promotion, so no helper proof
(`promotionKnownHelper`) and no mount fingerprint exist. That is the common case, so the
design goal (no recovery copy, no read of the original) is not met until it is covered.

**Trust model today.** The transcode pinned `OriginalSHA` once, while the worker copied the
original, and accepted it by size + mtime. Promotion keeps to that: the original is the
file whose size and mtime equal the transcode's (`promotionOriginalStatMatches`).

**Proposal.** For a metadata-mode promotion the helper's `Sample` of the original, taken in
`stepPromotePreserve`, becomes the proof:

1. Preconditions, all required, else the current copy path (reason unchanged):
   `Sample.Identity.Complete`, `Identity.Size == OriginalBytes`,
   `Identity.MtimeNs == OriginalStatMod` (nanoseconds). This is the same trust as the
   mount stat check, from the NAS filesystem instead of the SMB cache, plus an inode,
   generation, ctime and a digest of 8 MiB of the file.
2. `OriginalHelper = {Identity, Sample, Process}` from that `Sample`;
   `RecoverySample` is set to the same digest when empty (both use `internal/sampledigest`,
   which is what the existing metadata-mode checks of a recovery file compare against).
3. The helper-observed probe of the recovery directory and `RecoveryProof = "helper"`
   as in stage 5. Mode is chosen once (`RecoveryMode` empty), exactly as now.
4. Everything stage 5 does for the move itself is unchanged: before the rename the helper
   `Sample` must equal the proof in every field including ctime, else a full read of the
   original compared with `OriginalSHA` (rare: something touched the file; it is also the
   truthful check, since `OriginalSHA` is the real digest); after the rename the same
   inode (all fields but ctime) and the same digest, else a full read accepted only if
   it equals `OriginalSHA`.
5. After the move, metadata-mode checks of the recovery file keep their cheap form: a
   sampled-range comparison of `BackupPath` with `RecoverySample` over the mount (about
   8 MiB), never a full read.

**What must not change.** No path may fall to a full read of the original in the normal case;
no helper answer ever decides "the original is gone"; the mode never changes after the
intent is durable; restore/move-back/discard keep stage 5 semantics.

**Questions for review.**
1. Is the helper `Sample` (identity + 8 MiB digest + mtime/size equal to the transcode's)
   at least as strong as the mount's size + mtime for deciding "this is the file that
   was transcoded"? Where could it be weaker (mtime restored after a rewrite, a rewrite
   outside the sampled ranges)? Is it acceptable that, as today, bytes outside the
   sampled ranges are never compared in this mode?
2. Does every metadata-mode check that runs after the move (restore, move back, discard,
   duplicate guard, `promotionOriginalProved`, `verifyPromotionRecovery`) handle a
   promotion with `RecoveryProof="helper"` and `OriginalSHA` never read? Which one still
   assumes a copy or a mount fingerprint?
3. The preserve step is re-run after a restart. What happens if the process changed
   between the proof and the rename (proof from an earlier process counts for nothing)?
   Is a full read of 1-2 GB acceptable there?
4. Should a Sonarr/Radarr (non-filesystem) metadata-mode promotion get the same
   treatment, or only `service == filesystem` first?
