package resilience

import (
	"errors"
	"strings"
)

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
	StorageIOTransient              FailureClass = "storage_io_transient"
	RunnerKilled                    FailureClass = "runner_killed"
	StorageFull                     FailureClass = "storage_full"
	Cancelled                       FailureClass = "cancelled"
	IdempotencyConflict             FailureClass = "idempotency_conflict"
	SMBSigningRequired              FailureClass = "smb_signing_required"
	SMBSessionInvalid               FailureClass = "smb_session_invalid"
	SMBAuthFailed                   FailureClass = "smb_auth_failed"
	SMBTransportError               FailureClass = "smb_transport_error"
	StoragePermissionDenied         FailureClass = "storage_permission_denied"
	StorageIOError                  FailureClass = "storage_io_error"
	SourceUnreachable               FailureClass = "source_unreachable"
)

// ClassifyError preserves a typed boundary failure before considering messages.
// Use Classify for errors restored from older persisted jobs or HTTP strings.
func ClassifyError(err error) FailureClass {
	if err == nil {
		return ""
	}
	var classified interface{ FailureClass() string }
	if errors.As(err, &classified) && classified.FailureClass() != "" {
		return FailureClass(classified.FailureClass())
	}
	return Classify(err.Error())
}

func Classify(message string) FailureClass {
	s := strings.ToLower(message)
	// Explicit serialized classes take precedence over generic phrases such
	// as "connection reset", which formerly labeled SMB failures as SSH.
	for _, class := range []FailureClass{SMBSigningRequired, SMBSessionInvalid, SMBAuthFailed, SMBTransportError, StoragePermissionDenied, StorageIOError, SourceUnreachable, Cancelled, StorageFull} {
		if s == string(class) || strings.Contains(s, string(class)+":") {
			return class
		}
	}
	if legacySMBBoundary(s) {
		switch {
		case strings.Contains(s, "canceled") || strings.Contains(s, "cancelled"):
			return Cancelled
		case strings.Contains(s, "no space left") || strings.Contains(s, "disk full") || strings.Contains(s, "enospc"):
			return StorageFull
		case strings.Contains(s, "signing required"):
			return SMBSigningRequired
		case strings.Contains(s, "session expired") || strings.Contains(s, "session has expired") || strings.Contains(s, "session deleted") || strings.Contains(s, "invalid session"):
			return SMBSessionInvalid
		case strings.Contains(s, "logon failure") || strings.Contains(s, "logon is invalid") || strings.Contains(s, "authentication failed"):
			return SMBAuthFailed
		case strings.Contains(s, "permission denied") || strings.Contains(s, "access denied") || strings.Contains(s, "operation not permitted"):
			return StoragePermissionDenied
		case strings.Contains(s, "no such file") || strings.Contains(s, "does not exist"):
			return SourceUnreachable
		case strings.Contains(s, "source") && strings.Contains(s, "changed"):
			return SourceChanged
		case strings.Contains(s, "connection reset") || strings.Contains(s, "broken pipe") || strings.Contains(s, "connection refused") || strings.Contains(s, "timed out") || strings.Contains(s, "deadline exceeded") || strings.Contains(s, "no route to host"):
			return SMBTransportError
		default:
			return StorageIOError
		}
	}
	switch {
	case strings.Contains(s, "idempotency_conflict") || strings.Contains(s, "idempotency conflict") || (strings.Contains(s, "execution_spec_digest") && strings.Contains(s, "mismatch")):
		return IdempotencyConflict
	case strings.Contains(s, "cancelled") || strings.Contains(s, "canceled"):
		return Cancelled
	case strings.Contains(s, "no space left") || strings.Contains(s, "no-space") || strings.Contains(s, "no space") || strings.Contains(s, "enospc") || strings.Contains(s, "disk full"):
		return StorageFull
	case strings.Contains(s, "permission denied") || strings.Contains(s, "operation not permitted"):
		return StoragePermissionDenied
	case strings.Contains(s, "sigkill") || strings.Contains(s, "killed") || strings.Contains(s, "process terminated unexpectedly"):
		return RunnerKilled
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
	case strings.Contains(s, "invalid data found") || strings.Contains(s, "moov atom not found"):
		return FFmpegInputCorrupt
	case strings.Contains(s, "input/output error") || strings.Contains(s, "input output error") || strings.Contains(s, "i/o error"):
		return StorageIOTransient
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

func legacySMBBoundary(message string) bool {
	if strings.HasPrefix(message, "smb:") || strings.HasPrefix(message, "smb ") || strings.Contains(message, ": smb:") || strings.Contains(message, ": smb ") {
		return true
	}
	for _, operation := range []string{"opening smb ", "statting smb ", "reading smb ", "closing smb ", "authenticating smb ", "connecting to smb ", "mounting smb ", "uploading smb ", "verifying smb ", "ensuring smb ", "creating exclusive smb ", "renaming smb ", "reading back smb "} {
		if strings.Contains(message, operation) {
			return true
		}
	}
	return false
}

func Retryable(c FailureClass, allowed []string) bool {
	switch c {
	case RunnerKilled, FFmpegInputCorrupt, StorageFull, Cancelled, IdempotencyConflict,
		ValidationDurationMismatch, ValidationStreamLoss, ValidationCodecMismatch, SourceChanged,
		SMBSigningRequired, SMBSessionInvalid, SMBAuthFailed, SMBTransportError,
		StoragePermissionDenied, StorageIOError, SourceUnreachable:
		// SMB operations already recover on a fresh signed session. Replaying
		// the full encode after exhausted recovery is unsafe and wasteful.
		return false
	}
	for _, v := range allowed {
		if string(c) == v {
			return true
		}
	}
	return false
}
