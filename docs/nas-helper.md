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
deadline from the caller.

Required from the second review for the helper's API: no expected value is
ever sent to it; a job is created for the step that uses it; the sampling
function is shared so the digests are identical.
