# Podcast pipeline review — 2026-10-07

Three reviewers examined durability/classification, worker/ASR/audio, and UI/MCP/MoonStation contracts. Corrections received independent cross-review. No P1/P2 findings remain in the reviewed scope.

## Corrected findings

- Cancellation preserves semantic files created by another producer and validated published candidates; cleanup owns only derived private scratch.
- Source/candidate paths cannot invade worker state or work directories, including symlink aliases.
- Publication checks durable cancellation under the job lock at the atomic commit, for local files, external mounts and direct SMB. Copies, hashing and readback stay outside that lock. Failed terminal-marker writes do not permit later publication. SMB retries repeat the guard.
- Podcast publication fails closed when atomic no-replace rename/link is unavailable. Existing video publication retains its behavior.
- Podcast capability evidence follows the real bytes of ASR, FFmpeg and ffprobe, including Homebrew symlinks and same-path replacements.
- Review pagination measures encoded bytes before recording receipts, including multibyte text and HTML escaping. Approval requires every delivered review page.
- Keyed MCP submissions reuse their durable action after completion, failure or cancellation; concurrent admission and reconciliation produce one ASR. Changed inputs/policy are rejected, and explicit retry remains available.
- UI admission preserves the browser submission key after a lost acknowledgement and respects the explicit MoonStation hook key.

## Validation

- Full Go suite with `CGO_ENABLED=0`, `go vet`, formatting and diff checks.
- 171 Node UI/hook regressions and 18 Python benchmark tests.
- Focused race tests for concurrent coordinator admission/reconciliation and filesystem/SMB cancellation guards and recovery.
- Real coordinator→HTTP worker→FFmpeg→validated publication E2E using a scripted ASR fixture.
- Native M1 Max smoke: Apple Speech produced 28 timed units over a 10-second Generation Why fixture; a deliberately selected 480-ms test cut produced 9,520 ms of decoded MP3, with the original preserved. The test also verifies durable ASR recovery and byte-identical no-cut MP3 output. Its test label is synthetic, not an ad-detection accuracy measurement.

## Scope and remaining activation

Analysis belongs to the connected orchestrating LLM, which can delegate to Luna. No model client or additional scheduler is installed. UI and MCP share the existing Action Engine, SQLite leases and worker queue.

The MoonStation adapter preserves the caller's existing feed/enclosure/GUID contract, but still requires wiring into the separate application. Audiobookshelf, alternate ASR/codecs and silence snapping are deferred. Neither the native smoke nor the earlier full-episode benchmark establishes human listening acceptance or precision/recall. External model cost is not measured.
