package transcodeworker

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
)

// This file implements worker-local full candidate validation. It runs on the
// worker-local candidate BEFORE any publish/copy to the NAS, so a bad candidate
// never reaches the NAS. After publish, only a lightweight independent
// verification (existence/size/regular-file identity) runs, plus the
// coordinator's own lightweight identity check.
//
// The worker check is the authoritative structural/media/policy validation. It
// probes the LOCAL candidate with the same probe seam used for the source and
// enforces every materially required correctness/policy invariant that used to
// live in the coordinator: video codec, expected bit depth, pixel format,
// video/audio/subtitle/attachment/chapter preservation, per-subtitle action
// codec/disposition, audio copy-mode codec/language/channels, resolution
// preservation, and duration sanity. Any failure fails the job closed and the
// local candidate is removed without publishing. This preserves
// original-preservation, no-clobber, cancellation, recovery, idempotency, and
// promotion separation: validation never mutates the source, the destination,
// or another job's artifacts.

// CandidateAttestation is the accepted local candidate's identity computed
// during pre-publish validation. The size and content digest are cheap
// independent facts the coordinator compares against the published object
// without a full NAS read or media inspection.
type CandidateAttestation struct {
	SizeBytes   int64
	SHA256      string
	DurationSec float64
	VideoCodec  string
	Width       int
	Height      int
	BitDepth    int
	PixelFormat string
}

