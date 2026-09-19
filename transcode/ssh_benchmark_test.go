package transcode

import (
	"context"
	"strings"
	"testing"
)

func TestSSHExecutor_BenchmarkOperations(t *testing.T) {
	fakeScript := `#!/bin/sh
for arg in "$@"; do
    if [ "$arg" = "benchmark_submit" ]; then
        input=$(cat)
        # Ensure remote translated path was supplied
        echo "$input" | grep -q "/Volumes/media/source.mkv" || exit 2
        echo "$input" | grep -q "bench-test-123" || exit 3
        echo '{"protocol_version": 2, "id": "bench-test-123", "status": "queued"}'
        exit 0
    fi
    if [ "$arg" = "benchmark_status" ]; then
        echo '{"protocol_version": 2, "id": "bench-test-123", "status": "running", "source_path": "/Volumes/media/source.mkv", "progress": 50.0}'
        exit 0
    fi
    if [ "$arg" = "benchmark_cancel" ]; then
        echo '{"protocol_version": 2, "id": "bench-test-123", "status": "cancelled"}'
        exit 0
    fi
done
exit 1
`
	fakeSSH := createFakeSSHBinary(t, fakeScript)

	cfg := SSHConfig{
		Host:    "test.host",
		Command: "/remote/bin/navigatorr-transcode",
		PathMappings: []PathMapping{
			{Local: "/local/media", Remote: "/Volumes/media"},
		},
	}
	exec, err := NewSSHExecutor(cfg, WithSSHBinary(fakeSSH))
	if err != nil {
		t.Fatalf("failed to init SSHExecutor: %v", err)
	}

	ctx := context.Background()

	// 1. Submit
	req := BenchmarkRequest{
		ProtocolVersion: WorkerProtocolVersion,
		ID:              "bench-test-123",
		SourcePath:      "/local/media/source.mkv",
		Metric:          "vmaf",
		Samples: []BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 10.0, DurationSeconds: 20.0},
		},
		Candidates: []BenchmarkCandidate{
			{ID: "c1", Quality: 65},
		},
	}

	job, err := exec.BenchmarkSubmit(ctx, req)
	if err != nil {
		t.Fatalf("BenchmarkSubmit failed: %v", err)
	}
	if job.ID != "bench-test-123" {
		t.Errorf("got job ID %q, want 'bench-test-123'", job.ID)
	}

	// 2. Status
	st, err := exec.BenchmarkStatus(ctx, "bench-test-123")
	if err != nil {
		t.Fatalf("BenchmarkStatus failed: %v", err)
	}
	if st.Status != "running" {
		t.Errorf("got status %q, want 'running'", st.Status)
	}
	if st.SourcePath != "/local/media/source.mkv" {
		t.Errorf("expected translated local source path, got %q", st.SourcePath)
	}

	// 3. Cancel
	if err := exec.BenchmarkCancel(ctx, "bench-test-123"); err != nil {
		t.Fatalf("BenchmarkCancel failed: %v", err)
	}
}

func TestSSHExecutor_BenchmarkSubmit_OldWorkerRejection(t *testing.T) {
	// Old worker returns unknown command
	fakeScript := `#!/bin/sh
echo '{"error": "unknown command \"benchmark_submit\""}'
exit 1
`
	fakeSSH := createFakeSSHBinary(t, fakeScript)

	cfg := SSHConfig{
		Host:    "test.host",
		Command: "/remote/bin/navigatorr-transcode",
		PathMappings: []PathMapping{
			{Local: "/local/media", Remote: "/Volumes/media"},
		},
	}
	exec, err := NewSSHExecutor(cfg, WithSSHBinary(fakeSSH))
	if err != nil {
		t.Fatalf("failed to init SSHExecutor: %v", err)
	}

	req := BenchmarkRequest{
		ProtocolVersion: WorkerProtocolVersion,
		ID:              "bench-test-123",
		SourcePath:      "/local/media/source.mkv",
		Metric:          "vmaf",
		Samples: []BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 10.0, DurationSeconds: 20.0},
		},
		Candidates: []BenchmarkCandidate{
			{ID: "c1", Quality: 65},
		},
	}

	_, err = exec.BenchmarkSubmit(context.Background(), req)
	if err == nil {
		t.Fatalf("expected error from old worker, got nil")
	}
	if !strings.Contains(err.Error(), "ssh benchmark_submit failed") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestSSHExecutor_BenchmarkSubmit_ProtocolMismatch(t *testing.T) {
	// Worker reports an unsupported future protocol version
	fakeScript := `#!/bin/sh
echo '{"protocol_version": 99, "id": "bench-test-123", "status": "queued"}'
exit 0
`
	fakeSSH := createFakeSSHBinary(t, fakeScript)

	cfg := SSHConfig{
		Host:    "test.host",
		Command: "/remote/bin/navigatorr-transcode",
		PathMappings: []PathMapping{
			{Local: "/local/media", Remote: "/Volumes/media"},
		},
	}
	exec, err := NewSSHExecutor(cfg, WithSSHBinary(fakeSSH))
	if err != nil {
		t.Fatalf("failed to init SSHExecutor: %v", err)
	}

	req := BenchmarkRequest{
		ProtocolVersion: WorkerProtocolVersion,
		ID:              "bench-test-123",
		SourcePath:      "/local/media/source.mkv",
		Metric:          "vmaf",
		Samples: []BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 10.0, DurationSeconds: 20.0},
		},
		Candidates: []BenchmarkCandidate{
			{ID: "c1", Quality: 65},
		},
	}

	_, err = exec.BenchmarkSubmit(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "unsupported protocol version") {
		t.Fatalf("expected unsupported protocol version error, got: %v", err)
	}
}

func TestSSHExecutor_BenchmarkStatus_ProtocolMismatch(t *testing.T) {
	fakeScript := `#!/bin/sh
echo '{"protocol_version": 99, "id": "bench-test-123", "status": "running"}'
exit 0
`
	fakeSSH := createFakeSSHBinary(t, fakeScript)

	cfg := SSHConfig{
		Host:    "test.host",
		Command: "/remote/bin/navigatorr-transcode",
		PathMappings: []PathMapping{
			{Local: "/local/media", Remote: "/Volumes/media"},
		},
	}
	exec, err := NewSSHExecutor(cfg, WithSSHBinary(fakeSSH))
	if err != nil {
		t.Fatalf("failed to init SSHExecutor: %v", err)
	}

	_, err = exec.BenchmarkStatus(context.Background(), "bench-test-123")
	if err == nil || !strings.Contains(err.Error(), "unsupported protocol version") {
		t.Fatalf("expected protocol version error, got: %v", err)
	}
}

func TestSSHExecutor_BenchmarkCancel_ProtocolMismatch(t *testing.T) {
	fakeScript := `#!/bin/sh
echo '{"protocol_version": 99, "id": "bench-test-123", "status": "cancelled"}'
exit 0
`
	fakeSSH := createFakeSSHBinary(t, fakeScript)

	cfg := SSHConfig{
		Host:    "test.host",
		Command: "/remote/bin/navigatorr-transcode",
		PathMappings: []PathMapping{
			{Local: "/local/media", Remote: "/Volumes/media"},
		},
	}
	exec, err := NewSSHExecutor(cfg, WithSSHBinary(fakeSSH))
	if err != nil {
		t.Fatalf("failed to init SSHExecutor: %v", err)
	}

	err = exec.BenchmarkCancel(context.Background(), "bench-test-123")
	if err == nil || !strings.Contains(err.Error(), "unsupported protocol version") {
		t.Fatalf("expected protocol version error, got: %v", err)
	}
}
