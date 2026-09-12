package resilience

import "testing"

func TestClassifyAndRetryPolicy(t *testing.T) {
	cases := map[string]FailureClass{
		"worker busy: maximum parallel jobs reached": WorkerBusy,
		"ssh: connection timed out":                  SSHTransient,
		"ssh: connection refused":                    WorkerUnreachable,
		"subtitle codec mov_text is not supported":   ContainerSubtitleIncompatible,
		"Invalid data found when processing input":   FFmpegInputCorrupt,
		"codec mismatch":                             ValidationCodecMismatch,
		"source sha changed":                         SourceChanged,
		"some new ffmpeg problem":                    FFmpegUnknown,
	}
	for msg, want := range cases {
		if got := Classify(msg); got != want {
			t.Errorf("Classify(%q)=%s want %s", msg, got, want)
		}
	}
	if !Retryable(WorkerBusy, []string{"worker_busy"}) {
		t.Fatal("worker_busy should be retryable when explicitly allowed")
	}
	if Retryable(FFmpegInputCorrupt, []string{"worker_busy", "ssh_transient"}) {
		t.Fatal("deterministic corruption must not retry")
	}
}