func verifyCheckpointCandidate(ctx context.Context, job *JobRecord, path string) error {
	if job.Plan != nil {
		if err := transcode.ValidateFinalSize(job.SourceSizeBytes, job.CandidateSizeBytes, job.Plan.SizePolicy); err != nil {
			return err
		}
	}
	if transcode.IsSizeAudioOnlyPlan(job.Plan) {
		e := job.QualityEvidence
		if e == nil || e.Verdict != "pass" || e.CandidateSHA256 != job.CandidateSHA256 || e.CandidateSizeBytes != job.CandidateSizeBytes || e.PlanDigest != job.Plan.PlanDigest || e.VideoCopy.Check() != nil {
			return candidateInvalid(fmt.Errorf("%w: video copy attestation missing or mismatched; never publish", ErrSourceInvalid))
		}
	}
	if job.Plan != nil && job.Plan.QualityValidation != nil {
		e := job.QualityEvidence
		if e == nil || e.Verdict != "pass" || e.CandidateSHA256 != job.CandidateSHA256 || e.CandidateSizeBytes != job.CandidateSizeBytes || e.PlanDigest != job.Plan.PlanDigest || e.BenchmarkRequestDigest != job.Plan.QualityValidation.BenchmarkRequestDigest {
			return candidateInvalid(fmt.Errorf("%w: final perceptual quality attestation missing or mismatched; never publish", ErrSourceInvalid))
		}
	}
	// Pre-attestation worker records remain readable. A partial or missing
	// identity on a modern validated checkpoint is always an error.
	if job.CandidateSHA256 == "" && job.CandidateSizeBytes == 0 && job.ValidationFinishedAt.IsZero() {
		return nil
	}
	if job.CandidateSHA256 == "" || job.CandidateSizeBytes <= 0 {
		return candidateInvalid(fmt.Errorf("%w: validated candidate checkpoint has no complete integrity baseline", ErrSourceInvalid))
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != job.CandidateSizeBytes {
		return candidateInvalid(fmt.Errorf("%w: local candidate no longer matches validated size/type; never publish", ErrSourceInvalid))
	}
	// A fresh runner (restart or scheduled finalization retry) has an empty
	// digest memo and reads the whole candidate; the runner that just
	// validated it reuses that digest while the file's fingerprint is intact.
	sha, err := verifiedLocalSHA256(ctx, path)
	if err != nil {
		return candidateHashFailure(err)
	}
	if sha != job.CandidateSHA256 {
		return candidateInvalid(fmt.Errorf("%w: local candidate SHA-256 changed after validation; never publish", ErrSourceInvalid))
	}
	return nil
}

// validateEncodedCandidateFull performs full structural/media/policy validation
// of the worker-local candidate before the EncodeComplete checkpoint. It fails
// closed on any probe failure, missing video, codec/bit-depth/pixel-format
// mismatch, stream/chapter preservation loss, or duration divergence.
func (w *Worker) validateEncodedCandidateFull(ctx context.Context, localCandidate string, execPlan *ExecutionPlan, srcProbe SourceProbe, candidateDurHint float64) (CandidateAttestation, error) {
	var attest CandidateAttestation
	if err := validateEncodedCandidate(localCandidate); err != nil {
		return attest, err
	}
	if execPlan == nil || execPlan.Plan == nil {
		return attest, candidateInvalid(fmt.Errorf("%w: nil execution plan for candidate validation (fail closed)", ErrSourceInvalid))
	}
	info, err := os.Lstat(localCandidate)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return attest, candidateInvalid(fmt.Errorf("%w: local candidate %s is not a non-empty regular file", ErrSourceInvalid, localCandidate))
	}
	// Bind the probe to an immutable byte baseline. Hashing only after the
	// probe could bless a replacement written while ffprobe was running.
	sha, err := hashLocalFileSHA256(ctx, localCandidate)
	if err != nil {
		return attest, candidateHashFailure(err)
	}
	candProbe, err := w.probeSourceDetailsForJob(ctx, localCandidate)
	if err != nil {
		return attest, fmt.Errorf("probing local candidate for pre-publish validation: %w", err)
	}
	if candidateDurHint > 0 {
		candProbe.DurationSec = candidateDurHint
	}
	srcVideo, candVideo := firstMainVideo(srcProbe.Streams), firstMainVideo(candProbe.Streams)
	if candVideo == nil {
		return attest, checkInvalid("video", fmt.Errorf("%w: local candidate contains no video streams (fail closed, never publish)", ErrContentNotPreserved))
	}
	if strings.TrimSpace(candVideo.Codec) == "" {
		return attest, checkInvalid("video", fmt.Errorf("%w: local candidate video stream has empty codec (fail closed, never publish)", ErrContentNotPreserved))
	}
	// Video count must be preserved (exactly one video stream in practice).
	// Cover art (attached pictures) is counted separately so a cover cannot
	// stand in for programme video or vanish unnoticed.
	if sv, cv := countMainVideo(srcProbe.Streams), countMainVideo(candProbe.Streams); sv > 0 && cv != sv {
		return attest, checkInvalid("video", fmt.Errorf("%w: local candidate video count %d != source %d (fail closed, never publish)", ErrContentNotPreserved, cv, sv))
	}
	// The plan unmaps the source's preview tracks. One left in the candidate
	// means the map did not take: a second video the programme count above
	// does not see.
	if len(execPlan.Thumbnails) > 0 {
		if _, kept := splitProgrammeVideo(candProbe.Streams); len(kept) > 0 {
			return attest, checkInvalid("preview tracks", fmt.Errorf("%w: local candidate still carries %d preview image track(s) the plan leaves out (fail closed, never publish)", ErrContentNotPreserved, len(kept)))
		}
	}
	// A BMP or WebP cover is re-attached as an attachment Matroska does not
	// read back as a picture: it is held to the attachments below instead.
	asAttachment := coversAsAttachments(execPlan)
	if sc, cc := countCoverArt(srcProbe.Streams)-len(asAttachment), countCoverArt(candProbe.Streams); cc != sc {
		return attest, checkInvalid("cover art", fmt.Errorf("%w: local candidate cover art count %d != source %d (fail closed, never publish)", ErrContentNotPreserved, cc, sc))
	}
	// The encode reads stored pictures and carries the source's display matrix
	// as metadata: a lost or changed rotation or flip shows a different
	// picture, whatever the plan measures.
	if err := orientationPreserved(srcVideo, candVideo); err != nil {
		return attest, checkInvalid("video", err)
	}
	// Codec: the candidate must actually be the requested target codec.
	if expected := expectedWorkerVideoCodec(execPlan.Plan.VideoCodec); expected != "" {
		if got := norm(candVideo.Codec); !codecMatches(expected, got) {
			return attest, checkInvalid("video", fmt.Errorf("%w: local candidate video codec %q does not match expected %q (fail closed, never publish)", ErrContentNotPreserved, candVideo.Codec, expected))
		}
	}
	// Expected bit depth is a hard invariant.
	if execPlan.Plan.ExpectedBitDepth > 0 && candVideo.BitDepth > 0 && candVideo.BitDepth != execPlan.Plan.ExpectedBitDepth {
		return attest, checkInvalid("video", fmt.Errorf("%w: local candidate bit depth %d != expected %d (fail closed, never publish)", ErrContentNotPreserved, candVideo.BitDepth, execPlan.Plan.ExpectedBitDepth))
	}
	if err := pixelFormatCompatible(execPlan.Plan.PixelFormat, candVideo.PixelFormat, execPlan.Plan.ExpectedBitDepth); err != nil {
		return attest, failedCheck("video", err)
	}
	// Compact plans resize to their frozen target; everything else preserves
	// the source resolution.
	if c := execPlan.Plan.Compact; c != nil {
		if candVideo.Width != c.TargetWidth || candVideo.Height != c.TargetHeight || srcVideo == nil || srcVideo.Width != c.SourceWidth || srcVideo.Height != c.SourceHeight {
			return attest, checkInvalid("video", fmt.Errorf("%w: local candidate resolution %dx%d != compact target %dx%d (fail closed, never publish)", ErrContentNotPreserved, candVideo.Width, candVideo.Height, c.TargetWidth, c.TargetHeight))
		}
	} else if srcVideo != nil && srcVideo.Width > 0 && srcVideo.Height > 0 && candVideo.Width > 0 && candVideo.Height > 0 {
		if srcVideo.Width != candVideo.Width || srcVideo.Height != candVideo.Height {
			return attest, checkInvalid("video", fmt.Errorf("%w: local candidate resolution %dx%d != source %dx%d (fail closed, never publish)", ErrContentNotPreserved, candVideo.Width, candVideo.Height, srcVideo.Width, srcVideo.Height))
		}
	}
	if err := validateAudioPreservation(execPlan.Plan, srcProbe.Streams, candProbe.Streams); err != nil {
		return attest, failedCheck("audio tracks", err)
	}
	if err := validateSubtitlePreservation(execPlan.Plan, srcProbe.Streams, candProbe.Streams); err != nil {
		return attest, failedCheck("subtitle tracks", err)
	}
	// The candidate's packets are read in one pass for every kind the
	// source's were measured for before the encode; the source is not read
	// again. Every kind the source's scan reads must have been measured: a
	// check never passes for want of a measure.
	want := w.packetScanFor(execPlan.Plan, srcProbe)
	if err := sourceMeasured(want, srcProbe); err != nil {
		return attest, failedCheck("source packets", err)
	}
	if req := candidateScanFor(want, candProbe.Streams, candVideo); req.any() {
		candScan, err := w.packetsForJob(ctx, localCandidate, req)
		if err != nil {
			return attest, fmt.Errorf("candidate packet validation: %w", err)
		}
		if req.audio != nil {
			if err := validateAudioPackets(execPlan.Plan, srcProbe, candProbe, nonNil(candScan.audio)); err != nil {
				return attest, failedCheck("audio packets", err)
			}
		}
		if req.subtitles != nil {
			if err := validateSubtitlePackets(execPlan.Plan, srcProbe, candProbe, nonNil(candScan.subtitles)); err != nil {
				return attest, failedCheck("subtitle events", err)
			}
		}
		// The container's duration is its longest stream's: only the
		// video's own frames show a video that stops early.
		if req.video >= 0 {
			if err := validateVideoCompleteness(execPlan.Plan, srcProbe, candScan.video); err != nil {
				return attest, failedCheck("video completeness", err)
			}
		}
	}
	// Attachment preservation policy: the same files, not just as many.
	if err := attachmentsWithCovers(srcProbe.Streams, candProbe.Streams, asAttachment, execPlan.Plan.PreserveAttachments); err != nil {
		return attest, failedCheck("attachments", err)
	}
	// Chapter preservation policy (only when the probe could count chapters).
	if execPlan.Plan.PreserveChapters && srcProbe.Chapters > 0 && candProbe.Chapters != srcProbe.Chapters {
		return attest, checkInvalid("chapters", fmt.Errorf("%w: local candidate chapter count %d != source %d (fail closed, never publish)", ErrContentNotPreserved, candProbe.Chapters, srcProbe.Chapters))
	}
	// Duration sanity mirrors the historical coordinator tolerance (3s absolute
	// and 2% relative). A diverged duration is never published.
	if srcProbe.DurationSec > 0 && candProbe.DurationSec > 0 {
		d := math.Abs(candProbe.DurationSec - srcProbe.DurationSec)
		if d > 3 && (d/srcProbe.DurationSec) > 0.02 {
			return attest, checkInvalid("duration", fmt.Errorf("%w: local candidate duration %.1fs diverges from source %.1fs (diff %.1fs, fail closed, never publish)", ErrContentNotPreserved, candProbe.DurationSec, srcProbe.DurationSec, d))
		}
	}
	// Within a job run this is a strict stat-fingerprint check against the
	// baseline above (same inode, size and modification time), not a second
	// read; without a job run's digest memo it reads the file again.
	verifiedSHA, err := verifiedLocalSHA256(ctx, localCandidate)
	if err != nil {
		return attest, candidateHashFailure(err)
	}
	if verifiedSHA != sha {
		return attest, candidateInvalid(fmt.Errorf("%w: local candidate changed during validation; never publish", ErrContentNotPreserved))
	}
	attest = CandidateAttestation{
		SizeBytes:   info.Size(),
		SHA256:      sha,
		DurationSec: candProbe.DurationSec,
		VideoCodec:  candVideo.Codec,
		Width:       candVideo.Width,
		Height:      candVideo.Height,
		BitDepth:    candVideo.BitDepth,
		PixelFormat: candVideo.PixelFormat,
	}
	return attest, nil
}

