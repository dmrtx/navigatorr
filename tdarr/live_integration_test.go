package tdarr_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/tdarr"
)

// TestLiveTdarrConnection verifies connection, version, and native node detection
// against a live Tdarr 2.87.01 instance.
// Opt-in via TDARR_LIVE_TEST=1.
func TestLiveTdarrConnection(t *testing.T) {
	if os.Getenv("TDARR_LIVE_TEST") != "1" {
		t.Skip("skipping live Tdarr test; enable with TDARR_LIVE_TEST=1")
	}

	baseURL := os.Getenv("TDARR_LIVE_URL")
	if baseURL == "" {
		baseURL = "http://192.168.70.71:8265"
	}

	client := tdarr.NewClient(tdarr.ClientOptions{
		BaseURL: baseURL,
		Timeout: 5 * time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. Check Server Status
	status, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("failed to connect to live Tdarr at %s: %v", baseURL, err)
	}

	if status.Status != "good" {
		t.Errorf("expected status 'good', got %q", status.Status)
	}
	t.Logf("server connected: version=%s, os=%s, uptime=%ds", status.Version, status.OS, status.Uptime)

	// 2. Check Connected Nodes
	nodes, err := client.Nodes(ctx)
	if err != nil {
		t.Fatalf("failed to query live nodes: %v", err)
	}

	if len(nodes) == 0 {
		t.Fatalf("no nodes connected to live Tdarr server at %s", baseURL)
	}

	var foundM1Max bool
	for nodeID, node := range nodes {
		t.Logf("Discovered node %s (%s): os=%s, ffmpeg=%s, mkvpropedit=%s, workers=%d",
			node.NodeName, nodeID, node.Config.NodeType, node.Config.FFmpegPath, node.Config.MkvpropeditPath, len(node.Workers))
		if node.NodeName == "Davids-M1-Max" {
			foundM1Max = true
			t.Logf("M1 Max detected: nodeName=%s, nodeID=%s, remoteAddress=%s", node.NodeName, nodeID, node.RemoteAddress)
			for _, pt := range node.Config.PathTranslators {
				t.Logf("  Path translator: server=%s -> node=%s", pt.Server, pt.Node)
			}
		}
	}

	expectedNode := os.Getenv("TDARR_LIVE_NODE_NAME")
	if expectedNode == "" {
		expectedNode = "Davids-M1-Max"
	}
	if !foundM1Max && expectedNode == "Davids-M1-Max" {
		t.Logf("Warning: expected node %q not found among %d nodes (may be offline or renamed)", expectedNode, len(nodes))
	}
}

// TestLiveTdarrFullTranscodeCycle executes a full, end-to-end transcode against live Tdarr
// using an isolated, synthetic test video generated on the fly.
// TDARR_LIVE_LIBRARY_ID is mandatory to avoid touching productive libraries.
// Opt-in via TDARR_LIVE_TEST=1.
func TestLiveTdarrFullTranscodeCycle(t *testing.T) {
	if os.Getenv("TDARR_LIVE_TEST") != "1" {
		t.Skip("skipping live Tdarr transcode test; enable with TDARR_LIVE_TEST=1")
	}

	baseURL := os.Getenv("TDARR_LIVE_URL")
	if baseURL == "" {
		baseURL = "http://192.168.70.71:8265"
	}

	libraryID := os.Getenv("TDARR_LIVE_LIBRARY_ID")
	if libraryID == "" {
		t.Skip("TDARR_LIVE_LIBRARY_ID is required for full transcode cycle test to avoid using productive libraries. Configure a dedicated test library in Tdarr and pass TDARR_LIVE_LIBRARY_ID=<id>.")
	}

	testDir := os.Getenv("TDARR_LIVE_TEST_DIR")
	if testDir == "" {
		t.Skip("TDARR_LIVE_TEST_DIR is required for full transcode cycle test (must be a local directory mapped to the test library).")
	}

	localRoot := os.Getenv("TDARR_LIVE_LOCAL_ROOT")
	if localRoot == "" {
		localRoot = "/Volumes/media"
	}
	serverRoot := os.Getenv("TDARR_LIVE_SERVER_ROOT")
	if serverRoot == "" {
		serverRoot = "/media"
	}

	client := tdarr.NewClient(tdarr.ClientOptions{
		BaseURL: baseURL,
		Timeout: 10 * time.Second,
	})

	ctx := context.Background()

	// 1. Verify server connected
	status, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("failed to connect to Tdarr server at %s: %v", baseURL, err)
	}
	t.Logf("server connected: version=%s, os=%s, uptime=%ds", status.Version, status.OS, status.Uptime)

	// 2. Verify M1 Max node is connected
	nodes, err := client.Nodes(ctx)
	if err != nil {
		t.Fatalf("failed to query nodes: %v", err)
	}
	var foundM1Max bool
	for nodeID, node := range nodes {
		if node.NodeName == "Davids-M1-Max" {
			foundM1Max = true
			t.Logf("M1 Max detected: node=%s (%s)", node.NodeName, nodeID)
			break
		}
	}
	if !foundM1Max {
		t.Logf("Warning: M1 Max node not detected among active nodes; transcode may run on other nodes")
	}

	// 3. Validate library configuration via Tdarr API
	libSettings, err := client.GetLibrary(ctx, libraryID)
	if err != nil {
		t.Fatalf("failed to validate library %q in Tdarr: %v", libraryID, err)
	}
	if !libSettings.FolderToFolderConversion {
		t.Fatalf("ABORT: Tdarr library %q has folderToFolderConversion disabled; would overwrite in-place!", libraryID)
	}
	if libSettings.FolderToFolderConversionDeleteSource {
		t.Fatalf("ABORT: Tdarr library %q has delete source enabled; would delete original!", libraryID)
	}
	if libSettings.OutputFolder == "" {
		t.Fatalf("ABORT: Tdarr library %q has no outputFolder configured!", libraryID)
	}
	t.Logf("library validated: id=%s, name=%s, outputFolder=%s, candidate_mode=true", libSettings.ID, libSettings.Name, libSettings.OutputFolder)

	// 4. Generate synthetic test file via ffmpeg (2s SMPTE color bars + 1000Hz sine tone).
	// Use MP4 input because Tdarr's current scanner reliably exposes stream duration
	// metadata for MP4; the classic VideoToolbox plugin requires that duration when
	// calculating its target bitrate. The candidate output remains MKV.
	ffmpegBin, err := exec.LookPath("ffmpeg")
	if err != nil {
		ffmpegBin = "/opt/homebrew/bin/ffmpeg"
	}
	ffprobeBin, err := exec.LookPath("ffprobe")
	if err != nil {
		ffprobeBin = "/opt/homebrew/bin/ffprobe"
	}

	syntheticFile := filepath.Join(testDir, fmt.Sprintf("nav_live_synth_%d.mp4", time.Now().Unix()))
	cmd := exec.Command(ffmpegBin,
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=1000:duration=2",
		"-c:v", "libx264",
		"-c:a", "aac",
		"-y", syntheticFile,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed generating synthetic file: %v (output: %s)", err, string(out))
	}
	t.Logf("synthetic file generated: %s", syntheticFile)

	// Compute original checksum BEFORE transcode
	fOrig, err := os.Open(syntheticFile)
	if err != nil {
		t.Fatalf("failed reading synthetic file: %v", err)
	}
	hOrig := sha256.New()
	_, _ = io.Copy(hOrig, fOrig)
	fOrig.Close()
	origSHA := hex.EncodeToString(hOrig.Sum(nil))
	t.Logf("original SHA256 before: %s", origSHA)

	// 5. Setup Action Engine
	dbPath := filepath.Join(t.TempDir(), "live_action.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store error: %v", err)
	}
	defer st.Close()

	// Compute expected local output folder from Tdarr library settings
	expectedLocalOutputFolder := libSettings.OutputFolder
	if strings.HasPrefix(filepath.ToSlash(libSettings.OutputFolder), filepath.ToSlash(serverRoot)) {
		rel := strings.TrimPrefix(filepath.ToSlash(libSettings.OutputFolder), filepath.ToSlash(serverRoot))
		expectedLocalOutputFolder = filepath.Join(localRoot, rel)
	}

	allowedReadRoots := []string{testDir, localRoot, expectedLocalOutputFolder}
	allowedWriteRoots := []string{testDir, localRoot, expectedLocalOutputFolder}

	resResolver, err := fsop.NewResolver(allowedReadRoots, allowedWriteRoots)
	if err != nil {
		t.Fatalf("resolver error: %v", err)
	}

	cfg := &config.Config{
		Media: config.MediaConfig{
			AllowedReadRoots:  allowedReadRoots,
			AllowedWriteRoots: allowedWriteRoots,
		},
		Tdarr: config.TdarrConfig{
			Enabled: true,
			PathMappings: []config.PathMapping{
				{
					Local:  localRoot,
					Server: serverRoot,
				},
			},
			Libraries: map[string]config.TdarrLibraryConfig{
				"default": {
					ID:           libraryID,
					Name:         libSettings.Name,
					OutputFolder: libSettings.OutputFolder,
				},
			},
		},
	}

	// Verify path translation before submit:
	// 1) TranslateLocalToServer(syntheticFile) != syntheticFile
	// 2) Translated server path must start with server root
	// 3) Translated server path must NOT start with or contain /Volumes/
	serverPath := cfg.Tdarr.TranslateLocalToServer(syntheticFile)
	if serverPath == syntheticFile {
		t.Fatalf("local path %q was not translated to server path! Ensure TDARR_LIVE_TEST_DIR (%s) is within TDARR_LIVE_LOCAL_ROOT (%s)", syntheticFile, testDir, localRoot)
	}
	if strings.Contains(serverPath, "/Volumes/") || strings.HasPrefix(serverPath, "/Volumes") {
		t.Fatalf("CRITICAL: translated server path %q contains /Volumes/; refusing to send unmapped macOS path to Tdarr server", serverPath)
	}
	normServerRoot := filepath.Clean(filepath.ToSlash(serverRoot))
	normServerPath := filepath.Clean(filepath.ToSlash(serverPath))
	if !strings.HasPrefix(normServerPath, normServerRoot) {
		t.Fatalf("translated server path %q does not start with expected server root %q", serverPath, normServerRoot)
	}
	t.Logf("Path translation verified before submit: local %s -> server %s", syntheticFile, serverPath)

	submitted := false
	terminal := false
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		cancelledActive := false
		if submitted && !terminal {
			if cleanupNodes, err := client.Nodes(cleanupCtx); err == nil {
				for nodeID, node := range cleanupNodes {
					for workerID, worker := range node.Workers {
						if worker.Idle || filepath.Clean(worker.File) != filepath.Clean(serverPath) {
							continue
						}
						if err := client.Cancel(cleanupCtx, tdarr.CancelRequest{
							NodeID:   nodeID,
							WorkerID: workerID,
							Cause:    "Navigatorr isolated live-test cleanup",
						}); err != nil {
							t.Logf("live-test cleanup warning: failed cancelling active Tdarr worker %s/%s: %v", nodeID, workerID, err)
						} else {
							cancelledActive = true
							t.Logf("live-test cleanup: cancelled active Tdarr worker %s/%s for %s", nodeID, workerID, serverPath)
						}
					}
				}
			}
		}

		// Never remove a source that is still merely queued: doing so can leave
		// Tdarr's only worker stuck scanning a file that vanished after a timeout.
		if !submitted || terminal || cancelledActive {
			if err := os.Remove(syntheticFile); err != nil && !os.IsNotExist(err) {
				t.Logf("live-test cleanup warning: failed removing synthetic source: %v", err)
			}
		} else {
			t.Logf("live-test cleanup: preserving queued synthetic source for next isolated preflight: %s", syntheticFile)
		}
	}()

	engine := action.NewEngine(action.EngineDeps{
		Store:   st,
		Config:  cfg,
		Fs:      resResolver,
		Ffprobe: ffprobeBin,
		Tdarr:   client,
	})

	// 6. Submit via Action Engine Run()
	runRes, err := engine.Run(ctx, "transcode_media", map[string]any{
		"path": syntheticFile,
	})
	if err != nil {
		t.Fatalf("engine.Run failed: %v", err)
	}
	if runRes.Status != action.StatusWaitingExternal {
		t.Fatalf("expected waiting_external after submit, got %s (error: %s)", runRes.Status, runRes.Error)
	}
	submitted = true
	ref := runRes.Outputs["external_reference"].(string)
	t.Logf("synthetic file submitted: id=%s, status=%s, ref=%s", runRes.ID, runRes.Status, ref)

	// 7. Wait/Poll for Tdarr execution (max 90s, check every 3s)
	timeout := 90 * time.Second
	deadline := time.Now().Add(timeout)
	var observedQueuedRunning bool
	var discoveredJobID string
	var completedStatus *tdarr.JobStatusResponse

	for time.Now().Before(deadline) {
		jobStatus, err := client.JobStatus(ctx, ref)
		if err == nil && jobStatus.Found {
			if jobStatus.Status == "queued" || jobStatus.Status == "running" {
				if !observedQueuedRunning {
					t.Logf("queued/running observed: status=%s, progress=%.1f%%, fps=%.1f", jobStatus.Status, jobStatus.Progress, jobStatus.FPS)
					observedQueuedRunning = true
				}
				if jobStatus.JobId != "" && discoveredJobID == "" {
					discoveredJobID = jobStatus.JobId
					t.Logf("real jobId discovered if Tdarr exposes it: %s", discoveredJobID)
				}
			}
			if jobStatus.Status == "completed" {
				completedStatus = jobStatus
				terminal = true
				t.Logf("completed observed: jobId=%s, outputPath=%s", jobStatus.JobId, jobStatus.OutputPath)
				break
			}
			if jobStatus.Status == "failed" {
				terminal = true
				t.Fatalf("transcode job failed in Tdarr: %s (%s)", jobStatus.Error, jobStatus.Details)
			}
		}
		time.Sleep(3 * time.Second)
	}

	if completedStatus == nil {
		t.Fatalf("timed out waiting for transcode to complete in Tdarr after %v", timeout)
	}

	// 8. Resume Action to run validation and acceptance
	resumeRes, err := engine.Resume(ctx, runRes.ID, "", nil)
	if err != nil {
		t.Fatalf("engine.Resume failed: %v", err)
	}
	if resumeRes.Status != action.StatusCompleted {
		t.Fatalf("expected completed status after resume, got %s (error: %s, reason: %s)", resumeRes.Status, resumeRes.Error, resumeRes.WaitingReason)
	}
	t.Logf("Action completed: id=%s", resumeRes.ID)

	// 9. Verify candidate path exists and is distinct
	candidatePath, _ := resumeRes.Outputs["candidate_path"].(string)
	if candidatePath == "" {
		candidatePath, _ = resumeRes.Outputs["output_path"].(string)
	}
	if candidatePath == "" {
		t.Fatalf("no candidate path reported in outputs")
	}
	t.Logf("candidate path found: %s", candidatePath)

	// Verify candidate server path translation matches candidatePath
	serverOut, _ := resumeRes.Outputs["server_output_path"].(string)
	if serverOut == "" && completedStatus != nil {
		serverOut = completedStatus.OutputPath
	}
	if serverOut != "" {
		expectedLocal := cfg.Tdarr.TranslateServerToLocal(serverOut)
		if filepath.Clean(candidatePath) != filepath.Clean(expectedLocal) {
			t.Fatalf("candidate_path %q does not match translated server_output_path %q (server path: %s)", candidatePath, expectedLocal, serverOut)
		}
		t.Logf("Candidate server path translated successfully: server %s -> local %s", serverOut, expectedLocal)
	}

	if filepath.Clean(candidatePath) == filepath.Clean(syntheticFile) {
		t.Fatalf("CRITICAL: candidate path is identical to original source file!")
	}

	candFi, err := os.Stat(candidatePath)
	if err != nil {
		t.Fatalf("candidate file not found on disk at %s: %v", candidatePath, err)
	}
	if candFi.Size() == 0 {
		t.Fatalf("candidate file is 0 bytes")
	}
	defer os.Remove(candidatePath)

	// 10. Probe candidate with ffprobe
	probeCmd := exec.Command(ffprobeBin, "-v", "error", "-show_entries", "stream=codec_type,codec_name", "-of", "json", candidatePath)
	probeOut, err := probeCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("candidate ffprobe failed: %v (output: %s)", err, string(probeOut))
	}
	t.Logf("candidate ffprobe PASS: valid video/audio streams verified")

	// 11. Verify original file SHA256 after completion
	fAfter, err := os.Open(syntheticFile)
	if err != nil {
		t.Fatalf("original file missing after transcode completion: %v", err)
	}
	hAfter := sha256.New()
	_, _ = io.Copy(hAfter, fAfter)
	fAfter.Close()
	afterSHA := hex.EncodeToString(hAfter.Sum(nil))

	if afterSHA != origSHA {
		t.Fatalf("CRITICAL: Original file was modified during transcode! before=%s, after=%s", origSHA, afterSHA)
	}
	t.Logf("original SHA before == SHA after completion: %s == %s", origSHA, afterSHA)

	// 12. Verify no duplicate submit
	secondResume, err := engine.Resume(ctx, runRes.ID, "", nil)
	if err != nil {
		t.Fatalf("second resume failed: %v", err)
	}
	if secondResume.Status != action.StatusCompleted {
		t.Fatalf("expected action to remain completed, got %s", secondResume.Status)
	}
	t.Logf("no duplicate submit: action terminal and untouched")
}
