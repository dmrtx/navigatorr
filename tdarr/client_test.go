package tdarr

import (
	"context"
	"encoding/json"
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