// orientationPreserved refuses a candidate whose display rotation or flip
// differs from the source's. No matrix and an identity matrix agree.
func orientationPreserved(src, cand *SourceStream) error {
	if src == nil || cand == nil {
		return nil
	}
	if a, b := src.Orientation(), cand.Orientation(); a != b {
		return fmt.Errorf("%w: quality_final_orientation_changed: the source shows %+v, the local candidate %+v (fail closed, never publish)", ErrContentNotPreserved, a, b)
	}
	return nil
}

func firstStreamOfKind(streams []SourceStream, kind string) *SourceStream {
	for i := range streams {
		if strings.EqualFold(strings.TrimSpace(streams[i].Kind), kind) {
			return &streams[i]
		}
	}
	return nil
}

// firstMainVideo returns the first programme video stream, skipping cover art
// and preview-image tracks.
func firstMainVideo(streams []SourceStream) *SourceStream {
	programme, _ := splitProgrammeVideo(streams)
	if len(programme) == 0 {
		return nil
	}
	return &programme[0]
}

func countMainVideo(streams []SourceStream) int {
	programme, _ := splitProgrammeVideo(streams)
	return len(programme)
}

func countCoverArt(streams []SourceStream) int {
	n := 0
	for _, s := range streams {
		if s.IsAttachedPicture() {
			n++
		}
	}
	return n
}

