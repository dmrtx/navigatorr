package transcodeworker

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
)

func BuildVideoEncoderArgs(plan *transcode.Plan) ([]string, error) {
	if plan == nil {
		return nil, fmt.Errorf("video plan cannot be nil (fail closed)")
	}
	if err := plan.X265.Validate(plan.VideoCodec, plan.Quality); err != nil {
		return nil, err
	}
	var args []string
	var err error
	switch norm(plan.VideoCodec) {
	case transcode.VideoCodecCopy:
		// Only the size v3 audio-only shape copies video; any encoder knob
		// alongside it is a malformed plan.
		if err := transcode.ValidateSizeAudioOnlyShape(plan); err != nil {
			return nil, err
		}
		return []string{"-c:v", "copy"}, nil
	case "hevc_videotoolbox":
		args, err = buildVideoToolboxArgs(plan)
	case "libx265":
		args, err = buildLibX265Args(plan)
	default:
		return nil, fmt.Errorf("unsupported video codec %q (fail closed)", plan.VideoCodec)
	}
	if err != nil || plan.Compact == nil {
		return args, err
	}
	compact, err := compactEncoderArgs(plan.Compact, plan.VideoCodec)
	if err != nil {
		return nil, err
	}
	return append(args, compact...), nil
}

// buildVideoToolboxArgs builds hevc_videotoolbox args. Rate control occupies a
// single slot: -q:v (higher = higher quality) for quality mode, -b:v
// (AverageBitRate) for average-bitrate mode. The modes are mutually exclusive.
func buildVideoToolboxArgs(plan *transcode.Plan) ([]string, error) {
	if norm(plan.Preset) != "" {
		return nil, fmt.Errorf("preset is only supported for libx265, got %q for hevc_videotoolbox", plan.Preset)
	}
	if norm(plan.Tune) != "" {
		return nil, fmt.Errorf("tune is only supported for libx265, got %q for hevc_videotoolbox", plan.Tune)
	}
	profile, pixelFormat, err := validateHEVCVideoShape(plan)
	if err != nil {
		return nil, err
	}
	if err := validateVideoToolboxRateControl(plan); err != nil {
		return nil, err
	}
	args := []string{"-c:v", "hevc_videotoolbox"}
	if plan.AverageBitrateKbps > 0 {
		args = append(args, "-b:v", strconv.Itoa(plan.AverageBitrateKbps)+"k")
	} else {
		quality := plan.Quality
		if quality <= 0 {
			quality = 65
		}
		args = append(args, "-q:v", strconv.Itoa(quality))
	}
	if profile != "" {
		args = append(args, "-profile:v", profile)
	}
	if pixelFormat != "" {
		args = append(args, "-pix_fmt", pixelFormat)
	}
	if plan.PrioritizeSpeed != nil {
		args = append(args, "-prio_speed", boolFFmpeg(*plan.PrioritizeSpeed))
	}
	if plan.SpatialAQ != nil {
		args = append(args, "-spatial_aq", boolFFmpeg(*plan.SpatialAQ))
	}
	if plan.Realtime != nil {
		args = append(args, "-realtime", boolFFmpeg(*plan.Realtime))
	}
	// Bounded offline-quality knobs, in fixed deterministic order. Nil means
	// "emit nothing"; explicit values (including false/zero where applicable,
	// e.g. b_frames 0, closed_gop false) are emitted verbatim.
	if plan.QMin != nil {
		args = append(args, "-qmin", strconv.Itoa(*plan.QMin))
	}
	if plan.QMax != nil {
		args = append(args, "-qmax", strconv.Itoa(*plan.QMax))
	}
	if plan.GOPSize != nil {
		args = append(args, "-g", strconv.Itoa(*plan.GOPSize))
	}
	if plan.BFrames != nil {
		args = append(args, "-bf", strconv.Itoa(*plan.BFrames))
	}
	if plan.ClosedGOP != nil {
		args = append(args, "-flags", boolCGOP(*plan.ClosedGOP))
	}
	if plan.PowerEfficient != nil {
		args = append(args, "-power_efficient", boolFFmpeg(*plan.PowerEfficient))
	}
	if plan.MaxRefFrames != nil {
		args = append(args, "-max_ref_frames", strconv.Itoa(*plan.MaxRefFrames))
	}
	if plan.ConstantBitrate != nil {
		args = append(args, "-constant_bit_rate", boolFFmpeg(*plan.ConstantBitrate))
	}
	if plan.MaxBitrateKbps > 0 {
		args = append(args, "-maxrate", strconv.Itoa(plan.MaxBitrateKbps)+"k")
	}
	return args, nil
}

