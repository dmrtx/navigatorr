// Package podcast owns the typed, model-independent podcast cleaning contract.
// ASR supplies native times; the caller supplies labels over immutable IDs.
package podcast

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const Version = 1
const PromptVersion = "podcast-labels-v1"
const AnalysisFull = "full"
const AnalysisKnownAdsOnly = "known_ads_only"

type Unit struct {
	ID      string `json:"id"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
	Text    string `json:"text"`
	Timing  string `json:"timing"`
}
type Transcript struct {
	SchemaVersion     int     `json:"schema_version"`
	SourceHash        string  `json:"source_hash"`
	Provider          string  `json:"provider"`
	ProviderVersion   string  `json:"provider_version"`
	Language          string  `json:"language"`
	DurationMS        int64   `json:"duration_ms"`
	WallSeconds       float64 `json:"wall_seconds"`
	RealtimeFactor    float64 `json:"realtime_factor"`
	TimingGranularity string  `json:"timing_granularity"`
	Units             []Unit  `json:"units"`
}
type Policy struct {
	KnownAdsFirstPass  bool     `json:"known_ads_first_pass,omitempty" yaml:"known_ads_first_pass"`
	Language           string   `json:"language" yaml:"language"`
	Remove             []string `json:"remove" yaml:"remove"`
	WindowMS           int64    `json:"window_ms" yaml:"window_ms"`
	OverlapMS          int64    `json:"overlap_ms" yaml:"overlap_ms"`
	MaxRemovedFraction float64  `json:"max_removed_fraction" yaml:"max_removed_fraction"`
	ReviewRequired     bool     `json:"review_required" yaml:"review_required"`
}

func DefaultPolicy() Policy {
	return Policy{Language: "en_US", Remove: []string{"paid_ad"}, WindowMS: 240000, OverlapMS: 30000, MaxRemovedFraction: .35, ReviewRequired: true}
}
func Label(s string) bool {
	return s == "paid_ad" || s == "house_promo" || s == "cross_promo" || s == "content" || s == "uncertain"
}
func (p Policy) Validate() error {
	if p.Language == "" || p.WindowMS < 30000 || p.WindowMS > 600000 || p.OverlapMS < 0 || p.OverlapMS > p.WindowMS/2 || !(p.MaxRemovedFraction > 0 && p.MaxRemovedFraction <= .5) {
		return fmt.Errorf("invalid podcast policy")
	}
	seen := map[string]bool{}
	for _, l := range p.Remove {
		if !Label(l) || l == "content" || l == "uncertain" || seen[l] {
			return fmt.Errorf("invalid removal label %q", l)
		}
		seen[l] = true
	}
	return nil
}

type Block struct {
	ID     string `json:"id"`
	First  int    `json:"first"`
	Last   int    `json:"last"`
	Digest string `json:"digest"`
}
type Decision struct {
	FirstID  string      `json:"first_id"`
	LastID   string      `json:"last_id"`
	Label    string      `json:"label"`
	Reason   string      `json:"reason"`
	Evidence *AdEvidence `json:"evidence,omitempty"`
}
type Classification struct {
	BlockID          string     `json:"block_id"`
	BlockDigest      string     `json:"block_digest"`
	TranscriptDigest string     `json:"transcript_digest"`
	PromptVersion    string     `json:"prompt_version"`
	Model            string     `json:"model"`
	Reasoning        string     `json:"reasoning"`
	Decisions        []Decision `json:"decisions"`
}
type Cut struct {
	FirstID string `json:"first_id"`
	LastID  string `json:"last_id"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
}
type Cuts struct {
	AnalysisMode         string `json:"analysis_mode,omitempty"`
	Version              int    `json:"version"`
	SourceHash           string `json:"source_hash"`
	TranscriptDigest     string `json:"transcript_digest"`
	PolicyDigest         string `json:"policy_digest"`
	ClassificationDigest string `json:"classification_digest"`
	DurationMS           int64  `json:"duration_ms"`
	RemovedMS            int64  `json:"removed_ms"`
	Ranges               []Cut  `json:"ranges"`
}

// Automatic is a deterministic first pass over verified existing references.
// It supplies no LLM classifications or textual review approval.
type AdAutomatic struct {
	Scope  string `json:"scope"`
	Policy Policy `json:"policy"`
}

func (a AdAutomatic) Validate(c Cuts) error {
	if a.Scope == "" || len(a.Scope) > 128 || !a.Policy.KnownAdsFirstPass || c.AnalysisMode != AnalysisKnownAdsOnly || Digest(a.Policy) != c.PolicyDigest {
		return fmt.Errorf("invalid automatic known-ad scope/policy/mode")
	}
	return a.Policy.Validate()
}

