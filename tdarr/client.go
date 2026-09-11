package tdarr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPError represents an unexpected HTTP error from Tdarr.
type HTTPError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("tdarr api error: %d %s: %s", e.StatusCode, e.Status, e.Body)
}

type client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewClient creates a new Tdarr API client.
func NewClient(opts ClientOptions) Client {
	u := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if u == "" {
		u = "http://localhost:8265"
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "http://" + u
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	return &client{
		baseURL: u,
		apiKey:  strings.TrimSpace(opts.APIKey),
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

func (c *client) doJSON(ctx context.Context, method, path string, bodyIn any, out any) error {
	fullURL := c.baseURL + path

	var reqBody io.Reader
	if bodyIn != nil {
		data, err := json.Marshal(bodyIn)
		if err != nil {
			return fmt.Errorf("serializing tdarr request: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, reqBody)
	if err != nil {
		return fmt.Errorf("creating tdarr request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	if bodyIn != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if c.apiKey != "" {
		req.Header.Set("x-api-key", c.apiKey)
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing tdarr request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading tdarr response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       string(respBytes),
		}
	}

	if out != nil && len(respBytes) > 0 {
		if err := json.Unmarshal(respBytes, out); err != nil {
			return fmt.Errorf("unmarshaling tdarr response: %w (raw: %s)", err, string(respBytes))
		}
	}

	return nil
}

// Status returns server health and version metadata via GET /api/v2/status.
func (c *client) Status(ctx context.Context) (*ServerStatus, error) {
	var status ServerStatus
	if err := c.doJSON(ctx, http.MethodGet, "/api/v2/status", nil, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// Nodes returns all connected nodes via GET /api/v2/get-nodes.
func (c *client) Nodes(ctx context.Context) (map[string]Node, error) {
	var nodes map[string]Node
	if err := c.doJSON(ctx, http.MethodGet, "/api/v2/get-nodes", nil, &nodes); err != nil {
		return nil, err
	}
	return nodes, nil
}

// Submit sends a file to Tdarr to be scanned and queued for transcode.
func (c *client) Submit(ctx context.Context, req SubmitRequest) (*SubmitResponse, error) {
	filePath := strings.TrimSpace(req.FilePath)
	if filePath == "" {
		return nil, errors.New("file_path is required")
	}

	libID := strings.TrimSpace(req.LibraryID)
	if libID == "" {
		libID = deriveLibraryID(filePath)
	}

	payload := map[string]any{
		"data": map[string]any{
			"scanConfig": map[string]any{
				"dbID":        libID,
				"mode":        "scanFolderWatcher",
				"arrayOrPath": []string{filePath},
			},
		},
	}

	if err := c.doJSON(ctx, http.MethodPost, "/api/v2/scan-files", payload, nil); err != nil {
		return nil, fmt.Errorf("submitting file to tdarr: %w", err)
	}

	return &SubmitResponse{
		Success:   true,
		Reference: filePath,
		Message:   "Queued in Tdarr via scanFolderWatcher",
	}, nil
}

// JobStatus queries the progress of a job by checking live workers, job reports, staged items, and db.
func (c *client) JobStatus(ctx context.Context, ref string) (*JobStatusResponse, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("reference (job ID or file path) is required")
	}

	// 1. Check live workers on connected nodes
	nodes, err := c.Nodes(ctx)
	if err == nil {
		for nodeID, node := range nodes {
			for workerID, w := range node.Workers {
				if w.Idle {
					continue
				}
				if matchRef(w, ref) {
					return &JobStatusResponse{
						Found:      true,
						Status:     "running",
						Progress:   w.Percentage,
						ETA:        w.ETA,
						FPS:        w.FPS,
						NodeID:     nodeID,
						WorkerID:   workerID,
						JobId:      w.JobId,
						OutputPath: w.File,
						Details:    w.Status,
					}, nil
				}
			}
		}
	}

	// 2. Check job report by ID if ref looks like a jobId (or try querying)
	var report struct {
		JobId           string         `json:"jobId"`
		Filename        string         `json:"filename"`
		DownloadPath    string         `json:"downloadPath"`
		IsJobRunning    bool           `json:"isJobRunning"`
		JobReportExists bool           `json:"jobReportExists"`
		Job             map[string]any `json:"job"`
		JobRecord       map[string]any `json:"jobRecord"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/v2/job-reports/"+ref, nil, &report); err == nil && report.JobReportExists {
		if report.IsJobRunning {
			return &JobStatusResponse{
				Found:      true,
				Status:     "running",
				JobId:      report.JobId,
				OutputPath: report.DownloadPath,
				Details:    "Job report shows job is currently running",
			}, nil
		}
		// Check if failed
		if isJobRecordFailed(report.JobRecord) {
			return &JobStatusResponse{
				Found:   true,
				Status:  "failed",
				JobId:   report.JobId,
				Error:   "Transcode job marked as error in job report",
				Details: fmt.Sprintf("%v", report.JobRecord["error"]),
			}, nil
		}
		return &JobStatusResponse{
			Found:      true,
			Status:     "completed",
			Progress:   100,
			JobId:      report.JobId,
			OutputPath: report.DownloadPath,
			Details:    "Job completed according to job report",
		}, nil
	}

	// 3. Check FileJSONDB via cruddb
	var fileDoc map[string]any
	crudPayload := map[string]any{
		"data": map[string]any{
			"collection": "FileJSONDB",
			"mode":       "getById",
			"docID":      ref,
		},
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api/v2/cruddb", crudPayload, &fileDoc); err == nil && fileDoc != nil && len(fileDoc) > 0 {
		statusStr, _ := fileDoc["status"].(string)
		lowerStatus := strings.ToLower(statusStr)
		switch {
		case lowerStatus == "queued" || lowerStatus == "queue":
			return &JobStatusResponse{
				Found:   true,
				Status:  "queued",
				Details: "File is queued in Tdarr database",
			}, nil
		case strings.Contains(lowerStatus, "error") || strings.Contains(lowerStatus, "fail"):
			return &JobStatusResponse{
				Found:   true,
				Status:  "failed",
				Error:   statusStr,
				Details: "File marked failed in Tdarr database",
			}, nil
		case strings.Contains(lowerStatus, "transcod") || strings.Contains(lowerStatus, "success") || strings.Contains(lowerStatus, "complete"):
			return &JobStatusResponse{
				Found:      true,
				Status:     "completed",
				Progress:   100,
				OutputPath: ref,
				Details:    "File processed according to Tdarr database",
			}, nil
		}
	}

	// 4. Check staged items via client/staged
	var stagedResp struct {
		Array []map[string]any `json:"array"`
	}
	stagedPayload := map[string]any{
		"data": map[string]any{
			"start":    0,
			"pageSize": 20,
			"filters":  []any{},
			"sorts":    []any{},
			"opts":     map[string]any{},
		},
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api/v2/client/staged", stagedPayload, &stagedResp); err == nil {
		for _, item := range stagedResp.Array {
			file, _ := item["file"].(string)
			if file == ref || strings.HasSuffix(file, ref) || strings.HasSuffix(ref, file) {
				cacheFile, _ := item["cacheFile"].(string)
				outputPath := cacheFile
				if outputPath == "" {
					outputPath = file
				}
				return &JobStatusResponse{
					Found:      true,
					Status:     "completed",
					Progress:   100,
					OutputPath: outputPath,
					Details:    "Transcoded file staged and ready for review",
				}, nil
			}
		}
	}

	return &JobStatusResponse{
		Found:   false,
		Status:  "unknown",
		Details: "No active worker, report, or staged record found for reference",
	}, nil
}

// Cancel cancels a running transcode job on a node.
func (c *client) Cancel(ctx context.Context, req CancelRequest) error {
	nodeID := strings.TrimSpace(req.NodeID)
	workerID := strings.TrimSpace(req.WorkerID)
	cause := strings.TrimSpace(req.Cause)
	if cause == "" {
		cause = "user"
	}

	// If nodeID or workerID is missing, look them up from active workers
	if nodeID == "" || workerID == "" {
		nodes, err := c.Nodes(ctx)
		if err != nil {
			return fmt.Errorf("finding active worker to cancel: %w", err)
		}
		target := req.JobId
		if target == "" {
			target = req.FilePath
		}
		if target == "" {
			return errors.New("node_id + worker_id, or job_id, or file_path is required to cancel")
		}

		found := false
		for nID, node := range nodes {
			for wID, w := range node.Workers {
				if matchRef(w, target) {
					nodeID = nID
					workerID = wID
					found = true
					break
				}
			}
			if found {
				break
			}
		}

		if !found {
			return fmt.Errorf("no running worker found matching %q to cancel", target)
		}
	}

	payload := map[string]any{
		"data": map[string]any{
			"nodeID":   nodeID,
			"workerID": workerID,
			"cause":    cause,
		},
	}

	if err := c.doJSON(ctx, http.MethodPost, "/api/v2/cancel-worker-item", payload, nil); err != nil {
		return fmt.Errorf("cancelling tdarr worker item: %w", err)
	}

	return nil
}

// Helpers

func deriveLibraryID(filePath string) string {
	parts := strings.Split(strings.TrimPrefix(filePath, "/"), "/")
	if len(parts) >= 2 && parts[0] == "media" {
		return parts[1] // e.g. "Anime", "Movies", "TvSeries"
	}
	if len(parts) >= 1 {
		return parts[0]
	}
	return "default"
}

func matchRef(w WorkerItem, ref string) bool {
	if w.JobId != "" && w.JobId == ref {
		return true
	}
	if w.File != "" {
		if w.File == ref || strings.HasSuffix(w.File, ref) || strings.HasSuffix(ref, w.File) {
			return true
		}
	}
	return false
}

func isJobRecordFailed(record map[string]any) bool {
	if record == nil {
		return false
	}
	if status, ok := record["status"].(string); ok {
		lower := strings.ToLower(status)
		if strings.Contains(lower, "error") || strings.Contains(lower, "fail") {
			return true
		}
	}
	if errVal, ok := record["error"]; ok && errVal != nil && errVal != "" {
		return true
	}
	return false
}
