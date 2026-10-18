# Synthetic observability baseline — 7 October 2026

Measured against base `5b7d378fe55a61147b66d92925b9a30682af97fb` plus the uncommitted implementation diff in this delivery. Environment: Darwin arm64, Go 1.27.1. These are temporary synthetic files, injected storage and subprocess fixtures; no production library source or replacement was used. They do not compare against a prior release or reproduce the historical eight-file incident.

## Reproducible fixtures

```sh
GOCACHE=/private/tmp/navigatorr-review-gocache go test ./internal/transcodeworker -run 'TestSlowSynthetic|TestEightSyntheticSearches|TestFinalSizeRejection|TestScheduler|TestSyntheticObservability' -count=1 -v
GOCACHE=/private/tmp/navigatorr-review-gocache go test ./internal/transcodeworker ./transcode ./tools ./maintenanceui -run 'TestScheduler|TestFinalSizeFrozen|TestFinalSizeRejection|TestSyntheticObservabilityBaseline|TestEightSyntheticSearches|TestWorkerHealthMCP|TestRealWorld_MCPSchemaFootprint|TestIOOpt_|TestPR3|TestWorkerAvailability|TestConnectedWorker' -count=1 -v
```

Local HTTP/process regression tests require execution outside a filesystem-only sandbox. The expanded command above passed. Raw temporary outputs were recorded in `/private/tmp/navigatorr-observability-tests.txt` and `/private/tmp/navigatorr-observability-baseline.txt` during this run; this document retains the measured values needed for review without requiring those temporary files.

| Fixture | Actual measurement | Interpretation |
| --- | --- | --- |
| Mapped fake NAS source, 1,048,576 bytes, 20 ms injected download delay | Initial wall 32.001625 ms; reuse wall 106.166 microseconds | One confirmed source transfer; second cache lookup hit with 0 NAS bytes read. Timing is this synthetic test, not a NAS throughput claim. |
| Benchmark and encode source leases | Two job-local leases, one initial download | Changing encoder plans changes plan digest while source bytes remain reusable. Cache stores source content, not measured candidate quality. |
| Same source path/metadata, changed byte content and digest | Second download and different cache entry | Old cache evidence is not reused after source-byte change. Existing active leases remain valid; source cache is not duplicated. |
| Blocked 1,024-byte fake transfer with two configured slots | Scheduler wall 15.520208 ms; 2/2 slots occupied; independent queued job admitted | `reading_source` remains observable while transfer is blocked. This proves independent admission under the fixture, not a production latency SLO. |
| Eight fake-process sample searches, one nonqualifying configuration each | Batch wall 1.098765959 s; sum of measured search wall 0.885465 s; budget 2 s/source (16 s configured total) | Every source obtains no winner; all eight remain independent. Fake subprocesses emit 67-byte sample candidates for a 100-byte synthetic source with 120-second declared duration; sample extrapolation violates size policy. No real FFmpeg encode or global impossibility claim. |

Per-source measured search wall seconds and remaining configured budget:

| Source | Search seconds | Remaining seconds | Exit |
| --- | ---: | ---: | --- |
| 0 | 0.253865 | 1.746135 | all_candidates_invalid |
| 1 | 0.130038 | 1.869962 | all_candidates_invalid |
| 2 | 0.064988 | 1.935012 | all_candidates_invalid |
| 3 | 0.106724 | 1.893276 | all_candidates_invalid |
| 4 | 0.119792 | 1.880208 | all_candidates_invalid |
| 5 | 0.072393 | 1.927607 | all_candidates_invalid |
| 6 | 0.069042 | 1.930958 | all_candidates_invalid |
| 7 | 0.068623 | 1.931377 | all_candidates_invalid |

## Real encoder and final validation fixture

`TestRealModeBenchmarkWinnerFinalValidationAndSize` creates a two-second, 320×180, 24 FPS SDR FFV1 source at native 8 or 10 bits. It runs real software/hardware sample candidates when reported by executable capabilities, selects via the common mode selector, encodes the selected full candidate, performs structural and sampled final VMAF/CAMBI validation, validates the frozen final byte policy and verifies unchanged original SHA-256. A worker without the exact executable model reports an explicit skip.

