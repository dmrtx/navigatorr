# Direct SMB acceptance probe

This probe is the safety gate before Navigatorr may replace macOS `smbfs` with
a userspace SMB client. It is deliberately a separate binary and is **not**
wired into `navigatorr-transcode`; running it cannot resume or mutate a
transcode job, including E04.

It proves, using the same library proposed for the worker:

1. direct SMB2/3 authentication with message signing;
2. download of an existing small disposable fixture;
3. exclusive upload plus full SHA-256 readback;
4. same-share rename refusing to overwrite an existing destination;
5. rename to an absent destination plus full SHA-256 readback; and
6. cleanup of every uniquely named probe artifact.

The pinned SMB library's `Rename` sends `ReplaceIfExists=0`. The live collision
test is still mandatory because library source alone does not prove Synology's
server behavior.

The dependency is deliberately pinned at `v1.1.0`. Do not update or replace it
without re-auditing the wire-level rename flag and repeating every live gate.

## Configuration

Create a root-owned or service-user-owned YAML file and a separate password
file with mode `0600` (Ansible Vault should provision both):

```yaml
server: "192.168.70.72:445"
share: "media"
username: "navigatorr-transcode"
domain: ""
password_file: "/Library/Application Support/Navigatorr/smb-password"
base_path: "navigatorr-probe"
# Replace this example value with the value printed by --print-seed-sha256.
expected_sha256: "0000000000000000000000000000000000000000000000000000000000000000"
timeout: "2m"
max_source_bytes: 67108864
```

`base_path` must be a non-empty directory dedicated to disposable probe data.
The source argument is always relative to it; absolute paths, backslashes, NUL,
and traversal are rejected. `expected_sha256` is mandatory, so a corrupt
download cannot become its own false reference. Use only a small artificial,
disposable source fixture—never E04 or another real episode.

The artificial fixture is fixed at `navigatorr-probe/input.bin`, is 64 KiB,
and has deterministic contents. Obtain its compiled-in digest without reading
credentials or opening a network connection:

```sh
./navigatorr-smb-probe --print-seed-sha256
```

Put that digest in `expected_sha256`, then create the fixture in a separate,
explicit operation:

```sh
./navigatorr-smb-probe \
  --config /path/to/smb-probe.yaml \
  --source input.bin \
  --seed-fixture \
  --allow-seed-fixture
```

Seeding refuses any `base_path` other than `navigatorr-probe`, any source name
other than `input.bin`, a mismatched digest, a source-size limit below 64 KiB,
or an existing destination. It creates the directory non-recursively, uploads
with exclusive-create semantics, and verifies a full readback. On failure it
removes only the exact fixture and directory it created; it never recursively
deletes the namespace.

After the fixture exists, run the acceptance gate separately:

```sh
./navigatorr-smb-probe \
  --config /path/to/smb-probe.yaml \
  --source input.bin \
  --allow-write-probe
```

Success requires `ok`, `readback_proven`, `no_clobber_proven`,
`rename_proven`, and `cleanup_complete` all to be `true`.

## Promotion gate

Do not connect this package to the production worker until the exact built
binary passes under the LaunchDaemon service identity in all four conditions:

- initial headless run with no mounted network share;
- repeat after M1 reboot;
- repeat after replacing/rebuilding the binary; and
- an intentional existing-destination collision that leaves the sentinel and
  uploaded partial byte-for-byte intact.

Any failure keeps the current worker backend unchanged. Do not grant Full Disk
Access, mount the share, touch the original, or retry ffmpeg for E04 as part of
this probe.

The live order remains: first prove the same operations with `smbclient`, then
run this exact binary under the LaunchDaemon service identity, repeat after a
reboot, and repeat after a genuine binary replacement. The fixture and its
dedicated directory may be removed afterward only by exact path, never by a
recursive cleanup.
