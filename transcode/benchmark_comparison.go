package transcode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Frame pairs are extracted from the same normalized sample frame. This is a
// visual aid, not a replacement for the quality measurements or approval.
type BenchmarkComparisonFrame struct {
	SampleIndex   int      `json:"sample_index"`
	FrameIndex    int      `json:"frame_index"`
	SourceSeconds float64  `json:"source_seconds"`
	Width         int      `json:"width"`
	Height        int      `json:"height"`
	VMAF          *float64 `json:"vmaf,omitempty"`
	SSIM          *float64 `json:"ssim,omitempty"`
}
type BenchmarkComparison struct {
	ProtocolVersion int                        `json:"protocol_version"`
	CandidateID     string                     `json:"candidate_id"`
	CapturedAt      time.Time                  `json:"captured_at"`
	ExpiresAt       time.Time                  `json:"expires_at"`
	Frames          []BenchmarkComparisonFrame `json:"frames"`
}

func (e *HTTPExecutor) comparisonGET(ctx context.Context, id, query string) ([]byte, http.Header, error) {
	if err := ValidateBenchmarkJobID(id); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, e.reqTO)
	defer cancel()
	path := "/v1/benchmarks/" + url.PathEscape(id) + "/comparison" + query
	req, err := e.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, nil, err
	}
	data, code, headers, err := e.do(req, "benchmark_comparison", id, false)
	if err != nil {
		return nil, nil, err
	}
	if code != http.StatusOK {
		return nil, nil, &HTTPError{Method: http.MethodGet, URL: redactURL(e.base + path), StatusCode: code, Message: parseErrorMessage(data)}
	}
	return data, headers, nil
}
func (e *HTTPExecutor) BenchmarkComparison(ctx context.Context, id string) (BenchmarkComparison, error) {
	var comparison BenchmarkComparison
	data, _, err := e.comparisonGET(ctx, id, "")
	if err != nil {
		return comparison, err
	}
	if err = json.Unmarshal(data, &comparison); err != nil {
		return comparison, fmt.Errorf("invalid comparison response")
	}
	if comparison.ProtocolVersion != WorkerProtocolVersion || len(comparison.Frames) == 0 || len(comparison.Frames) > 3 {
		return comparison, fmt.Errorf("invalid comparison manifest")
	}
	return comparison, nil
}
func (e *HTTPExecutor) BenchmarkComparisonImage(ctx context.Context, id string, index int, side string) ([]byte, error) {
	if index < 0 || index > 100 || (side != "original" && side != "candidate") {
		return nil, fmt.Errorf("invalid comparison image")
	}
	data, headers, err := e.comparisonGET(ctx, id, "?"+url.Values{"image": {strconv.Itoa(index)}, "side": {side}}.Encode())
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(headers.Get("Content-Type"), "image/png") || len(data) < 8 || string(data[:8]) != "\x89PNG\r\n\x1a\n" {
		return nil, fmt.Errorf("invalid comparison image response")
	}
	return data, nil
}
