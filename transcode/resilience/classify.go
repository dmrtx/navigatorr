package resilience

import "strings"

type FailureClass string

const (
	WorkerBusy                      FailureClass = "worker_busy"
	SSHTransient                    FailureClass = "ssh_transient"
	WorkerUnreachable               FailureClass = "worker_unreachable"
	EncoderTemporarilyUnavailable   FailureClass = "encoder_temporarily_unavailable"
	EncoderCapabilityUnsupported    FailureClass = "encoder_capability_unsupported"
	ContainerSubtitleIncompatible   FailureClass = "container_subtitle_incompatible"
	ContainerAudioIncompatible      FailureClass = "container_audio_incompatible"
	ContainerAttachmentIncompatible FailureClass = "container_attachment_incompatible"
	FFmpegInputCorrupt              FailureClass = "ffmpeg_input_corrupt"
	FFmpegUnknown                   FailureClass = "ffmpeg_unknown"
	ValidationDurationMismatch      FailureClass = "validation_duration_mismatch"
	ValidationStreamLoss            FailureClass = "validation_stream_loss"
	ValidationCodecMismatch         FailureClass = "validation_codec_mismatch"
	SourceChanged                   FailureClass = "source_changed"
)

func Classify(message string) FailureClass {
	s := strings.ToLower(message)
	switch {
	case strings.Contains(s, "worker busy") || strings.Contains(s, "maximum parallel jobs"):
		return WorkerBusy
	case strings.Contains(s, "connection timed out") || strings.Contains(s, "operation timed out") || strings.Contains(s, "broken pipe") || strings.Contains(s, "connection reset"):
		return SSHTransient
	case strings.Contains(s, "no route to host") || strings.Contains(s, "connection refused") || strings.Contains(s, "could not resolve hostname"):
		return WorkerUnreachable
	case strings.Contains(s, "encoder_capability_unsupported") || strings.Contains(s, "encoder capability unsupported"):
		return EncoderCapabilityUnsupported
	case strings.Contains(s, "videotoolbox") && strings.Contains(s, "temporar"):
		return EncoderTemporarilyUnavailable
	case strings.Contains(s, "subtitle") && (strings.Contains(s, "not supported") || strings.Contains(s, "incompatible")):
		return ContainerSubtitleIncompatible
	case strings.Contains(s, "audio") && strings.Contains(s, "not supported"):
		return ContainerAudioIncompatible
	case strings.Contains(s, "attachment") && strings.Contains(s, "not supported"):
		return ContainerAttachmentIncompatible
	case strings.Contains(s, "invalid data found") || strings.Contains(s, "moov atom not found") || strings.Contains(s, "input/output error"):
		return FFmpegInputCorrupt
	case strings.Contains(s, "duration") && strings.Contains(s, "mismatch"):
		return ValidationDurationMismatch
	case strings.Contains(s, "stream") && strings.Contains(s, "lost"):
		return ValidationStreamLoss
	case strings.Contains(s, "codec") && strings.Contains(s, "mismatch"):
		return ValidationCodecMismatch
	case strings.Contains(s, "source") && (strings.Contains(s, "changed") || strings.Contains(s, "sha")):
		return SourceChanged
	default:
		return FFmpegUnknown
	}
}
func Retryable(c FailureClass, allowed []string) bool {
	for _, v := range allowed {
		if string(c) == v {
			return true
		}
	}
	return false
}