func countStreams(streams []SourceStream, kind string) int {
	n := 0
	for _, s := range streams {
		if strings.EqualFold(strings.TrimSpace(s.Kind), kind) {
			n++
		}
	}
	return n
}

// expectedWorkerVideoCodec maps an encoder/plan name to the probe codec family
// the container is expected to report.
func expectedWorkerVideoCodec(codec string) string {
	switch norm(codec) {
	case "hevc_videotoolbox", "libx265", "hevc", "h265", "x265":
		return "hevc"
	case "h264_videotoolbox", "h264", "avc", "avc1":
		return "h264"
	case "copy":
		return ""
	default:
		return norm(codec)
	}
}

// copiedCodecMatches holds a copied track's codec, as the probe names it, to
// the source track's (transcode.SameCopiedCodec, which the coordinator's
// check uses too).
func copiedCodecMatches(source, got string) bool {
	return transcode.SameCopiedCodec(source, got)
}

// encodedCodecNames maps the encoder or plan names a converted track is asked
// for to the codec the probe reports for its output.
var encodedCodecNames = map[string]string{
	"hevc_videotoolbox": "hevc", "libx265": "hevc", "x265": "hevc", "h265": "hevc",
	"h264_videotoolbox": "h264", "libx264": "h264", "x264": "h264", "avc": "h264", "avc1": "h264",
	"aac_at": "aac", "libfdk_aac": "aac",
	"libopus": "opus",
}