// validateVideoToolboxRateControl enforces the bounded typed rate-control
// model at argv build time (defense in depth behind recipe validation):
// quality and average bitrate are mutually exclusive, CBR and maxrate require
// an average bitrate, and maxrate must cap at or above the average. bufsize
// is never emitted: the current FFmpeg VideoToolbox encoder does not consume
// it meaningfully.
func validateVideoToolboxRateControl(plan *transcode.Plan) error {
	hasQuality := plan.Quality != 0
	hasBitrate := plan.AverageBitrateKbps != 0
	if hasQuality && hasBitrate {
		return fmt.Errorf("quality (%d) and average_bitrate_kbps (%d) are mutually exclusive (fail closed)", plan.Quality, plan.AverageBitrateKbps)
	}
	if !hasBitrate {
		if plan.Quality < 0 || plan.Quality > 100 {
			return fmt.Errorf("invalid quality %d (must be 1-100)", plan.Quality)
		}
		// Quality 0 selects the legacy default (65) at build time.
	} else if plan.AverageBitrateKbps < 1 || plan.AverageBitrateKbps > transcode.MaxVideoBitrateKbps {
		return fmt.Errorf("invalid average_bitrate_kbps %d (must be 1-%d)", plan.AverageBitrateKbps, transcode.MaxVideoBitrateKbps)
	}
	if plan.MaxBitrateKbps != 0 {
		if !hasBitrate {
			return fmt.Errorf("max_bitrate_kbps requires average_bitrate_kbps (fail closed)")
		}
		if plan.MaxBitrateKbps < 1 || plan.MaxBitrateKbps > transcode.MaxVideoBitrateKbps {
			return fmt.Errorf("invalid max_bitrate_kbps %d (must be 1-%d)", plan.MaxBitrateKbps, transcode.MaxVideoBitrateKbps)
		}
		if plan.MaxBitrateKbps < plan.AverageBitrateKbps {
			return fmt.Errorf("max_bitrate_kbps (%d) must be >= average_bitrate_kbps (%d)", plan.MaxBitrateKbps, plan.AverageBitrateKbps)
		}
	}
	if plan.ConstantBitrate != nil && *plan.ConstantBitrate && !hasBitrate {
		return fmt.Errorf("constant_bitrate requires average_bitrate_kbps (fail closed)")
	}
	if err := validateIntArg("qmin", plan.QMin, 0, transcode.MaxQPBound); err != nil {
		return err
	}
	if err := validateIntArg("qmax", plan.QMax, 0, transcode.MaxQPBound); err != nil {
		return err
	}
	if plan.QMin != nil && plan.QMax != nil && *plan.QMin > *plan.QMax {
		return fmt.Errorf("qmin (%d) must be <= qmax (%d)", *plan.QMin, *plan.QMax)
	}
	if err := validateIntArg("gop_size", plan.GOPSize, 1, transcode.MaxBenchmarkGOPSize); err != nil {
		return err
	}
	if err := validateIntArg("b_frames", plan.BFrames, 0, transcode.MaxBenchmarkBFrames); err != nil {
		return err
	}
	if err := validateIntArg("max_ref_frames", plan.MaxRefFrames, 1, transcode.MaxBenchmarkRefFrames); err != nil {
		return err
	}
	return nil
}

