package resilience

import (
	"errors"
	"fmt"
	"testing"
)

type boundaryFailure struct {
	err   error
	class string
}

func (f boundaryFailure) Error() string        { return f.err.Error() }
func (f boundaryFailure) Unwrap() error        { return f.err }
func (f boundaryFailure) FailureClass() string { return f.class }

func TestStorageErrorClassificationSurvivesWorkerBoundary(t *testing.T) {
	cases := map[string]FailureClass{
		"opening SMB source: invalid response error: signing required": SMBSigningRequired,
		"smb_transport_error: publish: broken pipe":                    SMBTransportError,
		"smb_session_invalid: remote user session has been deleted":    SMBSessionInvalid,
		"authenticating SMB session: logon is invalid":                 SMBAuthFailed,
		"opening SMB source: access denied":                            StoragePermissionDenied,
		"opening SMB source: no such file or directory":                SourceUnreachable,
		"uploading SMB partial: input/output error":                    StorageIOError,
		"opening SMB source: connection reset":                         SMBTransportError,
		"cancelled: download: reading SMB source: context canceled":    Cancelled,
		"uploading SMB partial: no space left on device":               StorageFull,
		"SMB source changed while downloading: copied=5 stat=10":       SourceChanged,
		"ffmpeg /media/smb-story.mkv: Invalid data found":              FFmpegInputCorrupt,
		"ffmpeg /media/smb_transport_error.mkv: unknown codec option":  FFmpegUnknown,
	}
	for message, want := range cases {
		if got := Classify(message); got != want {
			t.Errorf("Classify(%q)=%s want=%s", message, got, want)
		}
	}
	// Even conflicting text must not override the concrete storage boundary.
	cause := errors.New("ffmpeg: invalid data found when processing input")
	err := fmt.Errorf("job failed: %w", boundaryFailure{err: cause, class: string(SMBSigningRequired)})
	if got := ClassifyError(err); got != SMBSigningRequired || !errors.Is(err, cause) {
		t.Fatalf("classification=%s wrapped cause preserved=%v", got, errors.Is(err, cause))
	}
}

func TestStorageRecoveryDoesNotBlindlyReplayEncode(t *testing.T) {
	classes := []FailureClass{SMBSigningRequired, SMBSessionInvalid, SMBAuthFailed, SMBTransportError, StoragePermissionDenied, StorageIOError, SourceUnreachable}
	allowed := make([]string, 0, len(classes))
	for _, class := range classes {
		allowed = append(allowed, string(class))
	}
	for _, class := range classes {
		if Retryable(class, allowed) {
			t.Errorf("storage operation recovery must precede any deliberate new job; %s cannot replay encode", class)
		}
	}
}