// codecMatches holds a converted track's codec, as the probe names it, to the
// encoder or plan name it was asked for (libx265 → hevc, aac_at → aac,
// libopus → opus). A name with no mapping keeps the comparison it always had:
// either name contains the other.
func codecMatches(expected, got string) bool {
	expected, got = norm(expected), norm(got)
	if expected == "" {
		return true
	}
	if got == "" {
		return false
	}
	if probe, ok := encodedCodecNames[expected]; ok {
		return got == probe
	}
	return strings.Contains(got, expected) || strings.Contains(expected, got)
}

// pixelFormatCompatible verifies the candidate's pixel format is compatible
// with the plan's requested format. Bit-depth-conflicting or wrong chroma
// families fail closed; an unknown probe value is skipped (structural ffprobe
// normally reports it).
func pixelFormatCompatible(expected, actual string, expectedBitDepth int) error {
	exp, act := norm(expected), norm(actual)
	if exp == "" || act == "" {
		return nil
	}
	if exp == act {
		return nil
	}
	families := map[string]string{
		"p010le": "10", "yuv420p10le": "10",
		"p012le": "12", "yuv420p12le": "12",
		"nv12": "8", "yuv420p": "8", "yuvj420p": "8",
	}
	fe, eok := families[exp]
	fa, aok := families[act]
	if eok && aok && fe == fa {
		return nil
	}
	return candidateInvalid(fmt.Errorf("%w: local candidate pixel format %q is incompatible with requested %q (fail closed, never publish)", ErrContentNotPreserved, actual, expected))
}

func validateAudioPreservation(plan *transcode.Plan, src, cand []SourceStream) error {
	srcAudio, candAudio := plannedStreams(plan, src, "audio"), streamsOfKind(cand, "audio")
	if len(candAudio) != len(srcAudio) {
		return candidateInvalid(fmt.Errorf("%w: local candidate audio count %d != %s %d (fail closed, never publish)", ErrContentNotPreserved, len(candAudio), plannedLabel(plan), len(srcAudio)))
	}
	planned, written := plannedDispositions(srcAudio), dispositionsWritten(plan, src)
	for i := range srcAudio {
		a, b := srcAudio[i], candAudio[i]
		out := transcode.AudioOutputFor(plan.AudioMode, a.AudioTrack())
		target := out.Codec
		if out.HEAAC && !strings.EqualFold(b.Profile, "HE-AAC") {
			return fmt.Errorf("HE-AAC audio profile not present in candidate stream %d", i)
		}
		// A size level's AAC-LC track is checked too: the profile is half of
		// what tells a level 1 track from a level 2 one.
		if transcode.IsAudioLevelMode(plan.AudioMode) && target != "copy" && !out.HEAAC && !strings.EqualFold(b.Profile, "LC") {
			return candidateInvalid(fmt.Errorf("%w: local candidate audio stream %d is %q, expected AAC-LC (fail closed, never publish)", ErrContentNotPreserved, i, b.Profile))
		}
		// Only an AAC conversion can remap the layout. A copied track keeps
		// its bitstream, and Matroska reports no layout for PCM, so a copy
		// from MOV/MP4/AVI legitimately comes back as "".
		checkLayout := target != "copy"
		matches := codecMatches(target, b.Codec)
		if target == "copy" {
			target, matches = norm(a.Codec), copiedCodecMatches(a.Codec, b.Codec)
		}
		if !matches {
			return candidateInvalid(fmt.Errorf("%w: local candidate audio codec %q at stream %d != expected %q (fail closed, never publish)", ErrContentNotPreserved, b.Codec, i, target))
		}
		if !transcode.SameLanguage(a.Language, b.Language) {
			return candidateInvalid(fmt.Errorf("%w: local candidate audio language %q at stream %d != source %q (fail closed, never publish)", ErrContentNotPreserved, b.Language, i, a.Language))
		}
		if a.Channels > 0 && a.Channels != b.Channels {
			return candidateInvalid(fmt.Errorf("%w: local candidate audio channels %d at stream %d != source %d (fail closed, never publish)", ErrContentNotPreserved, b.Channels, i, a.Channels))
		}
		if checkLayout && !transcode.SameAudioLayout(a.Layout, b.Layout) {
			return candidateInvalid(fmt.Errorf("%w: local candidate audio layout %q at stream %d != source %q (fail closed, never publish)", ErrContentNotPreserved, b.Layout, i, a.Layout))
		}
		if d := transcode.DispositionChange(a.Disposition, b.Disposition, transcode.InferredDefault(i, planned, written)); d != "" {
			return checkInvalid("audio disposition", fmt.Errorf("%w: audio disposition %s changed at stream %d (fail closed, never publish)", ErrContentNotPreserved, d, i))
		}
		// A copied track keeps its name; a converted one carries the name
		// the conversion gives it (transcode.ConvertedAudioTitle).
		want := a.Title
		if out.Codec != "copy" {
			want = transcode.ConvertedAudioTitle(a.Title, a.Layout, a.Channels)
		}
		if b.Title != want {
			return checkInvalid("audio title", fmt.Errorf("%w: audio stream %d is named %q, the plan names it %q (fail closed, never publish)", ErrContentNotPreserved, i, b.Title, want))
		}
	}
	return nil
}