func validateIntArg(knob string, v *int, min, max int) error {
	if v == nil {
		return nil
	}
	if *v < min || *v > max {
		return fmt.Errorf("invalid %s %d (must be %d-%d)", knob, *v, min, max)
	}
	return nil
}

func boolCGOP(v bool) string {
	if v {
		return "+cgop"
	}
	return "-cgop"
}

// buildLibX265Args builds software libx265 args. For libx265, plan.Quality is
// interpreted as CRF where a LOWER value means HIGHER quality (valid 1..51).
// The preset is restricted to a fixed safe enum (no raw x265 params accepted).
func buildLibX265Args(plan *transcode.Plan) ([]string, error) {
	if plan.PrioritizeSpeed != nil || plan.SpatialAQ != nil || plan.Realtime != nil {
		return nil, fmt.Errorf("VideoToolbox-only options (prio_speed/spatial_aq/realtime) are not supported for libx265 (fail closed)")
	}
	if plan.AverageBitrateKbps != 0 || plan.MaxBitrateKbps != 0 || plan.ConstantBitrate != nil {
		return nil, fmt.Errorf("VideoToolbox-only rate control (average_bitrate/max_bitrate/constant_bitrate) is not supported for libx265: use CRF quality and preset (fail closed)")
	}
	if plan.QMin != nil || plan.QMax != nil || plan.GOPSize != nil || plan.BFrames != nil || plan.ClosedGOP != nil || plan.PowerEfficient != nil || plan.MaxRefFrames != nil {
		return nil, fmt.Errorf("VideoToolbox-only offline options (qmin/qmax/gop_size/b_frames/closed_gop/power_efficient/max_ref_frames) are not supported for libx265 (fail closed)")
	}
	profile, pixelFormat, err := validateHEVCVideoShape(plan)
	if err != nil {
		return nil, err
	}
	crf := plan.Quality
	if plan.X265 == nil && (crf < transcode.LibX265CRFMin || crf > transcode.LibX265CRFMax) {
		return nil, fmt.Errorf("invalid libx265 crf %d (must be %d-%d)", crf, transcode.LibX265CRFMin, transcode.LibX265CRFMax)
	}
	preset := norm(plan.Preset)
	if preset == "" {
		preset = defaultLibX265Preset
	}
	if !transcode.IsValidLibX265Preset(preset) {
		return nil, fmt.Errorf("unsupported libx265 preset %q (allowed: %v)", plan.Preset, transcode.ValidLibX265Presets())
	}
	tune := norm(plan.Tune)
	if tune != "" && !transcode.IsValidLibX265Tune(tune) {
		return nil, fmt.Errorf("unsupported libx265 tune %q (allowed: %v)", plan.Tune, transcode.ValidLibX265Tunes())
	}
	if pixelFormat == "p010le" {
		pixelFormat = "yuv420p10le"
	}
	args := []string{"-c:v", "libx265", "-preset", preset}
	if plan.X265 == nil {
		args = []string{"-c:v", "libx265", "-crf", strconv.Itoa(crf), "-preset", preset}
	} else {
		x := plan.X265
		params := fmt.Sprintf("rc-lookahead=%d:bframes=%d:ref=%d:aq-mode=%d:aq-strength=%g:psy-rd=%g:psy-rdoq=%g:ssim-rd=%s:rdoq-level=2", x.Lookahead, x.BFrames, x.RefFrames, x.AQMode, x.AQStrength, x.PsyRD, x.PsyRDOQ, boolFFmpeg(x.SSIMRD))
		args = append(args, "-b:v", strconv.Itoa(x.BitrateKbps)+"k", "-x265-params", params)
	}
	if tune != "" {
		args = append(args, "-tune", tune)
	}
	if profile != "" {
		args = append(args, "-profile:v", profile)
	}
	if pixelFormat != "" {
		args = append(args, "-pix_fmt", pixelFormat)
	}
	return args, nil
}

