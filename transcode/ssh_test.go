package transcode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSSHExecutor_PathMapping(t *testing.T) {
	cfg := SSHConfig{
		Host:    "192.0.2.10",
		Command: "/usr/local/bin/navigatorr-transcode",
		PathMappings: []PathMapping{
			{Local: "/media", Remote: "/Volumes/media"},
			{Local: "/media/Anime/Special", Remote: "/Volumes/fast-storage/Special"},
		},
	}

	exec, err := NewSSHExecutor(cfg)
	if err != nil {
		t.Fatalf("failed to create SSHExecutor: %v", err)
	}

	tests := []struct {
		name       string
		local      string
		wantRemote string
		expectErr  bool
	}{
		{
			name:       "exact match",
			local:      "/media",
			wantRemote: "/Volumes/media",
		},
		{
			name:       "basic subpath",
			local:      "/media/Movies/Test.mkv",
			wantRemote: "/Volumes/media/Movies/Test.mkv",
		},
		{
			name:       "longest prefix match",
			local:      "/media/Anime/Special/OVA.mkv",
			wantRemote: "/Volumes/fast-storage/Special/OVA.mkv",
		},
		{
			name:       "unicode, japanese, brackets, and spaces",
			local:      "/media/Anime/[Judas] 劇場版 少女☆歌劇 レヴュースタァライト (2021)/Ep 01 [1080p].mkv",
			wantRemote: "/Volumes/media/Anime/[Judas] 劇場版 少女☆歌劇 レヴュースタァライト (2021)/Ep 01 [1080p].mkv",
		},
		{
			name:      "boundary safety: /media2 must not match /media",
			local:     "/media2/test.mkv",
			expectErr: true,
		},
		{
			name:      "no mapping fails closed",
			local:     "/unmapped/path/file.mkv",
			expectErr: true,
		},
	}

	for _, tc := range tests {
		t.Run("LocalToRemote_"+tc.name, func(t *testing.T) {
			got, err := exec.TranslateLocalToRemote(tc.local)
			if tc.expectErr {
				if err == nil {
					t.Fatalf("expected error, got remote %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantRemote {
				t.Errorf("got %q, want %q", got, tc.wantRemote)
			}
		})
	}

	// Test reverse translation: Remote -> Local
	revTests := []struct {
		name      string
		remote    string
		wantLocal string
		expectErr bool
	}{
		{
			name:      "reverse basic",
			remote:    "/Volumes/media/Movies/Test.mkv",
			wantLocal: "/media/Movies/Test.mkv",
		},
		{
			name:      "reverse longest prefix",
			remote:    "/Volumes/fast-storage/Special/OVA.mkv",
			wantLocal: "/media/Anime/Special/OVA.mkv",
		},
		{
			name:      "reverse unicode and spaces",
			remote:    "/Volumes/media/Anime/[Judas] 劇場版 (2021)/Ep 01.mkv",
			wantLocal: "/media/Anime/[Judas] 劇場版 (2021)/Ep 01.mkv",
		},
		{
			name:      "reverse boundary safety: /Volumes/media_backup must not match",
			remote:    "/Volumes/media_backup/file.mkv",
			expectErr: true,
		},
		{
			name:      "reverse unmapped fails closed",
			remote:    "/Volumes/unknown/file.mkv",
			expectErr: true,
		},
	}

	for _, tc := range revTests {
		t.Run("RemoteToLocal_"+tc.name, func(t *testing.T) {
			got, err := exec.TranslateRemoteToLocal(tc.remote)
			if tc.expectErr {
				if err == nil {
					t.Fatalf("expected error, got local %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantLocal {
				t.Errorf("got %q, want %q", got, tc.wantLocal)
			}
		})
	}
}

func TestSSHExecutor_BuildSSHArgs(t *testing.T) {
	cfg := SSHConfig{
		Host:           "m1.local",
		Port:           2222,
		User:           "transcoder",
		Command:        "/opt/bin/navigatorr-transcode",
		IdentityFile:   "/home/user/.ssh/id_ed25519",
		KnownHostsFile: "/run/secrets/navigatorr_known_hosts",
		ConnectTimeout: 10 * time.Second,
	}
	exec, err := NewSSHExecutor(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	args := exec.buildSSHArgs()
	argsStr := strings.Join(args, " ")

	if !strings.Contains(argsStr, "BatchMode=yes") {
		t.Errorf("missing BatchMode=yes in %s", argsStr)
	}
	if !strings.Contains(argsStr, "StrictHostKeyChecking=yes") {
		t.Errorf("missing StrictHostKeyChecking=yes in %s", argsStr)
	}
	if !strings.Contains(argsStr, "ConnectTimeout=10") {
		t.Errorf("missing ConnectTimeout=10 in %s", argsStr)
	}
	if !strings.Contains(argsStr, "-p 2222") {
		t.Errorf("missing port flag in %s", argsStr)
	}
	if !strings.Contains(argsStr, "UserKnownHostsFile=/run/secrets/navigatorr_known_hosts") {
		t.Errorf("missing UserKnownHostsFile flag in %s", argsStr)
	}
	if !strings.Contains(argsStr, "-i /home/user/.ssh/id_ed25519") {
		t.Errorf("missing identity file flag in %s", argsStr)
	}
	if !strings.Contains(argsStr, "-l transcoder") {
		t.Errorf("missing user flag in %s", argsStr)
	}
	if args[len(args)-1] != "m1.local" {
		t.Errorf("expected host to be final argument, got %q", args[len(args)-1])
	}
}

func createFakeSSHBinary(t *testing.T, scriptContent string) string {
	t.Helper()
	dir := t.TempDir()
	binPath := filepath.Join(dir, "ssh")
	content := fmt.Sprintf("#!/bin/sh\n%s\n", scriptContent)
	if err := os.WriteFile(binPath, []byte(content), 0755); err != nil {
		t.Fatalf("failed to write fake ssh binary: %v", err)
	}
	return binPath
}

func TestSSHExecutor_SubmitAndStatusFlow(t *testing.T) {
	// Fake ssh script that checks stdin for submit, and outputs json
	fakeScript := `
for arg in "$@"; do
    if [ "$arg" = "doctor" ]; then
        echo '{"ok": true, "os": "darwin", "arch": "arm64"}'
        exit 0
    fi
    if [ "$arg" = "submit" ]; then
        input=$(cat)
        # Verify input has json content
        echo "$input" | grep -q "source_path" || exit 1
        echo '{"id": "job-test-123"}'
        exit 0
    fi
    if [ "$arg" = "status" ]; then
        echo '{"id": "job-test-123", "status": "running", "progress": 42.5, "fps": 120.0, "speed": 3.5, "candidate_path": "/Volumes/media/out.mkv"}'
        exit 0
    fi
    if [ "$arg" = "cancel" ]; then
        echo '{"id": "job-test-123", "status": "cancelled"}'
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

	// 1. Doctor
	if err := exec.Doctor(ctx); err != nil {
		t.Fatalf("doctor failed: %v", err)
	}

	// 2. Submit
	req := Request{
		ID:            "job-test-123",
		SourcePath:    "/local/media/source.mkv",
		CandidatePath: "/local/media/candidates/cand.mkv",
		Profile:       "hevc-vt",
	}
	job, err := exec.Submit(ctx, req)
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}
	if job.ID != "job-test-123" {
		t.Errorf("got job ID %q, want %q", job.ID, "job-test-123")
	}

	// 3. Status (and verify candidate path translated back to local)
	st, err := exec.Status(ctx, "job-test-123")
	if err != nil {
		t.Fatalf("status failed: %v", err)
	}
	if st.Status != StatusRunning {
		t.Errorf("got status %q, want %q", st.Status, StatusRunning)
	}
	if st.Progress != 42.5 {
		t.Errorf("got progress %v, want 42.5", st.Progress)
	}
	if st.CandidatePath != "/local/media/out.mkv" {
		t.Errorf("expected candidate_path translated to local: got %q, want %q", st.CandidatePath, "/local/media/out.mkv")
	}

	// 4. Cancel
	if err := exec.Cancel(ctx, "job-test-123"); err != nil {
		t.Fatalf("cancel failed: %v", err)
	}
}
