package store

import (
	"fmt"
	"strings"
)

// TranscodeOutcome is one finished size-mode search and, once the full encode
// has been measured, its final validation. Rows outlive the action that wrote
// them: they are the library a later search of the same or a sibling source
// starts from. They hold no media path; FolderKey is a digest of the source's
// directory, so siblings without library ids can still be grouped.
type TranscodeOutcome struct {
	ActionID       string `json:"action_id"`
	BenchmarkJobID string `json:"benchmark_job_id"`

	SourceSHA256 string  `json:"source_sha256"`
	SourceSize   int64   `json:"source_size"`
	DurationSec  float64 `json:"duration_sec"`
	VideoCodec   string  `json:"video_codec"`
	BitDepth     int     `json:"bit_depth"`
	Width        int     `json:"width"`
	Height       int     `json:"height"`
	FPS          float64 `json:"fps"`

	// LibraryService is sonarr, radarr or filesystem; LibraryID is the series
	// or movie id. Season and Episode are -1 when unknown.
	LibraryService string `json:"library_service,omitempty"`
	LibraryID      int64  `json:"library_id,omitempty"`
	Season         int    `json:"season"`
	Episode        int    `json:"episode"`
	FolderKey      string `json:"folder_key,omitempty"`

	Mode          string `json:"mode"`
	PolicyVersion string `json:"policy_version"`
	PolicyDigest  string `json:"policy_digest"`

	// Outcome is winner, no_winner or search_budget_exhausted.
	Outcome string `json:"outcome"`
	// ReusedFrom names the action whose recorded winner this one applied
	// instead of searching.
	ReusedFrom string `json:"reused_from,omitempty"`
	// WinnerJSON, PointsJSON and FinalJSON are owned by the action package.
	WinnerJSON string `json:"winner_json,omitempty"`
	PointsJSON string `json:"points_json,omitempty"`
	// FinalVerdict is pass or fail once final validation measured the full
	// encode, and empty before.
	FinalVerdict string `json:"final_verdict,omitempty"`
	FinalJSON    string `json:"final_json,omitempty"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Outcome values.
const (
	OutcomeWinner          = "winner"
	OutcomeNoWinner        = "no_winner"
	OutcomeBudgetExhausted = "search_budget_exhausted"
)

const transcodeOutcomeColumns = `action_id, benchmark_job_id, source_sha256, source_size, duration_sec, video_codec, bit_depth, width, height, fps,
	library_service, library_id, season, episode, folder_key, mode, policy_version, policy_digest, outcome, reused_from,
	winner_json, points_json, final_verdict, final_json, created_at, updated_at`

// RecordTranscodeOutcome stores a search's result. Recording the same search
// again (a restarted wait step) replaces its search fields and keeps its final
// validation and creation time.
func (s *Store) RecordTranscodeOutcome(o TranscodeOutcome) error {
	if o.ActionID == "" || o.BenchmarkJobID == "" || o.SourceSHA256 == "" || o.PolicyDigest == "" {
		return fmt.Errorf("action, benchmark job, source digest and policy digest are required")
	}
	switch o.Outcome {
	case OutcomeWinner, OutcomeNoWinner, OutcomeBudgetExhausted:
	default:
		return fmt.Errorf("unknown outcome %q", o.Outcome)
	}
	now := nowStr()
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO transcode_outcomes (`+transcodeOutcomeColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', ?, ?)
		ON CONFLICT(action_id, benchmark_job_id) DO UPDATE SET
			source_sha256=excluded.source_sha256, source_size=excluded.source_size, duration_sec=excluded.duration_sec,
			video_codec=excluded.video_codec, bit_depth=excluded.bit_depth, width=excluded.width, height=excluded.height, fps=excluded.fps,
			library_service=excluded.library_service, library_id=excluded.library_id, season=excluded.season, episode=excluded.episode,
			folder_key=excluded.folder_key, mode=excluded.mode, policy_version=excluded.policy_version, policy_digest=excluded.policy_digest,
			outcome=excluded.outcome, reused_from=excluded.reused_from, winner_json=excluded.winner_json, points_json=excluded.points_json,
			updated_at=excluded.updated_at`,
		o.ActionID, o.BenchmarkJobID, o.SourceSHA256, o.SourceSize, o.DurationSec, o.VideoCodec, o.BitDepth, o.Width, o.Height, o.FPS,
		o.LibraryService, o.LibraryID, o.Season, o.Episode, o.FolderKey, o.Mode, o.PolicyVersion, o.PolicyDigest, o.Outcome, o.ReusedFrom,
		o.WinnerJSON, o.PointsJSON, now, now)
	return err
}

// RecordTranscodeOutcomeFinal attaches final validation to a recorded search.
// It reports whether that search was recorded.
func (s *Store) RecordTranscodeOutcomeFinal(actionID, benchmarkJobID, verdict, finalJSON string) (bool, error) {
	if verdict != "pass" && verdict != "fail" {
		return false, fmt.Errorf("final verdict must be pass or fail")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE transcode_outcomes SET final_verdict=?, final_json=?, updated_at=? WHERE action_id=? AND benchmark_job_id=?`,
		verdict, finalJSON, nowStr(), actionID, benchmarkJobID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// TranscodeOutcomesForSource returns the recorded searches of these source
// bytes under this frozen policy, newest first, excluding one action.
func (s *Store) TranscodeOutcomesForSource(sourceSHA, policyDigest, excludeActionID string, limit int) ([]TranscodeOutcome, error) {
	if sourceSHA == "" || policyDigest == "" || limit <= 0 {
		return nil, nil
	}
	return s.queryTranscodeOutcomes(`WHERE source_sha256=? AND policy_digest=? AND action_id != ? ORDER BY created_at DESC, rowid DESC LIMIT ?`,
		sourceSHA, policyDigest, excludeActionID, limit)
}

// SiblingQuery selects the recorded searches of a source's siblings: the same
// library series or movie (and season, when set), or else the same folder.
type SiblingQuery struct {
	LibraryService string
	LibraryID      int64
	// Season is -1 for any season.
	Season    int
	FolderKey string
	// ExcludeSHA256 leaves out the source itself.
	ExcludeSHA256 string
	PolicyVersion string
	Limit         int
}

// SiblingTranscodeOutcomes returns sibling searches, newest first. A query with
// a library id matches by library; one without matches by folder.
func (s *Store) SiblingTranscodeOutcomes(q SiblingQuery) ([]TranscodeOutcome, error) {
	if q.Limit <= 0 {
		return nil, nil
	}
	var where []string
	var args []any
	switch {
	case q.LibraryService != "" && q.LibraryID > 0:
		where = append(where, "library_service=?", "library_id=?")
		args = append(args, q.LibraryService, q.LibraryID)
		if q.Season >= 0 {
			where = append(where, "season=?")
			args = append(args, q.Season)
		}
	case q.FolderKey != "":
		where = append(where, "folder_key=?")
		args = append(args, q.FolderKey)
	default:
		return nil, nil
	}
	if q.ExcludeSHA256 != "" {
		where = append(where, "source_sha256 != ?")
		args = append(args, q.ExcludeSHA256)
	}
	if q.PolicyVersion != "" {
		where = append(where, "policy_version=?")
		args = append(args, q.PolicyVersion)
	}
	args = append(args, q.Limit)
	return s.queryTranscodeOutcomes(`WHERE `+strings.Join(where, " AND ")+` ORDER BY created_at DESC, rowid DESC LIMIT ?`, args...)
}

func (s *Store) queryTranscodeOutcomes(tail string, args ...any) ([]TranscodeOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT `+transcodeOutcomeColumns+` FROM transcode_outcomes `+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TranscodeOutcome
	for rows.Next() {
		var o TranscodeOutcome
		if err := rows.Scan(&o.ActionID, &o.BenchmarkJobID, &o.SourceSHA256, &o.SourceSize, &o.DurationSec, &o.VideoCodec, &o.BitDepth, &o.Width, &o.Height, &o.FPS,
			&o.LibraryService, &o.LibraryID, &o.Season, &o.Episode, &o.FolderKey, &o.Mode, &o.PolicyVersion, &o.PolicyDigest, &o.Outcome, &o.ReusedFrom,
			&o.WinnerJSON, &o.PointsJSON, &o.FinalVerdict, &o.FinalJSON, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