const defaultLibX265Preset = "medium"

// validateHEVCVideoShape validates the HEVC profile / pixel format / bit depth
// constraints shared by hevc_videotoolbox and libx265.
func validateHEVCVideoShape(plan *transcode.Plan) (profile, pixelFormat string, err error) {
	profile = norm(plan.VideoProfile)
	pixelFormat = norm(plan.PixelFormat)
	if profile != "" && profile != "main" && profile != "main10" {
		return "", "", fmt.Errorf("unsupported HEVC profile %q", plan.VideoProfile)
	}
	if pixelFormat != "" && pixelFormat != "yuv420p" && pixelFormat != "p010le" {
		return "", "", fmt.Errorf("unsupported pixel format %q", plan.PixelFormat)
	}
	if profile == "main10" && pixelFormat != "p010le" {
		return "", "", fmt.Errorf("HEVC main10 requires pixel format p010le")
	}
	if pixelFormat == "p010le" && profile != "main10" {
		return "", "", fmt.Errorf("pixel format p010le requires HEVC main10")
	}
	if plan.ExpectedBitDepth != 0 && plan.ExpectedBitDepth != 8 && plan.ExpectedBitDepth != 10 {
		return "", "", fmt.Errorf("unsupported expected bit depth %d", plan.ExpectedBitDepth)
	}
	if plan.ExpectedBitDepth == 10 && (profile != "main10" || pixelFormat != "p010le") {
		return "", "", fmt.Errorf("expected 10-bit output requires main10 + p010le")
	}
	if profile == "main10" && plan.ExpectedBitDepth != 10 {
		return "", "", fmt.Errorf("main10 plan must require expected bit depth 10")
	}
	return profile, pixelFormat, nil
}