func validateSubtitlePreservation(plan *transcode.Plan, src, cand []SourceStream) error {
	srcSubs, candSubs := plannedStreams(plan, src, "subtitle"), streamsOfKind(cand, "subtitle")
	if len(candSubs) != len(srcSubs) {
		return candidateInvalid(fmt.Errorf("%w: local candidate subtitle count %d != %s %d (fail closed, never publish)", ErrContentNotPreserved, len(candSubs), plannedLabel(plan), len(srcSubs)))
	}
	planned, written := plannedDispositions(srcSubs), dispositionsWritten(plan, src)
	for i := range srcSubs {
		a, b := srcSubs[i], candSubs[i]
		act, ok := subtitleActionFor(plan, a)
		if !ok {
			// The build step rejects a subtitle without an action; a
			// candidate that has one anyway was not made by this plan.
			return candidateInvalid(fmt.Errorf("%w: subtitle stream %d has no planned action (fail closed, never publish)", ErrContentNotPreserved, i))
		}
		switch norm(act.Operation) {
		case "copy":
			if !copiedCodecMatches(a.Codec, b.Codec) {
				return candidateInvalid(fmt.Errorf("%w: copied subtitle codec changed at stream %d: source=%s candidate=%s (fail closed, never publish)", ErrContentNotPreserved, i, a.Codec, b.Codec))
			}
		case "transcode":
			if !transcode.SameConvertedSubtitleCodec(act.Codec, b.Codec) {
				return candidateInvalid(fmt.Errorf("%w: converted subtitle codec mismatch at stream %d: expected=%s candidate=%s (fail closed, never publish)", ErrContentNotPreserved, i, act.Codec, b.Codec))
			}
		default:
			return candidateInvalid(fmt.Errorf("%w: subtitle stream %d has an unknown planned action %q (fail closed, never publish)", ErrContentNotPreserved, i, act.Operation))
		}
		// A language the source names is kept; "und" is the absence of one,
		// which the muxer writes as no tag.
		if !transcode.SameLanguage(a.Language, b.Language) {
			return candidateInvalid(fmt.Errorf("%w: subtitle language mismatch at stream %d: source=%q candidate=%q (fail closed, never publish)", ErrContentNotPreserved, i, a.Language, b.Language))
		}
		// ffmpeg carries a subtitle's flags and name over whether it copies
		// or converts it, and the command names no subtitle: every flag the
		// output stores, and the name, are the plan's.
		if d := transcode.DispositionChange(a.Disposition, b.Disposition, transcode.InferredDefault(i, planned, written)); d != "" {
			return checkInvalid("subtitle disposition", fmt.Errorf("%w: subtitle disposition %s changed at stream %d (fail closed, never publish)", ErrContentNotPreserved, d, i))
		}
		if a.Title != b.Title {
			return checkInvalid("subtitle title", fmt.Errorf("%w: subtitle stream %d is named %q, the source track %q (fail closed, never publish)", ErrContentNotPreserved, i, b.Title, a.Title))
		}
	}
	return nil
}

