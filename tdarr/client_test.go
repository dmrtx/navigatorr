package tdarr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStatus_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/status" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ServerStatus{
			Status:       "good",
			IsProduction: true,
			OS:           "linux",
			Version:      "2.87.01",
			BuildDate:    "2026_09_10T04_17_04z",
			Uptime:       1234,
			ServerEngine: "nodejs",
		})
	}))
	defer ts.Close()

	c := NewClient(ClientOptions{BaseURL: ts.URL, Timeout: 2 * time.Second})
	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st.Status != "good" || st.Version != "2.87.01" || !st.IsProduction {
		t.Errorf("unexpected status: %+v", st)
	}
}

func TestStatus_MalformedResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("not valid json{"))
	}))
	defer ts.Close()

	c := NewClient(ClientOptions{BaseURL: ts.URL})
	_, err := c.Status(context.Background())
	if err == nil {
		t.Fatal("expected error on malformed json, got nil")
	}
	if !strings.Contains(err.Error(), "unmarshaling tdarr response") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestStatus_HTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer ts.Close()

	c := NewClient(ClientOptions{BaseURL: ts.URL})
	_, err := c.Status(context.Background())
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
	var httpErr *HTTPError
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected 500 status code in error, got: %v", err)
	}
	_ = httpErr
}

func TestStatus_Timeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c := NewClient(ClientOptions{BaseURL: ts.URL, Timeout: 20 * time.Millisecond})
	_, err := c.Status(context.Background())
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestNodes_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/get-nodes" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"node1": {
				"_id": "node1",
				"nodeName": "Davids-M1-Max",
				"remoteAddress": "192.168.68.55",
				"config": {
					"nodeName": "Davids-M1-Max",
					"ffmpegPath": "/opt/homebrew/bin/ffmpeg",
					"pathTranslators": [{"server": "/media", "node": "/Volumes/media"}]
				},
				"workers": {
					"w1": {
						"_id": "w1",
						"workerType": "transcodecpu",
						"file": "/media/Anime/ep1.mkv",
						"status": "Transcoding",
						"percentage": 42.5,
						"ETA": "00:05:30",
						"fps": 60.2,
						"jobId": "job-123",
						"idle": false
					}
				}
			}
		}`))
	}))
	defer ts.Close()

	c := NewClient(ClientOptions{BaseURL: ts.URL})
	nodes, err := c.Nodes(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	n, ok := nodes["node1"]
	if !ok || n.NodeName != "Davids-M1-Max" {
		t.Fatalf("unexpected node: %+v", n)
	}
	if w, ok := n.Workers["w1"]; !ok || w.Percentage != 42.5 || w.ETA != "00:05:30" {
		t.Fatalf("unexpected worker: %+v", w)
	}
}

func TestSubmit_Success(t *testing.T) {
	var receivedBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/scan-files" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`"Scan started"`))
	}))
	defer ts.Close()

	c := NewClient(ClientOptions{BaseURL: ts.URL})
	resp, err := c.Submit(context.Background(), SubmitRequest{
		FilePath:  "/media/Anime/Sousou no Frieren/s01e01.mkv",
		LibraryID: "lib-anime-123",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Success || resp.ExternalRef.LibraryID != "lib-anime-123" || resp.ExternalRef.ServerPath != "/media/Anime/Sousou no Frieren/s01e01.mkv" {
		t.Errorf("unexpected submit response: %+v", resp)
	}
}

func TestSubmit_EmptyLibraryFails(t *testing.T) {
	c := NewClient(ClientOptions{BaseURL: "http://localhost"})
	_, err := c.Submit(context.Background(), SubmitRequest{
		FilePath:  "/media/Anime/ep01.mkv",
		LibraryID: "",
	})
	if err == nil {
		t.Fatal("expected error when library_id is empty, got nil")
	}
	if !strings.Contains(err.Error(), "library_id is required") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestJobStatus_RunningOnWorker(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/get-nodes" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{
				"node1": {
					"_id": "node1",
					"nodeName": "Davids-M1-Max",
					"workers": {
						"w1": {
							"_id": "w1",
							"workerType": "transcodegpu",
							"file": "/media/Anime/Monster/ep01.mkv",
							"status": "Transcoding",
							"percentage": 55.0,
							"ETA": "00:02:10",
							"fps": 88.5,
							"jobId": "job-monster-1",
							"idle": false
						}
					}
				}
			}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	c := NewClient(ClientOptions{BaseURL: ts.URL})
	st, err := c.JobStatus(context.Background(), "/media/Anime/Monster/ep01.mkv")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !st.Found || st.Status != "running" || st.Progress != 55.0 || st.ETA != "00:02:10" {
		t.Errorf("unexpected status: %+v", st)
	}
}

func TestJobStatus_CompletedInReport(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/get-nodes":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{}`))
		case "/api/v2/job-reports/job-xyz":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{
				"jobId": "job-xyz",
				"isJobRunning": false,
				"jobReportExists": true,
				"downloadPath": "/media/Anime/Monster/ep01.mkv",
				"jobRecord": {"status": "success"}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	c := NewClient(ClientOptions{BaseURL: ts.URL})
	st, err := c.JobStatus(context.Background(), "job-xyz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !st.Found || st.Status != "completed" || st.OutputPath != "/media/Anime/Monster/ep01.mkv" {
		t.Errorf("unexpected status: %+v", st)
	}
}

