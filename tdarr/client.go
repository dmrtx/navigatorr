package tdarr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
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

// GetLibrary retrieves library configuration from LibrarySettingsJSONDB by library ID.
func (c *client) GetLibrary(ctx context.Context, libraryID string) (*LibrarySettings, error) {
	libraryID = strings.TrimSpace(libraryID)
	if libraryID == "" {
		return nil, errors.New("library_id is required")
	}

	crudPayload := map[string]any{
		"data": map[string]any{
			"collection": "LibrarySettingsJSONDB",
			"mode":       "getById",
			"docID":      libraryID,
		},
	}

	var lib LibrarySettings
	if err := c.doJSON(ctx, http.MethodPost, "/api/v2/cruddb", crudPayload, &lib); err != nil {
		return nil, fmt.Errorf("fetching library settings from tdarr: %w", err)
	}

	if lib.ID == "" {
		return nil, fmt.Errorf("tdarr library %q not found in LibrarySettingsJSONDB", libraryID)
	}

	return &lib, nil
}

// Submit sends a file to Tdarr to be scanned and queued for transcode.
func (c *client) Submit(ctx context.Context, req SubmitRequest) (*SubmitResponse, error) {
	filePath := strings.TrimSpace(req.FilePath)
	if filePath == "" {
		return nil, errors.New("file_path is required")
	}

	libID := strings.TrimSpace(req.LibraryID)
	if libID == "" {
		return nil, errors.New("library_id is required (must match a valid Tdarr library dbID)")
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

	extRef := ExternalReference{
		LibraryID:   libID,
		ServerPath:  filePath,
		SubmittedAt: time.Now().Unix(),
	}

	return &SubmitResponse{
		Success:     true,
		Reference:   extRef.String(),
		ExternalRef: extRef,
		Message:     "Queued in Tdarr via scanFolderWatcher",
	}, nil
}

// JobStatus queries the progress of a job by checking live workers, job reports, staged items, and db.
func (c *client) JobStatus(ctx context.Context, ref string) (*JobStatusResponse, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("reference is required")
	}

	ext := ParseExternalReference(ref)

	// 1. Check live workers on connected nodes (exact path or job ID match)
	nodes, err := c.Nodes(ctx)
	if err == nil {
		for nodeID, node := range nodes {
			for workerID, w := range node.Workers {
				if w.Idle {
					continue
				}
				matched := false
				if ext.JobID != "" && w.JobId == ext.JobID {
					matched = true
				} else if ext.ServerPath != "" && w.File != "" && filepath.Clean(w.File) == filepath.Clean(ext.ServerPath) {
					matched = true
				}
				if matched {
					outPath := w.File
					return &JobStatusResponse{
						Found:      true,
						Status:     "running",
						Progress:   w.Percentage,
						ETA:        w.ETA,
						FPS:        w.FPS,
						NodeID:     nodeID,
						WorkerID:   workerID,
						JobId:      w.JobId,
						OutputPath: outPath,
						Details:    w.Status,
					}, nil
				}
			}
		}
	}

	// 2. Check job report by ID ONLY if ext.JobID is a real job ID (never a file path)
	if ext.JobID != "" && !strings.Contains(ext.JobID, "/") {
		var report struct {
			JobId           string         `json:"jobId"`
			Filename        string         `json:"filename"`
			DownloadPath    string         `json:"downloadPath"`
			IsJobRunning    bool           `json:"isJobRunning"`
			JobReportExists bool           `json:"jobReportExists"`
			Job             map[string]any `json:"job"`
			JobRecord       map[string]any `json:"jobRecord"`
			CreatedAt       any            `json:"createdAt"`
		}
		if err := c.doJSON(ctx, http.MethodGet, "/api/v2/job-reports/"+ext.JobID, nil, &report); err == nil && report.JobReportExists {
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

			// If report has timestamp predating current submission, do not consider completed
			repTime := extractTimestamp(report.CreatedAt)
			if repTime == 0 && report.JobRecord != nil {
				repTime = extractTimestamp(report.JobRecord["createdAt"])
			}
			if repTime == 0 && report.Job != nil {
				repTime = extractTimestamp(report.Job["createdAt"])
			}
			if ext.SubmittedAt > 0 && repTime > 0 && repTime < ext.SubmittedAt-5 {
				return &JobStatusResponse{
					Found:   true,
					Status:  "queued",
					Details: "Prior job report predates current submission; waiting for new transcode",
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
	}

	// 3. Check FileJSONDB via cruddb if server path is known
	serverPath := ext.ServerPath
	if serverPath == "" && strings.HasPrefix(ref, "/") {
		serverPath = ref
	}
	if serverPath != "" {
		var fileDoc map[string]any
		crudPayload := map[string]any{
			"data": map[string]any{
				"collection": "FileJSONDB",
				"mode":       "getById",
				"docID":      serverPath,
			},
		}
		if err := c.doJSON(ctx, http.MethodPost, "/api/v2/cruddb", crudPayload, &fileDoc); err == nil && fileDoc != nil && len(fileDoc) > 0 {
			decision, _ := fileDoc["TranscodeDecisionMaker"].(string)
			if decision == "" {
				decision, _ = fileDoc["status"].(string)
			}
			lower := strings.ToLower(decision)
			switch {
			case lower == "queued" || lower == "queue":
				return &JobStatusResponse{
					Found:   true,
					Status:  "queued",
					Details: "File is queued in Tdarr database",
				}, nil
			case strings.Contains(lower, "error") || strings.Contains(lower, "fail"):
				return &JobStatusResponse{
					Found:   true,
					Status:  "failed",
					Error:   decision,
					Details: "File marked failed in Tdarr database",
				}, nil
			case strings.Contains(lower, "success") || strings.Contains(lower, "complete"):
				outPath, _ := fileDoc["outputFile"].(string)
				if outPath == "" {
					outPath, _ = fileDoc["outputFilePath"].(string)
				}
				jobId, _ := fileDoc["lastJobReport"].(string)

				// Determine whether this completion is from before ext.SubmittedAt
				if ext.SubmittedAt > 0 {
					var recordTime int64
					for _, key := range []string{"statTime", "scanTime", "mtime", "updatedAt", "createdAt"} {
						if ts := extractTimestamp(fileDoc[key]); ts > 0 {
							recordTime = ts
							break
						}
					}

					// If lastJobReport is present, check its createdAt timestamp
					if jobId != "" && !strings.Contains(jobId, "/") {
						var rep struct {
							CreatedAt any            `json:"createdAt"`
							JobRecord map[string]any `json:"jobRecord"`
							Job       map[string]any `json:"job"`
						}
						if err := c.doJSON(ctx, http.MethodGet, "/api/v2/job-reports/"+jobId, nil, &rep); err == nil {
							if rts := extractTimestamp(rep.CreatedAt); rts > 0 {
								recordTime = rts
							} else if rep.JobRecord != nil {
								if rts := extractTimestamp(rep.JobRecord["createdAt"]); rts > 0 {
									recordTime = rts
								}
							} else if rep.Job != nil {
								if rts := extractTimestamp(rep.Job["createdAt"]); rts > 0 {
									recordTime = rts
								}
							}
						}
					}

					// If evidence proves this record is older than our submission (with 5s grace):
					if recordTime > 0 && recordTime < ext.SubmittedAt-5 {
						return &JobStatusResponse{
							Found:   true,
							Status:  "queued",
							Details: "Prior transcode record predates current submission; file is queued for new transcode",
						}, nil
					}

					// If we have not yet observed a worker for this submission (ext.JobID == ""),
					// and have no record timestamp proving completion after SubmittedAt, do not assume completed!
					if ext.JobID == "" && (recordTime == 0 || recordTime < ext.SubmittedAt-5) {
						return &JobStatusResponse{
							Found:   true,
							Status:  "queued",
							Details: "Newly submitted file awaiting worker transcode; prior record is not from this submission",
						}, nil
					}

					// If ext.JobID was captured from a worker of this submission, but fileDoc still points to an older lastJobReport:
					if ext.JobID != "" && jobId != "" && ext.JobID != jobId {
						return &JobStatusResponse{
							Found:   true,
							Status:  "queued",
							Details: "File in database has not updated to current job yet",
						}, nil
					}
				}

				return &JobStatusResponse{
					Found:      true,
					Status:     "completed",
					Progress:   100,
					JobId:      jobId,
					OutputPath: outPath,
					Details:    "File processed successfully according to Tdarr database",
				}, nil
			case strings.Contains(lower, "not required"):
				return &JobStatusResponse{
					Found:   true,
					Status:  "failed",
					Error:   "transcode not required by Tdarr library rules",
					Details: "File skipped as transcode not required",
				}, nil
			}
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
			if serverPath != "" && filepath.Clean(file) == filepath.Clean(serverPath) {
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

// ParseExternalReference parses a reference string into an ExternalReference.
func ParseExternalReference(ref string) ExternalReference {
	var ext ExternalReference
	if err := json.Unmarshal([]byte(ref), &ext); err == nil && (ext.ServerPath != "" || ext.JobID != "") {
		return ext
	}
	if strings.Contains(ref, ":") {
		parts := strings.Split(ref, ":")
		if len(parts) >= 3 {
			ext.LibraryID = parts[0]
			second := parts[1]
			ext.ServerPath = strings.Join(parts[2:], ":")
			if ts, err := strconv.ParseInt(second, 10, 64); err == nil && ts > 0 {
				ext.SubmittedAt = ts
			} else {
				ext.JobID = second
			}
			return ext
		}
	}
	if strings.HasPrefix(ref, "/") {
		ext.ServerPath = ref
	} else {
		ext.JobID = ref
	}
	return ext
}

func matchRef(w WorkerItem, target string) bool {
	if target == "" {
		return false
	}
	if w.JobId != "" && w.JobId == target {
		return true
	}
	if w.File != "" && filepath.Clean(w.File) == filepath.Clean(target) {
		return true
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

func extractTimestamp(val any) int64 {
	if val == nil {
		return 0
	}
	switch v := val.(type) {
	case float64:
		ts := int64(v)
		if ts > 1e11 {
			return ts / 1000
		}
		return ts
	case int64:
		if v > 1e11 {
			return v / 1000
		}
		return v
	case int:
		return int64(v)
	case string:
		v = strings.TrimSpace(v)
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			if n > 1e11 {
				return n / 1000
			}
			return n
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t.Unix()
		}
	}
	return 0
}