// plannedStreams are the source's streams of a kind the output must carry, in
// source order: every one, or, when the plan keeps only some tracks
// (Plan.Tracks), exactly those, each with the disposition the output must
// have (the default moves to the first kept track when the source's was
// dropped). The candidate's streams of the kind are checked against them
// one by one, so an output with a track the plan dropped, or without one it
// kept, fails on the count or on the first pair that differs.
func plannedStreams(plan *transcode.Plan, src []SourceStream, kind string) []SourceStream {
	all := streamsOfKind(src, kind)
	if plan == nil || plan.Tracks == nil {
		return all
	}
	keptAudio, keptSubtitles, _, _ := plan.Tracks.Partition(trackInfos(src))
	kept := keptAudio
	if kind == "subtitle" {
		kept = keptSubtitles
	}
	dispositions := map[int]map[string]int{}
	for _, t := range kept {
		dispositions[t.Index] = t.Disposition
	}
	out := make([]SourceStream, 0, len(kept))
	for _, s := range all {
		if d, ok := dispositions[s.Index]; ok {
			s.Disposition = d
			out = append(out, s)
		}
	}
	return out
}

// plannedDispositions are the flags the output's tracks of a kind must carry,
// in output order.
func plannedDispositions(planned []SourceStream) []map[string]int {
	out := make([]map[string]int, len(planned))
	for i, s := range planned {
		out[i] = s.Disposition
	}
	return out
}

// dispositionsWritten reports whether the worker's command set any output
// track's flags, which stops ffmpeg's own default on every track
// (transcode.InferredDefault). It reads the plan the way the command was
// built from it (transcode.WrittenDisposition).
func dispositionsWritten(plan *transcode.Plan, src []SourceStream) bool {
	source := map[int]map[string]int{}
	for _, s := range src {
		source[s.Index] = s.Disposition
	}
	for _, kind := range []string{"audio", "subtitle"} {
		for _, s := range plannedStreams(plan, src, kind) {
			if transcode.WrittenDisposition(kind, s.Disposition, source[s.Index]) != nil {
				return true
			}
		}
	}
	return false
}

// plannedLabel names what the candidate's track counts are compared with.
func plannedLabel(plan *transcode.Plan) string {
	if plan != nil && plan.Tracks != nil {
		return "planned"
	}
	return "source"
}

func streamsOfKind(streams []SourceStream, kind string) []SourceStream {
	out := make([]SourceStream, 0, len(streams))
	for _, s := range streams {
		if strings.EqualFold(strings.TrimSpace(s.Kind), kind) {
			out = append(out, s)
		}
	}
	return out
}

// verifyPublishedCandidate performs the lightweight independent post-publish
// verification: the destination must exist as a non-empty regular file whose
// size exactly matches the accepted local candidate. It performs only stats,
// never full content reads or full media probes, so it stays cheap after the
// network copy. Any mismatch fails closed without marking the job complete.
func verifyPublishedCandidate(ctx context.Context, localCandidate, destination string) error {
	if strings.TrimSpace(localCandidate) == "" || strings.TrimSpace(destination) == "" {
		return fmt.Errorf("%w: empty candidate/destination for post-publish verification", ErrDestinationInvalid)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	localInfo, err := os.Lstat(localCandidate)
	if err != nil {
		return fmt.Errorf("%w: statting local candidate %s: %v", ErrStorageIO, localCandidate, err)
	}
	if !localInfo.Mode().IsRegular() || localInfo.Size() == 0 {
		return candidateInvalid(fmt.Errorf("%w: local candidate %s is not a non-empty regular file", ErrSourceInvalid, localCandidate))
	}
	destInfo, err := os.Lstat(destination)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: published destination %s is missing after publish (fail closed)", ErrStorageIO, destination)
		}
		return fmt.Errorf("%w: statting published destination %s: %v", ErrStorageIO, destination, err)
	}
	if !destInfo.Mode().IsRegular() {
		return fmt.Errorf("%w: published destination %s is not a regular file (fail closed)", ErrStorageIO, destination)
	}
	if destInfo.Size() == 0 {
		return fmt.Errorf("%w: published destination %s is empty (fail closed)", ErrStorageIO, destination)
	}
	if destInfo.Size() != localInfo.Size() {
		return fmt.Errorf("%w: published destination size %d != accepted candidate size %d (fail closed)", ErrSizeMismatch, destInfo.Size(), localInfo.Size())
	}
	return nil
}