func TestJobStatus_FailedInReport(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/get-nodes":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{}`))
		case "/api/v2/job-reports/job-err":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{
				"jobId": "job-err",
				"isJobRunning": false,
				"jobReportExists": true,
				"jobRecord": {"status": "error", "error": "hevc_videotoolbox encode error"}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	c := NewClient(ClientOptions{BaseURL: ts.URL})
	st, err := c.JobStatus(context.Background(), "job-err")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !st.Found || st.Status != "failed" || !strings.Contains(st.Error, "error") {
		t.Errorf("unexpected status: %+v", st)
	}
}

func TestCancel_Success(t *testing.T) {
	var cancelledData map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/get-nodes":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{
				"node1": {
					"_id": "node1",
					"workers": {
						"w1": {
							"_id": "w1",
							"file": "/media/Anime/Monster/ep01.mkv",
							"jobId": "job-cancel-test",
							"idle": false
						}
					}
				}
			}`))
		case "/api/v2/cancel-worker-item":
			_ = json.NewDecoder(r.Body).Decode(&cancelledData)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`"Cancelled"`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	c := NewClient(ClientOptions{BaseURL: ts.URL})
	err := c.Cancel(context.Background(), CancelRequest{
		JobId: "job-cancel-test",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, ok := cancelledData["data"].(map[string]any)
	if !ok || data["nodeID"] != "node1" || data["workerID"] != "w1" {
		t.Errorf("unexpected cancel payload: %+v", cancelledData)
	}
}

func TestJobStatus_StaleCompletedIgnoredForNewSubmit(t *testing.T) {
	submitTime := time.Now().Unix()
	serverPath := "/media/Anime/Monster/ep01.mkv"
	libID := "anime_lib_123"

	// 1. Initial State: Stale completed record in FileJSONDB and old job report
	phase := 1
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/get-nodes":
			if phase == 2 {
				// Phase 2: New worker is actively running
				w.Write([]byte(`{
					"node1": {
						"_id": "node1",
						"nodeName": "Davids-M1-Max",
						"workers": {
							"w1": {
								"_id": "w1",
								"file": "/media/Anime/Monster/ep01.mkv",
								"status": "Transcoding",
								"percentage": 30.0,
								"jobId": "new-job-today-789",
								"idle": false
							}
						}
					}
				}`))
				return
			}
			// Phase 1 and 3: No active workers
			w.Write([]byte(`{}`))

		case "/api/v2/cruddb":
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			data := req["data"].(map[string]any)
			if data["collection"] == "FileJSONDB" {
				if phase == 1 {
					// Stale success from yesterday (1 hour before submit)
					w.Write([]byte(fmt.Sprintf(`{
						"TranscodeDecisionMaker": "Success",
						"lastJobReport": "old-job-yesterday-123",
						"outputFile": "/media/transcodes/ep01.mkv",
						"statTime": %d
					}`, submitTime-3600)))
					return
				}
			}
			w.Write([]byte(`{}`))

		case "/api/v2/job-reports/old-job-yesterday-123":
			w.Write([]byte(fmt.Sprintf(`{
				"jobId": "old-job-yesterday-123",
				"jobReportExists": true,
				"isJobRunning": false,
				"createdAt": %d,
				"downloadPath": "/media/transcodes/ep01.mkv",
				"jobRecord": {"status": "success"}
			}`, (submitTime-3600)*1000)))

		case "/api/v2/job-reports/new-job-today-789":
			if phase == 3 {
				// Phase 3: New worker completed!
				w.Write([]byte(fmt.Sprintf(`{
					"jobId": "new-job-today-789",
					"jobReportExists": true,
					"isJobRunning": false,
					"createdAt": %d,
					"downloadPath": "/media/transcodes/ep01.mkv",
					"jobRecord": {"status": "success"}
				}`, (submitTime+20)*1000)))
				return
			}
			http.NotFound(w, r)

		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	c := NewClient(ClientOptions{BaseURL: ts.URL})

	// Submission reference contains SubmittedAt timestamp
	ref := fmt.Sprintf("%s:%d:%s", libID, submitTime, serverPath)

	// Step 1: Query JobStatus before worker starts -> MUST NOT return completed! Must return queued!
	phase = 1
	st1, err := c.JobStatus(context.Background(), ref)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st1.Status == "completed" {
		t.Fatalf("FAILED: JobStatus returned completed on a stale record predating submitTime!")
	}
	if st1.Status != "queued" {
		t.Errorf("expected status queued for stale record, got %s", st1.Status)
	}

	// Step 2: New worker appears on node with real JobID
	phase = 2
	st2, err := c.JobStatus(context.Background(), ref)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st2.Status != "running" {
		t.Errorf("expected status running, got %s", st2.Status)
	}
	if st2.JobId != "new-job-today-789" {
		t.Errorf("expected JobId new-job-today-789, got %s", st2.JobId)
	}

	// Step 3: Worker finishes and new report exists -> returns completed with discovered JobID!
	phase = 3
	refWithJob := fmt.Sprintf("%s:%s:%s", libID, "new-job-today-789", serverPath)
	st3, err := c.JobStatus(context.Background(), refWithJob)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st3.Status != "completed" {
		t.Errorf("expected status completed after new worker finishes, got %s", st3.Status)
	}
	if st3.JobId != "new-job-today-789" {
		t.Errorf("expected JobId new-job-today-789, got %s", st3.JobId)
	}
	if st3.OutputPath != "/media/transcodes/ep01.mkv" {
		t.Errorf("unexpected output path: %s", st3.OutputPath)
	}
}

func TestGetLibrary_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v2/cruddb" {
			w.Write([]byte(`{
				"_id": "lib-anime-test",
				"name": "Anime",
				"folderToFolderConversion": true,
				"folderToFolderConversionDeleteSource": false,
				"outputFolder": "/media/transcodes"
			}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	c := NewClient(ClientOptions{BaseURL: ts.URL})
	lib, err := c.GetLibrary(context.Background(), "lib-anime-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lib.ID != "lib-anime-test" || !lib.FolderToFolderConversion || lib.FolderToFolderConversionDeleteSource || lib.OutputFolder != "/media/transcodes" {
		t.Errorf("unexpected library settings: %+v", lib)
	}
}
