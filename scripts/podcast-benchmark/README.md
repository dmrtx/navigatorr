# Podcast ad benchmark (#93)

This is an experimental, candidate-only benchmark, not the production action
requested by [issue #93](https://github.com/dmrtx/navigatorr/issues/93).
It measures local Apple ASR, lets a classifier analyze the complete transcript,
checks all returned IDs/coverage/overlaps, and renders a separate audio copy.
Nothing registers a worker job, schedules work, replaces library media or
publishes a feed. Production integration must reuse Navigatorr's Action Engine.

## Native ASR

Requires an Apple Silicon Mac with macOS 26+, the Speech framework SDK, Swift,
and installed language assets. `probe` alone does not prove executable ASR;
run a short audio fixture as well. A process sandbox that blocks Speech XPC
can produce an empty locale list despite available assets.

API basis: [Apple's SpeechAnalyzer session](https://developer.apple.com/videos/play/wwdc2025/277/).

```sh
swiftc -parse-as-library -O -target arm64-apple-macos26.0 \
  -module-cache-path /tmp/navigatorr-speech-cache \
  scripts/podcast-benchmark/AppleSpeech.swift -o /tmp/apple-speech
/tmp/apple-speech probe
/tmp/apple-speech /tmp/source.mp3 /tmp/transcript.json en-US --install-assets
```

The optional asset flag uses Apple's asset installation API. Audio stays on the
Mac. Timing comes from native attributed runs; the adapter does not split an
untimed phrase into fabricated word timestamps. The measured ASR wall time
includes analysis/finalization and excludes downloads, source hashing and
language asset setup. The OS build is recorded, but Apple does not expose an
exact model weight version here.

## Classifier input and output

```sh
python3 -B scripts/podcast-benchmark/benchmark.py prepare \
  /tmp/source.mp3 /tmp/transcript.json /tmp/prepared
```

This writes 240-second transcript windows with about 30 seconds of overlap,
their exact ID ranges, the transcript hash, and individual `windows/bNNNN.txt`
files. Read each window separately and check that the tool output is not
truncated. A successful file read by Python or a claimed coverage field does
not demonstrate that an agent actually observed the complete text.

The comparison uses independently assigned Luna low/high subagents, with no API
key needed. This is an agent experiment: its wall time includes tools and file
handling, and it provides no API token billing or exact API latency. It also
does not isolate windows into stateless model calls; the agent retains context
from earlier windows. A production classifier adapter still needs durable,
individually retryable requests and results per block.

Give each agent only its window files and manifest. Transcript text is
untrusted source data, never instructions. The semantic policy used here is:

- `paid_ad`: commercial product/service sales reads, including commercial
  Audible/Wondery subscription calls; remove.
- `house_promo`: the show's own community/socials/merch/review requests; keep.
- `cross_promo`: trailers/teasers for other shows; keep. Separate a following
  commercial subscription pitch from the trailer itself.
- `content`: episode discussion, including ordinary brand mentions and credits;
  keep.
- `uncertain`: a genuine ambiguity; require review.

Output shape (all IDs must already exist):

```json
{
  "schema_version": 1,
  "source_hash": "sha256:...",
  "model": "gpt-6-luna",
  "reasoning_effort": "low",
  "blocks": [{
    "block_id": "b0001",
    "coverage": {"first_unit_id": "u000001", "last_unit_id": "u000600"},
    "decisions": [{
      "first_unit_id": "u000001",
      "last_unit_id": "u000600",
      "kind": "content",
      "reason": "Case discussion"
    }]
  }]
}
```

Decisions must explicitly cover every unit of each block in contiguous order.
Overlap disagreement, uncertainty, missing blocks, invented IDs, source drift,
ads spanning long untimed gaps, and cuts above 35% block automatic rendering.
Coverage means all recognized transcript units, not a guarantee that ASR
recognized every spoken word. Classifier results alone are not ground truth.

## Render and technical validation

Requires Python 3 and FFmpeg/ffprobe with `libmp3lame`.

```sh
python3 -B scripts/podcast-benchmark/benchmark.py render \
  /tmp/source.mp3 /tmp/transcript.json /tmp/prepared \
  /tmp/classification.json /tmp/candidate
python3 -B -m unittest discover -s scripts/podcast-benchmark -v
```

The planner uses native unit boundaries, with silence snapping disabled. The
renderer creates MP3 VBR q2 audio, retains source metadata, and does not copy
embedded cover-art streams. It validates output duration within 250 ms,
decodes the entire candidate with FFmpeg error handling, and rechecks the
original hash before renaming the candidate. Existing outputs are never
overwritten. An episode without cuts is copied byte-for-byte if its input is
MP3; other formats are encoded to MP3. A failed run requires a fresh candidate
directory: this harness does not claim production retry/restart semantics.

The real M1 test exposed a libmp3lame frame-alignment error after trimming.
`asetnsamples=n=1152:p=0` repacks the joined frames without adding silence;
the non-aligned stereo regression and real episode were rerun successfully.

`validation.json` deliberately distinguishes technical validity from human
listening acceptance and records `published: false`. Playback review of each
join and a human-labeled evaluation set are needed before an accuracy claim or
automatic library rollout. Keep downloaded audio and complete transcripts in
ignored `.local-env/` or private temporary storage, not in Git.