// Task travels through the existing worker queue, not a second scheduler.
type Task struct {
	Version    int          `json:"version"`
	Operation  string       `json:"operation"`
	Language   string       `json:"language"`
	ASRJobID   string       `json:"asr_job_id,omitempty"`
	Cuts       *Cuts        `json:"cuts,omitempty"`
	Catalog    *AdCatalog   `json:"catalog,omitempty"`
	Learning   *AdLearning  `json:"learning,omitempty"`
	Automatic  *AdAutomatic `json:"automatic,omitempty"`
	MatchJobID string       `json:"match_job_id,omitempty"`
}
type Result struct {
	Operation        string  `json:"operation"`
	SourceHash       string  `json:"source_hash"`
	TranscriptDigest string  `json:"transcript_digest"`
	DurationMS       int64   `json:"duration_ms"`
	UnitCount        int     `json:"unit_count,omitempty"`
	ASRWallSeconds   float64 `json:"asr_wall_seconds,omitempty"`
	RealtimeFactor   float64 `json:"realtime_factor,omitempty"`
	RemovedMS        int64   `json:"removed_ms,omitempty"`
	OutputDurationMS int64   `json:"output_duration_ms,omitempty"`
	DecodePassed     bool    `json:"decode_passed,omitempty"`
	MatchDigest      string  `json:"match_digest,omitempty"`
	LearnedAds       int     `json:"learned_ads,omitempty"`
}

func Digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

var hashRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func ValidHash(s string) bool { return hashRE.MatchString(s) }
func (t Transcript) Validate() error {
	if t.SchemaVersion != Version || !ValidHash(t.SourceHash) || t.Provider == "" || t.ProviderVersion == "" || t.Language == "" || t.DurationMS <= 0 || t.DurationMS > 43200000 || len(t.Units) == 0 || len(t.Units) > 200000 {
		return fmt.Errorf("invalid transcript identity or size")
	}
	seen := map[string]bool{}
	var prev int64
	for _, u := range t.Units {
		if u.ID == "" || len(u.ID) > 64 || seen[u.ID] || u.Text == "" || len(u.Text) > 4096 || u.StartMS < prev || u.EndMS <= u.StartMS || u.EndMS > t.DurationMS+250 || (u.Timing != "native_attributed_run" && u.Timing != "native_result") {
			return fmt.Errorf("invalid native timed unit %q", u.ID)
		}
		prev = u.EndMS
		seen[u.ID] = true
	}
	return nil
}
func (t Task) Validate() error {
	if t.Version != Version || t.Language == "" {
		return fmt.Errorf("invalid podcast task identity")
	}
	switch t.Operation {
	case "match_ads":
		if t.Catalog == nil || t.Cuts != nil || t.ASRJobID != "" || t.Learning != nil || t.Automatic != nil || t.MatchJobID != "" {
			return fmt.Errorf("matching requires only a frozen catalog")
		}
		return t.Catalog.Validate()
	case "transcribe":
		if t.Cuts != nil || t.ASRJobID != "" || t.Catalog != nil || t.Learning != nil || t.Automatic != nil || t.MatchJobID != "" {
			return fmt.Errorf("transcription cannot include cuts")
		}
	case "render":
		if t.Catalog != nil {
			return fmt.Errorf("render cannot include a catalog")
		}
		if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`).MatchString(t.ASRJobID) || t.Cuts == nil {
			return fmt.Errorf("render requires an ASR job and cuts")
		}
		if err := t.Cuts.Validate(); err != nil {
			return err
		}
		if t.MatchJobID != "" && !regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`).MatchString(t.MatchJobID) {
			return fmt.Errorf("invalid matching job")
		}
		if t.Automatic != nil {
			if t.MatchJobID == "" || t.Learning != nil || !strings.EqualFold(strings.ReplaceAll(t.Language, "-", "_"), strings.ReplaceAll(t.Automatic.Policy.Language, "-", "_")) {
				return fmt.Errorf("automatic known-ad rendering requires matching evidence without classifications")
			}
			return t.Automatic.Validate(*t.Cuts)
		}
		if t.Cuts.AnalysisMode == AnalysisKnownAdsOnly {
			return fmt.Errorf("known-ad cuts require an automatic proof")
		}
		if t.Learning != nil {
			return t.Learning.Validate(*t.Cuts)
		}
	default:
		return fmt.Errorf("unsupported podcast operation")
	}
	return nil
}
func (c Cuts) Validate() error {
	if c.AnalysisMode != "" && c.AnalysisMode != AnalysisFull && c.AnalysisMode != AnalysisKnownAdsOnly {
		return fmt.Errorf("invalid cuts analysis mode")
	}
	if c.Version != Version || !ValidHash(c.SourceHash) || !ValidHash(c.TranscriptDigest) || !ValidHash(c.PolicyDigest) || !ValidHash(c.ClassificationDigest) || c.DurationMS <= 0 || len(c.Ranges) > 1000 {
		return fmt.Errorf("invalid cut identity")
	}
	var prev, removed int64
	for _, r := range c.Ranges {
		if r.FirstID == "" || r.LastID == "" || r.StartMS < prev || r.EndMS <= r.StartMS || r.EndMS > c.DurationMS || r.EndMS-r.StartMS > c.DurationMS/2 {
			return fmt.Errorf("invalid cut bounds")
		}
		prev = r.EndMS
		removed += r.EndMS - r.StartMS
	}
	if removed != c.RemovedMS || removed > c.DurationMS/2 {
		return fmt.Errorf("invalid removed duration")
	}
	return nil
}