This run used the compatible local libvmaf runtime also documented in `TRANSCODE_MODES_REVIEW.md`:

```sh
DYLD_LIBRARY_PATH=/private/tmp/navigatorr-mode-calibration/vmaf-build/src GOCACHE=/private/tmp/navigatorr-review-gocache go test ./internal/transcodeworker -run '^TestRealModeBenchmarkWinnerFinalValidationAndSize$' -count=1 -v
```

All six mode/depth cases executed and passed. `size` and `quality` each evaluated two candidates (software and hardware); `x265_preserve` evaluated libx265 only. Every fixture winner was libx265. Native 8-bit source: 315,181 bytes → final 146,633 bytes, final sampled VMAF 100.000, CAMBI mean 0.318. Native 10-bit source: 231,297 bytes → final 147,191 bytes, final sampled VMAF 100.000, CAMBI mean 0.023. Each final quality sample covers 0.5 seconds of the two-second source; this is not a perceptual inspection of every frame or general visual calibration. Native 10-bit mode wall intervals were 1.188677 / 1.408856 / 1.196185 seconds (size / quality / preserve), including benchmark and final work. Total test duration 7.64 seconds. The test verifies policies and unchanged originals, not production speed or hardware superiority.

## Health and accounting guarantees exercised

Each scheduler sweep records its own observation, last success, elapsed wall time, started count, consecutive errors and allowlisted diagnostic. Injected independent/both failures and an actually malformed durable record produce degraded health; recovery clears the active diagnostic without resubmitting. A corrupt queue record is surfaced while valid independent queued jobs remain eligible. Repeated identical classes are logged once until recovery/class change, without raw paths, URLs or credentials. HTTP liveness and readiness do not imply scheduler progress. Missing/expired/future evidence normalizes to unknown in the daemon, transport and cached shared UI/MCP observation. MCP reuses the same cached observation as UI.

Final size tests reject 95/100 and growth; 85/100 passes the exact 15% boundary. Integer-byte rational comparison also distinguishes an extra byte at 10^18-byte scale. Rejected candidates are never published, original bytes remain intact, and typed policy reasons reach the coordinator. Publication retries retain encoded checkpoints. Completed local publication never claims NAS write bytes. NAS transfer counters are optional: only confirmed mapped storage transfers are counted; uncertain partial writes/read failures remain unknown, and verification of an already-published destination is not counted as another transfer.

## Measurement boundaries and conclusion

Worker status exposes queue, source hash, probe, staging, encode, final validation, publication and cleanup wall intervals. Benchmark status exposes staging/probe/reference samples/sample encode/sample validation and search wall consumption. Source cache lookups provide measured hits/misses and confirmed logical mapped NAS read bytes. Overlapping encode/metric intervals are work measurements and must not be summed into total wall duration; total wall/search durations are separately recorded. `null`/omitted byte evidence means unknown, not zero. Byte counts are logical completed transfers, not packet bytes, physical filesystem space, avoided quality verification or realized library savings.

Coordinator inventory/hash/promotion timing is added separately by the action projection in this delivery. No CPU utilization/cycles or rigorous separation of synchronous dependency/I/O wait from CPU activity was measured. Search budget begins after staging and queue admission; a durable start prevents resetting the finite worker search deadline after restart. Reuse after restart therefore retains the original deadline rather than promising that all downtime is excluded from that deadline. Human decision time is outside worker search measurement. After a resumed search, earlier compute spans have no complete durable measurement: `SearchSeconds` stays unknown and the coordinator retains the reservation instead of refunding the unobserved work.

The fixtures demonstrate useful cache reuse, safe budgeted exits and independent admission while a source transfer waits. They do not establish a production bottleneck or justify a new scheduler/cache. This delivery preserves the existing pool, hashes, validation and cache authorities. Production representative measurements, broader visual policy validation and complete browser acceptance remain separate evidence; these timings alone do not close every #91/#92 acceptance criterion.
