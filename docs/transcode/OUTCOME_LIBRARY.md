# Size-mode outcome library

A size-mode search measures many settings on sampled windows. Before this
library its result lived only in the action's state, so a second action on the
same file, or on the next episode of a series, started from nothing. The
library keeps every finished search in the `transcode_outcomes` table
(migration 12) and uses it in two bounded ways: exact-match reuse and sibling
warm start. Neither changes a pass/fail limit, skips final validation or skips
promotion approval.

## What is recorded

One row per finished size-mode search of `transcode_media`, keyed by action
and benchmark job (a hardware continuation's x265 search has its own row):

| Group | Fields |
| --- | --- |
| Source fingerprint | SHA-256 of the bytes, size, duration, video codec, bit depth, width, height, fps |
| Library identity | Sonarr/Radarr service and series/movie id, season and episode when known; `folder_key`, a truncated SHA-256 of the source's directory, as the fallback |
| Policy | mode, policy version, frozen policy digest |
| Search | outcome (`winner`, `no_winner`, `search_budget_exhausted`), the winner (the candidate as sent, the worker's winner and its evaluation), and every measured point (encoder, settings, predicted size, VMAF mean/p5/worst window, CAMBI mean/max, verification result) |
| Final validation | verdict (`pass`/`fail`), candidate bytes, VMAF mean/p5/worst window, CAMBI, window count |

The identity comes from the `library_context` input, else from the parent
`transcode_batch` (its `service`/`series_id`, and the batch item's `S01E02`
label). No media path is stored or logged: the folder is kept only as a digest.
Rows have no foreign key to `action_instances`, so archiving or purging an
action keeps its outcome.

Recording never fails an action. A search that ran out of time while the worker
was busy with other jobs is not recorded, because it says nothing about the
source. Final validation is attached only when the plan came from a benchmark
winner. A worker's fail is recorded when the encode ends. A pass is recorded
only at the coordinator's validation step, once the evidence matches the
candidate and plan and the encoded size is within the policy; an encode over
the size limit is recorded as a fail (`insufficient_savings`), and evidence the
coordinator refuses records no verdict. A candidate the user later rejects at
promotion keeps its pass: reuse still validates the new encode and still asks
for promotion approval.

## Exact-match reuse (search step)

When the request is built, the library is read for the same source SHA-256 and
the same frozen policy digest. Status shows the matches under `outcome_library`,
with a note such as "This exact source already failed with policy
transcode-modes-v3 (sha256:…) in action …".

The search is skipped only when all of these hold:

* size policy v3, and the action is not past a search review, waiting for one,
  or continuing after a failed hardware result;
* `reuse_library_outcome` is not set to anything but `true` (batches pass it to
  their children);
* the newest conclusive record (a no-winner, a time-out, or a winner whose
  encode was validated) is a winner whose full encode **passed** final
  validation, and whose encoded size is within this policy's size limit;
* that winner is libx265 (hardware winners are reported, not reused);
* this request proposes the same candidate again, compared by every setting
  (`candidateIdentity`), so a changed ladder or a worker without the same
  Main10 support does not match.

Then no job is sent to the worker. The request is still built, kept and
digested as if it had been submitted, and the wait step judges the recorded
winner through the same code as a worker's result: the same winner checks
against the frozen request and policy, the same winning plan, and the same
`QualityValidationPlan` on the final grid. The full encode is validated as
usual, and a failure there is recorded, which stops further reuse of that
setting. The two-round search review is unchanged: it reads action history and
is checked before reuse, and a reviewed round always searches.

A reused action's own row names the action it reused (`reused_from`).

## Sibling warm start

When a fresh size v3 search is built, the coordinator looks for validated
winners of siblings: the same series and season, else the same series, else the
same folder (`folder_key`). It keeps winners whose full encode passed final
validation and that are libx265 constant quality at the preset of the
request's initial ladder, one per source (the newest), up to 20, and excludes
the source itself. The prior is the median of their CRFs, rounded to the lower,
higher-quality side. It is sent as the request's `warm_start` and shown in
status (`warm_start`: CRF, basis, sibling count and the actions it came from).

The worker uses it only in the initial x265 ladder, and only when the search
prunes by quality. The ladder's highest-quality rung (the leader) is measured
first as before; when it passes, the remaining rungs are measured from the rung
at or just below the prior, two at a time, instead of all at once:

* while the highest measured passing rung has an unmeasured rung above it and
  below the lowest failing one, that rung is measured next;
* when nothing above the leader has passed yet, the walk goes down from the
  lowest failing rung;
* the walk ends once the boundary between passing and failing is bracketed.

Rungs it leaves unmeasured are reported with a reason: larger than a rung that
passed, or dominated by a rung that failed quality. If the rung the walk ends
on later fails verification on quality, the rungs it left below are measured
then, as the full ladder would have done. Refinement, the quality fallback,
hardware search, verification and final validation are unchanged, and so are
all limits: the prior changes only which rungs are measured and in what order.
The search evidence reports where the walk started, which rungs it skipped, or
why the prior was not used (`no_quality_pruning`, `no_x265_rungs`,
`several_x265_families`).

The coordinator sends the field only to a worker that advertises
`automatic_warm_start`, so the server must be deployed before the worker; an
older worker searches as before. No prior is sent with a hardware continuation,
or when `reuse_library_outcome` is false, which turns off both reuse and the
prior. An exactly reused search keeps the request it was digested with but
reports no prior, since nothing is measured.

## Content fingerprint (design only, not built)

Exact reuse needs identical bytes. A different release of the same episode
(another group's encode, a remux with extra audio) has a new SHA-256 and gets
no reuse, though the sibling prior still applies when the library knows the
series and season. A content fingerprint could link such releases.

| Option | How | Cost | Strengths | Weaknesses |
| --- | --- | --- | --- | --- |
| Audio fingerprint (Chromaprint on the first audio track) | decode audio once, ~1-2 s CPU per hour | low | robust to video re-encodes, cuts at the same place | dubbed or edited releases differ; a remux without the original language misses; music-heavy or silent content collides |
| Perceptual hashes of sampled frames (pHash/dHash at fixed timestamps) | decode ~20 frames at fixed fractions of the duration | low to medium | follows the picture, which is what the search measured | sensitive to different cuts, intros, letterboxing and frame-rate changes; needs alignment by duration |
| Both, matched only when duration agrees within ~1 s | combine the two | medium | few false matches | most code and the most to tune |

What a match could safely mean differs from exact reuse. A different encode of
the same content has different grain, noise and artefacts, so another
release's winning CRF is only a hint, as strong as a sibling's. A fingerprint
match would therefore feed the warm-start prior (closest evidence first), never
skip a search. The cost is a new probe step on the coordinator or worker and a
second index on the table.

Recommendation: do not build it yet. Exact reuse plus the series/season prior
cover the common cases (re-running a file, the next episode). Revisit after the
library has data showing how often the same episode arrives as a different
release.
