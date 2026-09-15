package resilience

import "testing"

func TestClassifyPR4Taxonomy(t *testing.T) {
	cases := map[string]FailureClass{
		"idempotency_conflict: key already persists":   IdempotencyConflict,
		"execution_spec_digest mismatch (fail closed)": IdempotencyConflict,
		"transcode job was cancelled":                  Cancelled,
		"job canceled by user":                         Cancelled,
		"no space left on device":                      StorageFull,
		"ENOSPC during write":                          StorageFull,
		"process killed with SIGKILL":                  RunnerKilled,
		"process terminated unexpectedly":              RunnerKilled,
		"invalid data found when processing input":     FFmpegInputCorrupt,
		"moov atom not found":                          FFmpegInputCorrupt,
		"read failed: input/output error":              StorageIOTransient,
		"i/o error on storage":                         StorageIOTransient,
	}
	for msg, want := range cases {
		if got := Classify(msg); got != want {
			t.Errorf("Classify(%q)=%s want %s", msg, got, want)
		}
	}
}

func TestRetryablePR4Denies(t *testing.T) {
	deny := []FailureClass{RunnerKilled, FFmpegInputCorrupt, StorageFull, Cancelled, IdempotencyConflict, ValidationDurationMismatch, ValidationStreamLoss, ValidationCodecMismatch, SourceChanged}
	allowAll := []string{"runner_killed", "ffmpeg_input_corrupt", "storage_full", "cancelled", "idempotency_conflict", "validation_duration_mismatch", "validation_stream_loss", "validation_codec_mismatch", "source_changed", "storage_io_transient", "ssh_transient"}
	for _, c := range deny {
		if Retryable(c, allowAll) {
			t.Errorf("Retryable(%s) must always be false even when listed", c)
		}
	}
	if !Retryable(StorageIOTransient, []string{"storage_io_transient"}) {
		t.Error("storage_io_transient must be retryable when explicitly listed")
	}
	if Retryable(StorageIOTransient, []string{"ssh_transient"}) {
		t.Error("storage_io_transient must not retry when not listed")
	}
}