func boolFFmpeg(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// statisticsTags are the per-track muxer statistics (mkvmerge writes them)
// that describe the source bitstream, not a re-encoded one.
var statisticsTags = []string{"BPS", "NUMBER_OF_BYTES", "NUMBER_OF_FRAMES", "DURATION", "_STATISTICS_WRITING_APP", "_STATISTICS_WRITING_DATE_UTC", "_STATISTICS_TAGS"}

func clearStatisticsTags(kind string, typeIndex int) []string {
	args := make([]string, 0, 2*len(statisticsTags))
	for _, tag := range statisticsTags {
		args = append(args, fmt.Sprintf("-metadata:s:%s:%d", kind, typeIndex), tag+"=")
	}
	return args
}

// sourceFormatWords mark a track title that describes the source format,
// as whole words ("Atmosphere" names no format).
var sourceFormatWords = regexp.MustCompile(`(?i)\b(truehd|atmos|dts|flac|l?pcm|alac|mlp|lossless|master\s+audio|hd[\s-]?ma|dolby|uncompressed|hra)\b`)

// sourceFormatPhrases and sourceFormatTokens are the format, sample and
// channel words removed from a converted track's title; whatever else the
// title says ("Director's Commentary") is kept.
var (
	sourceFormatPhrases = regexp.MustCompile(`(?i)\bdts[-:\s]?(hd|x|es)\b([\s-]+(master\s+audio|ma|high\s+resolution(\s+audio)?|hra)\b)?|\bmaster\s+audio\b|\bhd[\s-]?ma\b|\bhigh\s+resolution(\s+audio)?\b|\bdolby\s+digital(\s+plus)?\b`)
	sourceFormatTokens  = regexp.MustCompile(`(?i)^(truehd|atmos|dts|flac|l?pcm|alac|mlp|lossless|dolby|uncompressed|hra|stereo|mono|surround|\d+ch|\d(\.\d){1,2}(ch)?|\d+-?bits?|\d+(\.\d+)?k?hz|\d+-?bits?/\d+(\.\d+)?(k?hz)?|\d+/\d+(\.\d+)?(k?hz)?)$`)
)

// convertedAudioTitle retitles a track converted to AAC when its title names
// the format it no longer has: the format and channel words are replaced by
// the AAC layout ("TrueHD Atmos 7.1" becomes "AAC 7.1", "Director's
// Commentary DTS-HD MA 5.1" becomes "Director's Commentary AAC 5.1"). Titles
// that name no source format ("Commentary") are kept; ok is false then.
// Without a reported layout (PCM in Matroska) the channel count names it.
func convertedAudioTitle(title, layout string, channels int) (string, bool) {
	if !sourceFormatWords.MatchString(title) {
		return "", false
	}
	l := strings.ToLower(strings.TrimSpace(layout))
	if i := strings.IndexByte(l, '('); i > 0 {
		l = l[:i]
	}
	if l == "" {
		l = map[int]string{1: "mono", 2: "stereo", 6: "5.1", 8: "7.1"}[channels]
	}
	format := "AAC " + l
	switch l {
	case "mono":
		format = "AAC Mono"
	case "stereo":
		format = "AAC Stereo"
	case "":
		format = "AAC"
	}
	var label []string
	for _, word := range strings.Fields(sourceFormatPhrases.ReplaceAllString(title, " ")) {
		core := strings.Trim(word, "()[]{},;:|/+-·")
		if core == "" || sourceFormatTokens.MatchString(core) {
			continue
		}
		label = append(label, word)
	}
	rest := strings.Trim(strings.Join(label, " "), " ()[]{},;:|/+-·")
	if rest == "" {
		return format, true
	}
	return rest + " " + format, true
}

// BuildFFmpegArgs generates the exact, safe FFmpeg argument slice from an ExecutionPlan.
// It explicitly specifies codec mapping per stream type and subtitle stream index,
// avoiding dangerous global defaults that could corrupt stylized subtitles or mux incompatible codecs.
// A plan with cover art needs the extracted cover files: use
// BuildFFmpegArgsWithCoverArt.
func BuildFFmpegArgs(execPlan *ExecutionPlan, sourcePath, candidatePath, progressPath string) ([]string, error) {
	return BuildFFmpegArgsWithCoverArt(execPlan, sourcePath, candidatePath, progressPath, nil)
}

// BuildFFmpegArgsWithCoverArt is BuildFFmpegArgs for sources with attached
// pictures. coverPaths maps each cover's source stream index to the absolute
// path of its extracted, byte-identical image (see ExtractCoverArt). Only the
// programme video is mapped and encoded (`0:V` excludes attached pictures);
// each cover is re-attached untouched as a Matroska image attachment.
func BuildFFmpegArgsWithCoverArt(execPlan *ExecutionPlan, sourcePath, candidatePath, progressPath string, coverPaths map[int]string) ([]string, error) {
	if execPlan == nil || execPlan.Plan == nil {
		return nil, fmt.Errorf("execution plan cannot be nil (fail closed)")
	}
	if strings.TrimSpace(sourcePath) == "" || strings.TrimSpace(candidatePath) == "" {
		return nil, fmt.Errorf("source and candidate paths are required (fail closed)")
	}
	covers := execPlan.CoverArt()
	videoMap := "0:v?"
	if len(covers) > 0 {
		videoMap = "0:V?"
	}
	for _, c := range covers {
		if path := coverPaths[c.SourceIndex]; !filepath.IsAbs(path) {
			return nil, fmt.Errorf("cover art stream %d has no extracted image (fail closed)", c.SourceIndex)
		}
		if c.MimeType == "" || !safeAttachmentFilename(c.Filename) {
			return nil, fmt.Errorf("cover art stream %d has no attachment name or type (fail closed)", c.SourceIndex)
		}
	}

	args := []string{
		"-y",
		"-progress", progressPath,
		"-nostats",
		"-i", sourcePath,
		"-map", videoMap,
		"-map", "0:a?",
		"-map", "0:s?",
		"-map", "0:t?",
	}

	if execPlan.Plan.PreserveMetadata {
		args = append(args, "-map_metadata", "0")
	}
	if execPlan.Plan.PreserveChapters {
		args = append(args, "-map_chapters", "0")
	}

	videoArgs, err := BuildVideoEncoderArgs(execPlan.Plan)
	if err != nil {
		return nil, err
	}
	args = append(args, videoArgs...)
	if execPlan.Plan.Compact != nil {
		// The source title often names the dropped format ("... Dolby
		// Vision", "... HDR10"), and its muxer statistics describe the old
		// bitstream; neither may survive on the SDR stream.
		args = append(args, "-metadata:s:v:0", "title=")
		args = append(args, clearStatisticsTags("v", 0)...)
	}

	// Keep every audio stream; compact only selected source codecs, never
	// downmix or remove languages. Already compact/unknown formats are copied.
	args = append(args, "-c:a", "copy")
	for _, stream := range execPlan.Streams {
		if stream.Kind == "audio" && stream.Disposition != nil {
			var flags []string
			for name, value := range stream.Disposition {
				if value > 0 {
					flags = append(flags, name)
				}
			}
			sort.Strings(flags)
			disposition := strings.Join(flags, "+")
			if disposition == "" {
				disposition = "0"
			}
			args = append(args, fmt.Sprintf("-disposition:a:%d", stream.TypeIndex), disposition)
		}
		if stream.Kind == "audio" && stream.TargetCodec != "copy" {
			if stream.TargetCodec != "aac" || stream.BitrateKbps < 1 {
				return nil, fmt.Errorf("invalid compact audio action")
			}
			// Size mode never flattens object audio, so it never retitles an
			// Atmos or DTS:X track to AAC either (fail closed).
			if execPlan.Plan.AudioMode == transcode.AudioModeCompactLossless && transcode.ObjectAudioText(stream.Title) {
				return nil, fmt.Errorf("compact_lossless would convert object audio track %d (fail closed)", stream.TypeIndex)
			}
			encoder := "aac"
			if execPlan.Plan.AudioMode == "he_aac" {
				encoder = "aac_at"
				args = append(args, fmt.Sprintf("-profile:a:%d", stream.TypeIndex), "4")
			}
			args = append(args, fmt.Sprintf("-c:a:%d", stream.TypeIndex), encoder, fmt.Sprintf("-b:a:%d", stream.TypeIndex), strconv.Itoa(stream.BitrateKbps)+"k")
			// Source muxer bitrate/size tags no longer describe an encoded track.
			args = append(args, clearStatisticsTags("a", stream.TypeIndex)...)
			if title, ok := convertedAudioTitle(stream.Title, stream.Layout, stream.Channels); ok {
				args = append(args, fmt.Sprintf("-metadata:s:a:%d", stream.TypeIndex), "title="+title)
			}
		}
	}

	// Subtitle streams: specify explicitly per subtitle stream index
	for _, stream := range execPlan.Streams {
		if stream.Kind == "subtitle" {
			args = append(args, fmt.Sprintf("-c:s:%d", stream.TypeIndex), stream.TargetCodec)
		}
	}

	// Attachments: copy
	if execPlan.Plan.PreserveAttachments {
		args = append(args, "-c:t", "copy")
	}

	// Cover art: re-attached after the mapped source attachments.
	for i, c := range covers {
		idx := execPlan.SourceAttachments + i
		args = append(args, "-attach", coverPaths[c.SourceIndex],
			fmt.Sprintf("-metadata:s:t:%d", idx), "mimetype="+c.MimeType,
			fmt.Sprintf("-metadata:s:t:%d", idx), "filename="+c.Filename)
	}

	args = append(args, candidatePath)

	return args, nil
}
