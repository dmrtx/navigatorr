package transcode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

type SubtitleAction struct {
	SourceStreamIndex int    `json:"source_stream_index" yaml:"source_stream_index"`
	TypeIndex         int    `json:"type_index" yaml:"type_index"`
	SourceCodec       string `json:"source_codec" yaml:"source_codec"`
	Operation         string `json:"operation" yaml:"operation"`
	Codec             string `json:"codec" yaml:"codec"`
	Reason            string `json:"reason,omitempty" yaml:"reason,omitempty"`
}

type ResiliencePlan struct {
	MaxAttempts         int      `json:"max_attempts" yaml:"max_attempts"`
	TransientRetries    int      `json:"transient_retries" yaml:"transient_retries"`
	RetryBackoffSeconds []int    `json:"retry_backoff_seconds,omitempty" yaml:"retry_backoff_seconds,omitempty"`
	MaxFallbacks        int      `json:"max_fallbacks" yaml:"max_fallbacks"`
	RetryOn             []string `json:"retry_on,omitempty" yaml:"retry_on,omitempty"`
}

type Plan struct {
	Container                    string           `json:"container" yaml:"container"`
	VideoCodec                   string           `json:"video_codec" yaml:"video_codec"`
	Quality                      int              `json:"quality" yaml:"quality"`
	AudioMode                    string           `json:"audio_mode" yaml:"audio_mode"`
	SubtitleMode                 string           `json:"subtitle_mode" yaml:"subtitle_mode"`
	ConvertIncompatibleSubtitles bool             `json:"convert_incompatible_subtitles" yaml:"convert_incompatible_subtitles"`
	PreserveMetadata             bool             `json:"preserve_metadata" yaml:"preserve_metadata"`
	PreserveChapters             bool             `json:"preserve_chapters" yaml:"preserve_chapters"`
	PreserveAttachments          bool             `json:"preserve_attachments" yaml:"preserve_attachments"`
	SubtitleActions              []SubtitleAction `json:"subtitle_actions,omitempty" yaml:"subtitle_actions,omitempty"`
	RecipeVersion                string           `json:"recipe_version,omitempty" yaml:"recipe_version,omitempty"`
	RecipeDigest                 string           `json:"recipe_digest,omitempty" yaml:"recipe_digest,omitempty"`
	PlanDigest                   string           `json:"plan_digest,omitempty" yaml:"plan_digest,omitempty"`
	Resilience                   ResiliencePlan   `json:"resilience,omitempty" yaml:"resilience,omitempty"`
	AppliedFallbacks             []string         `json:"applied_fallbacks,omitempty" yaml:"applied_fallbacks,omitempty"`
}

func DigestPlan(p *Plan) (string, error) {
	if p == nil {
		return "", fmt.Errorf("plan is nil")
	}
	cp := *p
	cp.PlanDigest = ""
	b, err := json.Marshal(cp)
	if err != nil {
		return "", fmt.Errorf("serializing plan digest: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

type ConversionRecord struct {
	StreamType  string `json:"stream_type"`
	StreamIndex int    `json:"stream_index"`
	FromCodec   string `json:"from_codec"`
	ToCodec     string `json:"to_codec"`
	Reason      string `json:"reason"`
}

func ContainerExtension(container string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(container)) {
	case "mkv", "matroska":
		return ".mkv", nil
	default:
		return "", fmt.Errorf("unsupported container %q (fail closed)", container)
	}
}

type Request struct {
	ID            string `json:"id"`
	SourcePath    string `json:"source_path"`
	CandidatePath string `json:"candidate_path"`
	Profile       string `json:"profile"`
	Plan          *Plan  `json:"plan,omitempty"`
}
type Job struct {
	ID string `json:"id"`
}
type JobStatus struct {
	ID                    string             `json:"id"`
	Status                string             `json:"status"`
	Progress              float64            `json:"progress"`
	FPS                   float64            `json:"fps"`
	Speed                 float64            `json:"speed"`
	CandidatePath         string             `json:"candidate_path"`
	Error                 string             `json:"error,omitempty"`
	Profile               string             `json:"profile,omitempty"`
	RecipeVersion         string             `json:"recipe_version,omitempty"`
	RecipeDigest          string             `json:"recipe_digest,omitempty"`
	Container             string             `json:"container,omitempty"`
	VideoCodec            string             `json:"video_codec,omitempty"`
	Quality               int                `json:"quality,omitempty"`
	Attempt               int                `json:"attempt,omitempty"`
	RetryCount            int                `json:"retry_count,omitempty"`
	FallbackCount         int                `json:"fallback_count,omitempty"`
	AppliedFallbacks      []string           `json:"applied_fallbacks,omitempty"`
	FailureClassification string             `json:"failure_classification,omitempty"`
	Conversions           []ConversionRecord `json:"conversions,omitempty"`
}

type PathMapping struct {
	Local  string `json:"local" yaml:"local"`
	Remote string `json:"remote" yaml:"remote"`
}
type SSHConfig struct {
	Host           string        `json:"host" yaml:"host"`
	Port           int           `json:"port,omitempty" yaml:"port,omitempty"`
	User           string        `json:"user" yaml:"user"`
	Command        string        `json:"command" yaml:"command"`
	IdentityFile   string        `json:"identity_file" yaml:"identity_file"`
	KnownHostsFile string        `json:"known_hosts_file,omitempty" yaml:"known_hosts_file,omitempty"`
	ConnectTimeout time.Duration `json:"connect_timeout" yaml:"connect_timeout"`
	CommandTimeout time.Duration `json:"command_timeout,omitempty" yaml:"command_timeout,omitempty"`
	PathMappings   []PathMapping `json:"path_mappings" yaml:"path_mappings"`
}
