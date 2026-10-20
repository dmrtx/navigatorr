# How much transcode work can move to the Apple media engine — experiment

Measured 2026-10-19 on one Apple M4 Mac (10 CPU cores, 32 GB), ffmpeg 9.0.1
(Homebrew build with `libx265`, `hevc_videotoolbox` and `libvmaf` 3.2.0),
against the repository state at `dbf2251`. Everything was done on synthetic clips
generated for this test. No library media, no production job, no search, apply or
promotion was involved, and no code in the repository changed.

This is phase 1 of a two-phase task: measure first, then propose. Section 9 is
the proposal; it is not implemented.

## 1. Short answer

| Question | Answer |
| --- | --- |
| Can the *encode* move to the media engine? | Yes, and it is 4x (1080p) to 7x (2160p) faster than `libx265` medium while using 30-45x fewer CPU-seconds per frame. |
| Does it cost size at the same quality gates? | Only if VideoToolbox is run the way the repository runs it today. With a normal GOP (`-g 240`) the VideoToolbox file was 0.86x-1.37x the x265 size on these clips (median about 1.0x); with the current argv (no `-g`) it was 2.8x-4.0x. |
| Can the *quality metrics* move? | No. libvmaf and CAMBI are CPU-only here. For a 1080p candidate they cost about twice what the x265 encode costs, so a search stays CPU-bound. |
| Can *decoding* move? | Yes, bit-identically in every case tested (H.264 8-bit, HEVC 10-bit), but the saving is small: 0-20 % wall on metric/validation passes and about 6 % of CPU in a 2160p x265 encode. |
| Does a VT encode next to x265 jobs add throughput? | Yes, because it only needs about one core and the media engine, but it slows each x265 job by 6-14 %. |

The most important finding is not about the GPU at all. Size mode v3 excludes
VideoToolbox because its candidates "rarely won its smallest-qualifying
selection". The candidates the repository builds (`action/benchmark_template.go`
adds `-q:v 65/70/75` with no `gop_size`) inherit the encoder's default keyframe
interval of 12 frames. Section 4 shows that alone makes the output 2.7-3.2x
larger. The historical "VT never wins" result was measured with that handicap and
says nothing about what VT can do with a normal GOP.

## 2. Method

**Clips.** Four 24 fps clips at 1920x1080 (8 s) and 3840x2160 (5 s), 8-bit
4:2:0, stored lossless (FFV1) and converted to raw Y4M for the encodes so that
source decoding does not limit the encoder. Each is a multi-octave noise texture,
camera-panned with a slowly rotating (sub-pixel) field, a second independently
moving and rotating patch, and per-frame temporal grain:

| Clip | Character |
| --- | --- |
| `detail` | fine texture, slow motion, light grain |
| `grain` | soft texture, heavy temporal grain |
| `dark` | low exposure (shadows), light grain |
| `motion` | fast pan and rotation, light grain |

A first set with integer-pixel panning and no grain was discarded: x265 predicted
it almost perfectly (98 fps, 330 kbit/s at CRF 28), which is not representative.
The final clips are still a **stress texture, not film**. They have no flat
areas, faces, cuts or compression history. Treat absolute sizes as indicative and
ratios as the thing under test; Section 8 lists what real content could change.

