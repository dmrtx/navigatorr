package transcode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
)

func fileSHA256(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func TestLiveSSHTranscodeE2E(t *testing.T) {
	if os.Getenv("NAVIGATORR_LIVE_SSH_TRANSCODE") != "1" {
		t.Skip("skipping live SSH transcode test; set NAVIGATORR_LIVE_SSH_TRANSCODE=1 to run")
	}

	host := os.Getenv("NAVIGATORR_SSH_HOST")
	if host == "" {
		host = "192.168.68.55"
	}
	user := os.Getenv("NAVIGATORR_SSH_USER")
	if user == "" {
		user = "morotxo"
	}
	remoteCmd := os.Getenv("NAVIGATORR_SSH_COMMAND")
	if remoteCmd == "" {
		remoteCmd = "/Users/morotxo/.local/bin/navigatorr-transcode"
	}
	identityFile := os.Getenv("NAVIGATORR_SSH_IDENTITY_FILE")
	if identityFile == "" {
		home, _ := os.UserHomeDir()
		defaultKey := filepath.Join(home, ".ssh", "id_ed25519")
		if _, err := os.Stat(defaultKey); err == nil {
			identityFile = defaultKey
		}
	}

	localRoot := os.Getenv("NAVIGATORR_SSH_LOCAL_ROOT")
	remoteRoot := os.Getenv("NAVIGATORR_SSH_REMOTE_ROOT")
	if localRoot == "" || remoteRoot == "" {
		localRoot = "/Volumes/media"
		remoteRoot = "/Volumes/media"
	}

	cfg := SSHConfig{
		Host:           host,
		User:           user,
		Command:        remoteCmd,
		IdentityFile:   identityFile,
		ConnectTimeout: 10 * time.Second,
		PathMappings: []PathMapping{
			{Local: localRoot, Remote: remoteRoot},
		},
	}

	execClient, err := NewSSHExecutor(cfg)
	if err != nil {
		t.Fatalf("failed to create SSH executor: %v", err)
	}

	ctx := context.Background()

	// 1. Doctor check
	if err := execClient.Doctor(ctx); err != nil {
		t.Fatalf("live SSH doctor check failed: %v", err)
	}

	// 2. Prepare synthetic source video inside localRoot
	testDir := filepath.Join(localRoot, ".navigatorr_test_live_transcode")
	if err := os.MkdirAll(testDir, 0755); err != nil {
		t.Fatalf("failed to create test directory %s: %v", testDir, err)
	}
	defer func() {
		_ = os.RemoveAll(testDir)
	}()

	sourcePath := filepath.Join(testDir, "live_synthetic_2s.mkv")
	genCmd := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=24",
		"-f", "lavfi", "-i", "sine=duration=2:frequency=1000",
		"-c:v", "libx264",
		"-c:a", "aac",
		sourcePath,
	)
	if out, err := genCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to generate synthetic video: %v (output: %s)", err, out)
	}

	initialSHA, err := fileSHA256(sourcePath)
	if err != nil {
		t.Fatalf("failed to hash synthetic source: %v", err)
	}

	jobID := "live-test-" + time.Now().Format("20060102150405")
	candidatePath := filepath.Join(testDir, ".navigatorr-candidates", "live_synthetic_2s."+jobID+".mkv")

	// 3. Submit
	job, err := execClient.Submit(ctx, Request{
		ID:            jobID,
		SourcePath:    sourcePath,
		CandidatePath: candidatePath,
		Profile:       "hevc-vt",
	})
	if err != nil {
		t.Fatalf("live SSH submit failed: %v", err)
	}
	if job.ID != jobID {
		t.Errorf("expected job ID %q, got %q", jobID, job.ID)
	}

	// 4. Poll status until completed
	deadline := time.Now().Add(60 * time.Second)
	var finalStatus JobStatus
	for time.Now().Before(deadline) {
		st, err := execClient.Status(ctx, jobID)
		if err == nil {
			finalStatus = st
			if st.Status == StatusCompleted || st.Status == StatusFailed {
				break
			}
		}
		time.Sleep(1 * time.Second)
	}

	if finalStatus.Status != StatusCompleted {
		t.Fatalf("live SSH transcode failed or timed out: %+v", finalStatus)
	}

	// 5. Inspect candidate with ffprobe
	insp, err := mediainspect.InspectDetailed(ctx, "ffprobe", candidatePath)
	if err != nil {
		t.Fatalf("inspecting candidate failed: %v", err)
	}

	if len(insp.Video) == 0 {
		t.Fatalf("candidate has no video stream")
	}
	videoCodec := strings.ToLower(insp.Video[0].Codec)
	if videoCodec != "hevc" {
		t.Errorf("expected candidate codec 'hevc', got %q", videoCodec)
	}
	if len(insp.Audio) == 0 {
		t.Errorf("expected audio stream preserved in candidate")
	}

	// 6. Verify original SHA is bit-for-bit identical
	finalSHA, err := fileSHA256(sourcePath)
	if err != nil {
		t.Fatalf("failed to hash source after live transcode: %v", err)
	}
	if finalSHA != initialSHA {
		t.Fatalf("CRITICAL INTEGRITY BREACH: original source was modified during transcode! initial: %s, final: %s", initialSHA, finalSHA)
	}
}
