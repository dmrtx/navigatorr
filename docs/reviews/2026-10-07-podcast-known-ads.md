# Confirmed podcast ads: first-pass validation

The first pass uses the existing Action Engine and worker queue. It recognizes
previously reviewed recordings, verifies their entire waveform and confirms
all normalized native transcript words before admitting ID labels. Unknown and
boundary units still go to the orchestrating LLM. ASR remains on the original;
this benchmark reused existing native transcripts and did not measure new ASR.

## Real audio

Five seeds were derived from the approved Generation Why #703 classifications,
separating paid ads and cross promotions even when the final cut merged them.
The final 5.04-second ad was below the eight-second library minimum and remained
for the LLM. Original source SHA-256:
`0f13e90997edd6a37a4dad03b02bd88860898bd41b796387e5160dacbcb6d1db`.

The held-out copies were #700 from the earlier public RSS benchmark, SHA-256
`caeb7f548a323e83d0c7891161a7eb7c05dcdc4e931e0d9fda371a0d036d3cf4`,
and the preserved production download of #702, SHA-256
`b18c999216055742bb71697ff1df72b563d707ca61780da8d3c29183c2b793fd`.
Dynamic ad insertion means copies of the same episode can differ; native
transcripts and source hashes were checked against these exact files.

Measured on the production Apple M1 Max, using its FFmpeg binaries and an
isolated test library. Audio was already local. Timing includes PCM decoding,
spectral indexing, alignment and complete waveform verification; it excludes
network copies, native ASR, LLM classification and final render.

| Copy | Audio duration | Acoustic candidates | Accepted native units | Accepted native speech | Decode | Search/verify | First pass |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| #703 training | 47:51.092 | 5 | 414 | 158.280 s | 2.198 s | 1.069 s | 3.267 s |
| #700 held out | 51:33.708 | 1 | 105 | 45.060 s | 2.358 s | 1.155 s | 3.513 s |
| #702 held out | 56:24.999 | 1 | 0 | 0 s | 2.568 s | 1.251 s | 3.818 s |

#700 repeats the full 46.620-second Killer Psyche/Lindsay Clancy promotion at
18:24.441–19:11.061. Native boundary units remain for the LLM, so accepted speech
is shorter than the audio reference. ASR wrote “3” in one copy and “three” in
the other; deterministic normalization treats equivalent small English integers
identically without equating different numbers/offers.

#702's similar 19.680-second candidate passed waveform correlation at 0.9400,
but its normalized native text differed. No automatic labels were admitted.
No automatically labeled unit contradicted the previous Luna classifications
in any of the three copies: **0 ms of conflicting content/category labels**.
Those classifications are the benchmark reference, not independent human
ground truth. This small dataset does not establish precision/recall or prove
that all ads will be removed. No billed token/cost measurements were taken.

## Regression and review

Tests include repeated clips at different offsets and gains, MP3 recoding,
changed final offers/audio, partial clips, silence, tones and a shared loud
music bed with different quiet voices. The shared-bed case can pass acoustic
correlation; the independent native-text check rejects automatic labels.
Quiet edges are excluded from alignment probes but still verified in full.

Integration tests cover category-specific seeds, no learning from automatic
labels, scoped libraries, deduplication, delivery receipts for unknown units,
mixed coverage, frozen snapshots, lost submit acknowledgements, cached-ASR
first pass, same-ID restart recovery, revocation and completed-render recovery.
Additional regressions exercise native end times up to 100 ms past duration,
metadata-only tampering and render proofs exceeding the old 1 MiB HTTP limit.

An independent agent review found and verified fixes for shared-bed matching,
terminal recovery after revocation, proof limits, native end tolerance,
deduplication/cancellation lock scope and bounded PCM output. The final review
reported no remaining P1/P2 findings. Normal full-decode, source integrity,
exact native cuts, review and single atomic publication remain required.

Reproduce with private original/native/classification fixtures:

```sh
NAV_AD_LIBRARY_BENCHMARK=/absolute/fixture.json \
NAV_AD_LIBRARY_BENCHMARK_RESULT=/absolute/result.json \
  go test ./internal/transcodeworker -run TestRealAdLibraryBenchmark -v -count=1
```

Fixtures contain copyrighted audio and episode text and remain outside the
repository. Results are recorded here; no audio or full transcript is committed.
