package tdarr

import (
	"context"
	"fmt"
	"time"
)

// ServerStatus represents the response from GET /api/v2/status.
type ServerStatus struct {
	Status       string `json:"status"`
	IsProduction bool   `json:"isProduction"`
	OS           string `json:"os"`
	Version      string `json:"version"`
	BuildDate    string `json:"buildDate"`
	Uptime       int64  `json:"uptime"`
	ServerEngine string `json:"serverEngine"`
}

// PathTranslator maps server paths to node paths.
type PathTranslator struct {
	Server string `json:"server"`
	Node   string `json:"node"`
}

// NodeConfig represents configuration of a connected Tdarr node.
type NodeConfig struct {
	NodeName        string           `json:"nodeName"`
	ServerURL       string           `json:"serverURL"`
	ServerIP        string           `json:"serverIP"`
	ServerPort      string           `json:"serverPort"`
	FFmpegPath      string           `json:"ffmpegPath"`
	MkvpropeditPath string           `json:"mkvpropeditPath"`
	PathTranslators []PathTranslator `json:"pathTranslators"`
	NodeType        string           `json:"nodeType"`
	NodeID          string           `json:"nodeID"`
}

// WorkerItem represents a single worker running on a node.
type WorkerItem struct {
	ID         string  `json:"_id"`
	WorkerType string  `json:"workerType"`
	File       string  `json:"file"`
	Status     string  `json:"status"`
	Percentage float64 `json:"percentage"`
	ETA        string  `json:"ETA"`
	FPS        float64 `json:"fps"`
	JobId      string  `json:"jobId"`
	Idle       bool    `json:"idle"`
}

// Node represents a connected Tdarr node from GET /api/v2/get-nodes.
type Node struct {
	ID            string                `json:"_id"`
	NodeName      string                `json:"nodeName"`
	RemoteAddress string                `json:"remoteAddress"`
	Config        NodeConfig            `json:"config"`
	Workers       map[string]WorkerItem `json:"workers"`
	WorkerLimits  map[string]int        `json:"workerLimits"`
	NodePaused    bool                  `json:"nodePaused"`
}

// ExternalReference represents a structured, realistic reference for a submitted transcode job.
type ExternalReference struct {
	LibraryID   string `json:"library_id"`
	ServerPath  string `json:"server_path"`
	SubmittedAt int64  `json:"submitted_at"`
	JobID       string `json:"job_id,omitempty"`
}

func (r ExternalReference) String() string {
	if r.JobID != "" {
		return fmt.Sprintf("%s:%s:%s", r.LibraryID, r.JobID, r.ServerPath)
	}
	return fmt.Sprintf("%s:%d:%s", r.LibraryID, r.SubmittedAt, r.ServerPath)
}

// SubmitRequest defines the parameters to send a file to Tdarr for transcode.
type SubmitRequest struct {
	FilePath  string `json:"file_path"`
	LibraryID string `json:"library_id"`
	Profile   string `json:"profile,omitempty"`
}

// SubmitResponse holds the result of submitting a job to Tdarr.
type SubmitResponse struct {
	Success     bool              `json:"success"`
	Reference   string            `json:"reference"`
	ExternalRef ExternalReference `json:"external_ref"`
	Message     string            `json:"message,omitempty"`
}

// JobStatusResponse represents the unified state of a transcode job in Tdarr.
type JobStatusResponse struct {
	Found      bool    `json:"found"`
	Status     string  `json:"status"` // "queued", "running", "completed", "failed", "unknown"
	Progress   float64 `json:"progress"`
	ETA        string  `json:"eta,omitempty"`
	FPS        float64 `json:"fps,omitempty"`
	NodeID     string  `json:"node_id,omitempty"`
	WorkerID   string  `json:"worker_id,omitempty"`
	JobId      string  `json:"job_id,omitempty"`
	OutputPath string  `json:"output_path,omitempty"`
	Error      string  `json:"error,omitempty"`
	Details    string  `json:"details,omitempty"`
}

// CancelRequest defines the target of a cancellation operation.
type CancelRequest struct {
	NodeID   string `json:"node_id,omitempty"`
	WorkerID string `json:"worker_id,omitempty"`
	JobId    string `json:"job_id,omitempty"`
	FilePath string `json:"file_path,omitempty"`
	Cause    string `json:"cause,omitempty"`
}

// Client defines the interface for interacting with Tdarr.
type Client interface {
	Status(ctx context.Context) (*ServerStatus, error)
	Nodes(ctx context.Context) (map[string]Node, error)
	Submit(ctx context.Context, req SubmitRequest) (*SubmitResponse, error)
	JobStatus(ctx context.Context, ref string) (*JobStatusResponse, error)
	Cancel(ctx context.Context, req CancelRequest) error
}

// ClientOptions configures a Tdarr Client.
type ClientOptions struct {
	BaseURL string
	APIKey  string
	Timeout time.Duration
}
