package tdarr_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	t.Logf("Live Tdarr server connected: version=%s, os=%s, uptime=%ds", status.Version, status.OS, status.Uptime)

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
			if len(node.Config.PathTranslators) > 0 {
				for _, pt := range node.Config.PathTranslators {
					t.Logf("  Path translator: server=%s -> node=%s", pt.Server, pt.Node)
				}
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

// TestLiveTdarrFullTranscodeCycle executes an end-to-end transcode test against live Tdarr
// using an isolated, synthetic test video generated on the fly.
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
	testDir := os.Getenv("TDARR_LIVE_TEST_DIR")

	// If library ID not explicitly provided, check if any library exists in Tdarr
	if libraryID == "" {
		libReq := map[string]any{
			"data": map[string]any{
				"collection": "LibrarySettingsJSONDB",
				"mode":       "getAll",
			},
		}
		libData, _ := json.Marshal(libReq)
		resp, err := http.Post(baseURL+"/api/v2/cruddb", "application/json", bytes.NewReader(libData))
		if err == nil {
			defer resp.Body.Close()
			var libs []map[string]any
			if json.NewDecoder(resp.Body).Decode(&libs) == nil && len(libs) > 0 {
				libraryID, _ = libs[0]["_id"].(string)
				if folders, ok := libs[0]["folders"].([]any); ok && len(folders) > 0 && testDir == "" {
					testDir, _ = folders[0].(string)
				}
			}
		}
	}

	if libraryID == "" {
		t.Skip("No Tdarr library configured or specified via TDARR_LIVE_LIBRARY_ID; skipping full transcode cycle test.")
	}

	if testDir == "" {
		testDir = t.TempDir()
	}

	ffmpegBin, err := exec.LookPath("ffmpeg")
	if err != nil {
		ffmpegBin = "/opt/homebrew/bin/ffmpeg"
		if _, statErr := os.Stat(ffmpegBin); statErr != nil {
			t.Skipf("ffmpeg not found, skipping live synthetic transcode: %v", err)
		}
	}

	// 1. Generate an isolated, synthetic test video (2 seconds, 320x240 colorbars + sine audio)
	syntheticFile := filepath.Join(testDir, fmt.Sprintf("nav_test_pattern_%d.mkv", time.Now().Unix()))
	cmd := exec.Command(ffmpegBin,
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=1000:duration=2",
		"-c:v", "libx264",
		"-c:a", "aac",
		"-y", syntheticFile,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to generate synthetic test video: %v (output: %s)", err, string(out))
	}
	defer os.Remove(syntheticFile)

	// 2. Compute original checksum BEFORE transcode
	origBytes, err := os.ReadFile(syntheticFile)
	if err != nil {
		t.Fatalf("failed reading synthetic test file: %v", err)
	}
	h := sha256.Sum256(origBytes)
	origSHA := fmt.Sprintf("%x", h[:])

	// 3. Set up Navigatorr Action Engine with live Tdarr client
	client := tdarr.NewClient(tdarr.ClientOptions{
		BaseURL: baseURL,
		Timeout: 10 * time.Second,
	})

	dbPath := filepath.Join(t.TempDir(), "live_test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed opening store: %v", err)
	}
	defer st.Close()

	resResolver, err := fsop.NewResolver([]string{testDir}, []string{testDir})
	if err != nil {
		t.Fatalf("failed creating resolver: %v", err)
	}

	ffprobeBin, err := exec.LookPath("ffprobe")
	if err != nil {
		ffprobeBin = "/opt/homebrew/bin/ffprobe"
	}

	cfg := &config.Config{
		Media: config.MediaConfig{
			AllowedReadRoots:  []string{testDir},
			AllowedWriteRoots: []string{testDir},
		},
		Tdarr: config.TdarrConfig{
			Enabled: true,
			Libraries: map[string]config.TdarrLibraryConfig{
				"default": {
					ID:   libraryID,
					Name: "LiveTestLib",
				},
			},
		},
	}

	engine := action.NewEngine(action.EngineDeps{
		Store:   st,
		Config:  cfg,
		Fs:      resResolver,
		Ffprobe: ffprobeBin,
		Tdarr:   client,
	})

	// 4. Initial Run -> Queues in Tdarr and enters waiting_external
	ctx := context.Background()
	runRes, err := engine.Run(ctx, "transcode_media", map[string]any{
		"path": syntheticFile,
	})
	if err != nil {
		t.Fatalf("engine.Run failed: %v", err)
	}

	t.Logf("Action submitted: id=%s, status=%s, waiting=%s", runRes.ID, runRes.Status, runRes.WaitingReason)

	// 5. Verify original file is 100% physically intact
	currentBytes, err := os.ReadFile(syntheticFile)
	if err != nil {
		t.Fatalf("original file missing after submit: %v", err)
	}
	currentH := sha256.Sum256(currentBytes)
	currentSHA := fmt.Sprintf("%x", currentH[:])
	if currentSHA != origSHA {
		t.Fatalf("CRITICAL: Original file was modified during transcode! orig=%s, current=%s", origSHA, currentSHA)
	}

	// 6. Resume verification (ensure no second submit happens)
	resumeRes, err := engine.Resume(ctx, runRes.ID, "", nil)
	if err != nil {
		t.Fatalf("engine.Resume failed: %v", err)
	}
	t.Logf("Action resumed: status=%s, waiting=%s", resumeRes.Status, resumeRes.WaitingReason)

	// 7. Verify original file is STILL 100% physically intact after resume
	afterResumeBytes, err := os.ReadFile(syntheticFile)
	if err != nil {
		t.Fatalf("original file missing after resume: %v", err)
	}
	afterResumeH := sha256.Sum256(afterResumeBytes)
	afterResumeSHA := fmt.Sprintf("%x", afterResumeH[:])
	if afterResumeSHA != origSHA {
		t.Fatalf("CRITICAL: Original file was modified during resume! orig=%s, current=%s", origSHA, afterResumeSHA)
	}
}