**Encoders** (all main10, as size mode freezes for 8-bit sources):
- `libx265 -preset medium -crf N -pix_fmt yuv420p10le`, CRF 22-30 (the v3 ladder), plus CRF 31-36 only where CRF 30 overshot the gate (to show the ladder's cap, not as candidates).
- `hevc_videotoolbox -q:v N -profile:v main10 -pix_fmt p010le`: (a) as the repository builds it today ("gop12"), (b) with `-g 240` ("vt-g240", the x265 keyint is 250), (c) with `-g 240 -bf 2` ("vt-g240-bf2").
- Constant and average bitrate VT at the x265-matched bitrate (Section 3).

**Quality.** libvmaf with `vmaf_v0.6.1`, scored through the repository's graph
(both inputs converted to 10-bit, restamped by frame index, `shortest=1`,
`repeatlast=0`, 10 threads). The production model `vmaf_v1.0.16_3d0h` could not be
loaded: this libvmaf build lacks the speed feature extractors it needs, the same
limitation the repository's own tests work around by falling back to v0.6.1. The
mean, nearest-rank p5 and worst rolling window (2 s here; windows are 5 s in
recipes) are computed the way `quality.ParseVMAF` does. A candidate "passes" when
p5 >= 84 and worst window >= 85; the **selection margin** of size v3 requires 1.5
more on both (p5 >= 85.5, window >= 86.5). Rows below use that margin unless they
say "gate only". CAMBI was also checked on every winner (Section 3).

**Speed and CPU.** Wall time and user+system CPU time of the ffmpeg child
(`getrusage`). Other work was running on this Mac for much of the session, so
timing runs waited for at least 70-85 % host idle and each was repeated; the
quality and size numbers are deterministic and unaffected. Where a figure is a
single run it is stated. VT speed and a 1080p/2160p x265 encode on this chip are
not the production worker's (an M1 Max); re-measure there before relying on an
absolute number.

## 3. Matched quality: size

Smallest candidate that passes the gates with the 1.5 selection margin. "x265" is
restricted to the v3 ladder (CRF 22-30).

| Clip | x265 CRF / kbit/s | VT today (gop12) q / kbit/s / vs x265 | VT `-g 240` q / kbit/s / vs x265 | VT `-g 240 -bf 2` q / kbit/s / vs x265 |
| --- | --- | --- | --- | --- |
| detail 1080p | 28 / 849 | 53 / 3370 / 3.97x | 51 / 923 / 1.09x | 51 / 852 / 1.00x |
| dark 1080p | 24 / 875 | 60 / 2639 / 3.02x | 57 / 754 / 0.86x | 57 / 732 / 0.84x |
| motion 1080p | 28 / 836 | 55 / 3040 / 3.63x | 53 / 1142 / 1.37x | 53 / 1073 / 1.28x |
| detail 2160p | 30 / 2553 | 50 / 9179 / 3.60x | 46 / 2472 / 0.97x | 46 / 2270 / 0.89x |
| dark 2160p | 26 / 2449 | 57 / 8636 / 3.53x | 55 / 2255 / 0.92x | 55 / 2128 / 0.87x |
| motion 2160p | 28 / 3506 | 53 / 9796 / 2.79x | 53 / 3709 / 1.06x | 53 / 3689 / 1.05x |
| **median** | | **3.6x** | **1.0x** (0.86-1.37) | **0.94x** (0.84-1.28) |

Gate only (no margin), the VT `-g 240` ratios are 0.96, 1.08, 1.37, 0.85, 0.80,
0.81 and the `-bf 2` ratios 0.90, 1.08, 1.28, 0.79, 1.05, 1.05.

Reading these numbers:
- Both grids are coarse. One x265 CRF step changes size by roughly 8-12 %; one
  pair of VT quality values by 10-35 % (VT quality behaves as about 2-step
  increments: 51 and 52 produced identical bytes). Ratios within about +-20 %
  mean "comparable", not "VT wins".
- On `detail 2160p` the CRF 30 ladder cap makes x265 overshoot (p5 87.7 at 2553
  kbit/s); CRF 31 would be 2271 kbit/s at p5 86.0. This is the one place the
  ladder, not the encoder, decides who is smaller.
- The `grain` clip is excluded from the table. x265 fails the gate at every CRF
  22-30 (VMAF p5 62-76 at 0.2-16 Mbit/s: the grain is incompressible noise) and
  VT only passes at q 64-66 and 58-96 Mbit/s. It is a fallback-path stress case,
  not a comparison.
- CAMBI (the repository's banding gate, mean <= 6, peak <= 16) was between 0 and
  0.21 mean / 0.47 peak for every x265 and VT winner. These clips contain grain and
  no smooth gradients, so this says nothing about banding on real dark scenes.
- Constant and average bitrate VT at the x265 CRF-28 bitrate scored VMAF p5
  36-54 and 40-67 (average and constant bitrate respectively): VT's quality mode
  is far better suited to these gates than either bitrate mode, because the
  bitrate modes do not spend bits where the gates measure.

VMAF mean differed less from x265 than p5 did, in both directions; the gates
(p5, worst window) are what bind, so the comparison above holds them equal.

## 4. Why VT "never won": the keyframe interval

The ffmpeg VideoToolbox HEVC encoder uses a 12-frame keyframe interval unless
`-g` is given (`ffprobe` on a default encode: 8 keyframes in 96 frames; x265: 1).
`buildVideoToolboxArgs` emits `-g` only when the plan sets `gop_size`, and the
VT candidates built for size, quality and compact modes do not. Test at `-q:v 55`
on three 1080p clips (all other options identical):

| Option vs default | detail kbit/s (p5) | motion kbit/s (p5) | dark kbit/s (p5) |
| --- | --- | --- | --- |
| default (GOP 12) | 3765 (90.2) | 3040 (87.2) | 2090 (83.4) |
| `-g 240` | **1379 (92.0)** | **1318 (89.9)** | **653 (85.1)** |
| `-bf 2` | 3583 (90.1) | 2938 (86.6) | 2047 (83.3) |
| `-spatial_aq 1`, `-prio_speed 0`, `-power_efficient 0`, `-bf 0` | identical bytes to default | identical | identical |

`-g 240` makes the file 2.7-3.2x smaller *and* raises VMAF. `-bf 2` gives a few
percent. The three options in the last row had no effect on this encoder build and
are not worth exposing as tuning knobs. Quality and compact modes add the same
q65/70/75 candidates without a GOP, so they carry the same handicap; this
experiment did not measure those modes' targets.

## 5. Speed and CPU

Encode of the whole clip from raw input, no metric running, host at 77-92 % idle,
mean of two runs.

| | x265 medium (unpartitioned) | VT |
| --- | --- | --- |
| 1080p throughput | 46 fps | 190 fps (4.1x) |
| 2160p throughput | 6-9 fps (8.8 on `detail`, 6.3 on `grain`) | 54 fps (6-9x) |
| 1080p CPU per frame | 0.169 CPU-s (78 % of 10 cores) | 0.006 CPU-s (11 % of 10 cores) |
| 2160p CPU per frame | 0.83-1.1 CPU-s | 0.017 CPU-s (9-10 % of 10 cores) |

VT encode keeps about one core busy (frame upload and muxing) and is
engine-bound: its speed did not change with `-q:v` (20-80), with `-g`, with B
frames, or with the sample (183-192 fps at 1080p across all quality values).

## 6. Hardware decode: speed, CPU and bit-identity

`-hwaccel videotoolbox` on the input, frames returned to system memory. Sources:
a 32 s H.264 1080p 8-bit stream (x264 CRF 16) and a 20 s HEVC 2160p 10-bit stream
(VT, high quality), both made from the synthetic clips.

| Stage | Software | Hardware | Result identical? |
| --- | --- | --- | --- |
| Decode only, H.264 1080p (768 frames) | 524-573 fps, 6.2-6.6 CPU-s | 291-299 fps, 0.48-0.52 CPU-s | framemd5 identical |
| Decode only, HEVC 2160p 10-bit (480 frames) | 65-66 fps, 57-59 CPU-s | 113-116 fps, 1.1-1.3 CPU-s | framemd5 identical |
| Feeding x265 CRF 26, 1080p | 15.8 s, 127.5 CPU-s | 15.7 s, 125.6 CPU-s (-1.5 % CPU) | **x265 bitstream MD5 identical** |
| Feeding x265 CRF 26, 2160p | 54.6 s, 396 CPU-s | 54.3 s, 373 CPU-s (-5.9 % CPU) | **bitstream MD5 identical** |
| 10 s FFV1 sample extraction, 1080p | 1.88 s, 13.2 CPU-s | 2.13 s, 9.3 CPU-s (-30 % CPU, 13 % slower wall) | framemd5 identical |
| 10 s FFV1 sample extraction, 2160p | 16.1 s, 140.5 CPU-s | 13.9 s, 117.8 CPU-s (-14 % wall, -16 % CPU) | framemd5 identical |
| libvmaf, FFV1 reference + HEVC candidate decoded in hardware, 1080p (192 frames) | 3.7-4.2 s, 30-36 CPU-s | 3.7-3.8 s, 32-34 CPU-s (no measurable gain) | **per-frame scores identical** |
| libvmaf, compressed reference + candidate both in hardware, 1080p (768 frames) | 17.4 s, 138 CPU-s | 14.7 s, 128 CPU-s (-15 % wall, -7 % CPU) | **identical** |
| libvmaf, same, 2160p (480 frames) | 55.6 s, 425 CPU-s | 44.4 s, 346 CPU-s (-20 % wall, -19 % CPU) | **identical** |

Software decode of 1080p H.264 is already 500+ fps, so hardware decode is not
faster there (it is 1.8x slower in wall time at 92 % less CPU). It only wins where
software decode is a real cost: high-resolution HEVC, and metric passes whose
reference is also a compressed stream. The single-run libvmaf rows are within
about 5 % run-to-run noise on this machine except the 2160p one (-20 %), which
two independent hardware variants reproduced (-18 % and -19 %).

Only H.264 8-bit and HEVC 10-bit 4:2:0 were verified. ffmpeg silently falls back
to software for codecs or profiles VideoToolbox cannot decode, and a decoder
difference would change the pixels the encoder and the metric see, so a worker
would need a startup self-check of the same kind it runs for x265 partitioning.

## 7. Where the search spends CPU

Per 1080p frame, 10 threads, from the runs above:

| Work | CPU-s per frame | GPU/media engine? |
| --- | --- | --- |
| x265 medium candidate encode | 0.169 | VT can replace it (0.006) |
| libvmaf (v0.6.1) | 0.157-0.186 | no |
| CAMBI full-reference pass | 0.194 | no |
| Sample extraction (decode + FFV1) | about 0.055 | decode part only (-30 %) |
| FFV1 reference decode for each metric | 0.036 | no (FFV1 has no hardware decoder) |

A candidate that is encoded, scored and CAMBI-checked costs about 0.53 CPU-s per
frame, of which the x265 encode is roughly 32 %. Replacing the candidate encodes
by VT would cut a search's CPU by up to about 30 %; hardware decode in the metric
passes alone would cut about 3 % (FFV1 reference, the production shape). The final
encode of the winner is where VT matters: 25-45x fewer CPU-seconds and 4-9x
faster. A 2-hour 1080p24 film is about 173,000 frames: roughly 8 CPU-hours / 1 hour
wall with x265 at 46 fps, about 15 minutes with VT at 190 fps (this chip, this
synthetic content; real content makes x265 slower, VT barely).

## 8. Concurrent VT and x265

All jobs start together on the same clip; "pools 5" is the worker's partition for
two parallel jobs on 10 CPUs (`pools=5:frame-threads=3`). Mean of two runs, host
at 73-92 % idle before each.

| Scenario | 1080p fps per job | 2160p fps per job | host CPU |
| --- | --- | --- | --- |
| x265 alone | 46.0 | 6.3 | 78 % / 69 % |
| VT alone | 190 | 54.5 | 11 % / 9 % |
| x265 + VT | 49.5 + 177 | 6.5 + 52.8 | 80 % / 74 % |
| 2 x265 (pools 5) | 24.9 + 24.9 | 3.5 + 3.5 | 84 % / 80 % |
| 2 x265 (pools 5) + VT | 21.8 + 21.5 + 164 | 3.3 + 3.3 + 50.8 | 83 % / 80 % |

- Next to two x265 jobs a VT encode runs at 86-93 % of its solo speed and slows
  each x265 job by 12-14 % (1080p) or 6 % (2160p). Host CPU does not rise because
  x265 was not saturating 10 cores at 1080p anyway.
- Two parallel x265 jobs at 1080p produced 49.6 fps in total against 46.0 for one
  unpartitioned job (+8 %); at 2160p 7.0 against 6.3 (+11 %, within noise). On
  this clip parallelism does not buy much; the second job mostly shares the same
  cores.
- If the VT job's output is the product (not scratch work) the pair is a clear
  throughput gain; as a *background* job it costs the x265 jobs about one tenth of
  their speed.

## 9. Limits of this experiment

- **Synthetic, one family of texture.** Global pans and rotations are easy for
  x265's motion search; real content with complex motion, cuts, film grain and
  flat areas usually widens x265's lead over a hardware encoder. A ratio of 1.0x
  here is a best case for VT until real clips agree. A follow-up with a few
  freely licensed clips would settle it; none was downloaded for this document.
- One 5-8 s clip per class, single encodes, no repeated quality trials (quality
  is deterministic; timings were repeated as stated).
- VMAF v0.6.1, not the production v1.0.16; windows of 2 s, not 5 s.
- This chip has one media engine; the production worker is a different chip.
- 8-bit sources only, no HDR, no 4:2:2, no two-pass or bitrate-ladder rate control.
- No assessment of subjective quality beyond VMAF/CAMBI.

## 10. Proposal for phase 2

1. **Fix the VT candidates, not just add more of them.** Give every VT candidate
   `gop_size: 240` (and optionally `b_frames: 2`). `gop_size` is already a typed
   plan field propagated to candidates (`action/benchmark_template.go`), so no
   worker or schema change is needed. Under the "older policies unchanged" rule
   this belongs in a new policy version; size v3 stays frozen without VT.
2. **A size-mode option "allow hardware" (prefer speed).** When on,
   the search adds VT quality candidates (suggested start ladder q 45/50/55/60:
   the passing values on these clips were 45-57; the repository's current
   65/70/75 are far above the gate and would be wasted). VT qualifies through the
   same gates and the same selection margin as x265. It wins if its projected size
   is within a user-set margin of the x265 winner (for example "up to 15 % larger",
   0-50 %); otherwise x265 wins as today. If the x265 search finds nothing in the
   CRF 22-30 ladder, a passing VT candidate may still win, which is a speed-neutral
   improvement for hard sources like `grain`.
3. **Hardware decode where it paid**: metric passes whose reference is also a
   compressed stream and 2160p HEVC sample extraction, behind a per-worker
   startup bit-identity check (compare framemd5 of a short fixture decoded both
   ways) with automatic software fallback. Not for 1080p H.264 decode, where
   software is faster.
4. **Do not move x265 final encodes to VT by default.** Size is the goal of size
   mode, and the evidence above is synthetic.
5. **Before freezing a margin, validate on 3-4 real freely licensed clips**
   (including a grainy and a dark one) and re-run the quality gates with the
   production VMAF model on the actual worker.

Phase 2 changes candidate selection and should get an Opus review, as agreed.

### Outcome

Phase 2 is implemented as proposed items 1 and 2. One decision differs from the
proposal: the option is **on by default** (with a 15 % margin, always visible
and switchable off per job or batch), only for the new policy, so batches and
digests frozen earlier are unchanged and `allow_hardware=false` reproduces the
earlier snapshot exactly. The winner's encoder is shown as VideoToolbox with a
Hardware badge before anything is replaced. Items 3 to 5 are left as follow-ups
(hardware decode behind a self-check, validation on real freely licensed clips
with the production VMAF model, keeping x265 for final encodes in the size
mode).

### Addendum: first real-content result

The experiment above used synthetic clips, and its own caveat was that the margin
had to be validated on real content with the production VMAF model before being
frozen. The first supervised run on a hard 1080p episode showed why. The
higher-quality fallback rung `-q:v` 70 won the sample search (VMAF p5 87.1, worst
window 89.2 against limits of 84 and 85, so it cleared the gates by the 1.5
selection margin on the window but only barely on p5) and then failed the
whole-file check; its worst frame was 78.7. The sample search sees five short
windows, the final check every frame, and VideoToolbox quality moves in roughly
two-step increments and varies more between scenes than x265 CRF does.

Two changes follow, both conservative:

1. **Wider selection margin for hardware candidates**: +2 VMAF on p5 and worst
   window on top of 1.5, so p5 must reach 87.5 and the worst window 88.5. The
   observed p5 of 87.1 is rejected; a result with a comfortable margin still
   wins. Two points is a single-episode calibration and the smallest step that
   rejects the observed failure, not a fitted value: it should be revisited when
   more real results exist. A worst-frame floor was considered and not added: one
   observation cannot set it, and the whole-file frame-count and p5 checks already
   cover the tail.
2. **The size action continues with x265 when a hardware result still fails the
   whole-file check**, instead of ending with the original kept and the slow x265
   family unmeasured. See `TRANSCODE_AUTOMATIC.md` (*Same-action x265
   continuation*) for the budget, reuse and rollout rules.
