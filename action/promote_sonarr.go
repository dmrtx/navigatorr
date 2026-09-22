package action

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/arrservice"
)

type promotionEpisode struct {
	ID            int `json:"id"`
	SeriesID      int `json:"seriesId"`
	EpisodeFileID int `json:"episodeFileId"`
}
type promotionFile struct {
	ID           int             `json:"id"`
	SeriesID     int             `json:"seriesId"`
	Path         string          `json:"path"`
	RelativePath string          `json:"relativePath"`
	Size         int64           `json:"size"`
	Quality      json.RawMessage `json:"quality"`
	Languages    json.RawMessage `json:"languages"`
	ReleaseGroup string          `json:"releaseGroup"`
	MediaInfo    struct {
		VideoCodec string `json:"videoCodec"`
	} `json:"mediaInfo"`
}
type promotionLibrary struct {
	Episodes []promotionEpisode
	Files    []promotionFile
}

func (e *Engine) promotionSnapshot(ctx context.Context, svc *arrservice.Service, p *promotionState) (*promotionLibrary, error) {
	snap := new(promotionLibrary)
	query := map[string]string{"seriesId": strconv.Itoa(p.SeriesID)}
	data, err := svc.Get(ctx, "/api/v3/episode", query)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &snap.Episodes); err != nil {
		return nil, err
	}
	data, err = svc.Get(ctx, "/api/v3/episodefile", query)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &snap.Files); err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	for _, ep := range snap.Episodes {
		if ep.ID <= 0 || ep.SeriesID != p.SeriesID || seen[ep.ID] {
			return nil, fmt.Errorf("invalid or duplicate Sonarr episode response")
		}
		seen[ep.ID] = true
	}
	seen = map[int]bool{}
	for i, f := range snap.Files {
		if f.ID <= 0 || f.SeriesID != p.SeriesID || seen[f.ID] {
			return nil, fmt.Errorf("invalid or duplicate Sonarr episodeFile response")
		}
		seen[f.ID] = true
		if f.Path == "" && f.RelativePath != "" {
			snap.Files[i].Path = filepath.Join(p.SeriesPath, f.RelativePath)
		}
		if !withinPromotionPath(p.SeriesPath, snap.Files[i].Path) {
			return nil, fmt.Errorf("Sonarr returned an episodeFile outside the selected series")
		}
	}
	return snap, nil
}

func promotionExpectedIDs(p *promotionState) map[int]bool {
	ids := make(map[int]bool, len(p.EpisodeIDs))
	for _, id := range p.EpisodeIDs {
		ids[id] = true
	}
	return ids
}

func (e *Engine) promotionOriginalStillActive(ctx context.Context, svc *arrservice.Service, p *promotionState) error {
	snap, err := e.promotionSnapshot(ctx, svc, p)
	if err != nil {
		return err
	}
	expected := promotionExpectedIDs(p)
	seen := 0
	for _, ep := range snap.Episodes {
		if expected[ep.ID] {
			if ep.EpisodeFileID != p.OriginalFileID {
				return fmt.Errorf("episode %d changed since promotion was planned", ep.ID)
			}
			seen++
		} else if ep.EpisodeFileID == p.OriginalFileID {
			return fmt.Errorf("original episodeFile now includes an unapproved episode")
		}
	}
	if seen != len(expected) {
		return fmt.Errorf("one or more approved episodes disappeared")
	}
	for _, f := range snap.Files {
		if f.ID == p.OriginalFileID {
			if filepath.Clean(f.Path) != p.OriginalPath {
				return fmt.Errorf("old episodeFile path changed since promotion was planned")
			}
			return nil
		}
	}
	return fmt.Errorf("original episodeFile disappeared before import")
}

// promotionAdoptedFile verifies Sonarr's logical adoption without opening the
// reported path. Keeping discovery separate from physical inspection lets the
// finalization step use the durable post-rename path instead of accidentally
// reopening a stale temporary path returned during Sonarr reconciliation.
func (e *Engine) promotionAdoptedFile(ctx context.Context, svc *arrservice.Service, p *promotionState) (*promotionFile, bool, error) {
	snap, err := e.promotionSnapshot(ctx, svc, p)
	if err != nil {
		return nil, false, err
	}
	expected := promotionExpectedIDs(p)
	newID, seen := 0, 0
	for _, ep := range snap.Episodes {
		if !expected[ep.ID] {
			continue
		}
		seen++
		if ep.EpisodeFileID <= 0 || ep.EpisodeFileID == p.OriginalFileID {
			return nil, false, nil
		}
		if newID != 0 && newID != ep.EpisodeFileID {
			return nil, false, fmt.Errorf("affected episodes reference different imported files")
		}
		newID = ep.EpisodeFileID
	}
	if seen != len(expected) || newID <= 0 {
		return nil, false, fmt.Errorf("approved episode set changed during promotion")
	}
	for _, ep := range snap.Episodes {
		if ep.EpisodeFileID == newID && !expected[ep.ID] {
			return nil, false, fmt.Errorf("imported file is associated with an unapproved episode")
		}
	}
	for _, f := range snap.Files {
		if f.ID != newID {
			continue
		}
		if f.Size != p.CandidateBytes {
			return nil, false, fmt.Errorf("Sonarr imported file size differs from the validated candidate")
		}
		if !promotionHEVC(f.MediaInfo.VideoCodec) {
			return nil, false, fmt.Errorf("Sonarr has not confirmed HEVC streams for the imported file")
		}
		return &f, true, nil
	}
	return nil, false, fmt.Errorf("Sonarr's active episodeFile is absent from the library listing")
}

// An episode's active file ID, Sonarr's media metadata, and the file's actual
// content must agree. A path or file-size comparison alone is insufficient.
func (e *Engine) promotionAdopted(ctx context.Context, svc *arrservice.Service, p *promotionState) (*promotionFile, bool, error) {
	f, ok, err := e.promotionAdoptedFile(ctx, svc, p)
	if err != nil || !ok {
		return f, ok, err
	}
	if p.NewFileID > 0 && p.NewFileID != f.ID {
		return nil, false, fmt.Errorf("adopted episodeFile identity changed; recovery retained")
	}
	if err := e.promotionInspectAdopted(ctx, p, f.Path); err != nil {
		return nil, false, err
	}
	p.NewFileID, p.NewPath = f.ID, f.Path
	return f, true, nil
}

type promotionCommandResponse struct {
	ID     int            `json:"id"`
	Name   string         `json:"name"`
	Status string         `json:"status"`
	State  string         `json:"state"`
	Body   map[string]any `json:"body"`
}

func promotionCommandState(r promotionCommandResponse) string {
	if r.Status != "" {
		return strings.ToLower(r.Status)
	}
	return strings.ToLower(r.State)
}

func promotionPayloadMatches(want, got map[string]any) bool {
	// Normalize RawMessage metadata and numeric types across a SQL JSON
	// round trip. Sonarr adds fields to command bodies; compare the expected
	// subset recursively rather than serialized field order or raw bytes.
	var normalizedWant, normalizedGot any
	a, err := json.Marshal(want)
	if err != nil {
		return false
	}
	b, err := json.Marshal(got)
	if err != nil {
		return false
	}
	if json.Unmarshal(a, &normalizedWant) != nil || json.Unmarshal(b, &normalizedGot) != nil {
		return false
	}
	return promotionJSONSubset(normalizedWant, normalizedGot, "")
}

func promotionJSONSubset(want, got any, key string) bool {
	switch value := want.(type) {
	case map[string]any:
		other, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range value {
			candidate, ok := other[k]
			if !ok || !promotionJSONSubset(v, candidate, k) {
				return false
			}
		}
		return true
	case []any:
		other, ok := got.([]any)
		if !ok || len(value) != len(other) {
			return false
		}
		for i, v := range value {
			if !promotionJSONSubset(v, other[i], "") {
				return false
			}
		}
		return true
	case string:
		other, ok := got.(string)
		if !ok {
			return false
		}
		if key == "name" || key == "importMode" {
			return strings.EqualFold(value, other)
		}
		return value == other
	default:
		return want == got
	}
}

func (e *Engine) promotionFindCommand(ctx context.Context, svc *arrservice.Service, payload map[string]any) (*promotionCommandResponse, error) {
	data, err := svc.Get(ctx, "/api/v3/command", nil)
	if err != nil {
		return nil, err
	}
	var commands []promotionCommandResponse
	if err := json.Unmarshal(data, &commands); err != nil {
		return nil, err
	}
	var match *promotionCommandResponse
	for _, cmd := range commands {
		if cmd.ID <= 0 || !promotionPayloadMatches(payload, cmd.Body) {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("multiple Sonarr commands match the persisted promotion intent")
		}
		copy := cmd
		match = &copy
	}
	return match, nil
}

func promotionUncertain(sentAt, reason string) (StepResult, error) {
	sent, err := time.Parse(time.RFC3339Nano, sentAt)
	if err == nil && time.Since(sent) < 2*time.Minute {
		return promoteWait(reason + "; reconciling the recorded side effect without resubmitting")
	}
	return StepResult{Status: StepWaitingDecision, WaitingReason: reason + "; no conclusive command/library evidence is available, so the request will not be repeated. The verified recovery copy is retained.", WaitingOptions: []WaitingOption{{Decision: "reconcile", Description: "Check Sonarr again after investigating the recorded command; do not send another import"}}}, nil
}

func (e *Engine) promotionCommand(ctx context.Context, ec *ExecutionContext, p *promotionState, svc *arrservice.Service, key string, payload map[string]any, retryConfirmedFailure bool) (StepResult, error) {
	cmd := p.Commands[key]
	if cmd == nil {
		cmd = &promotionCommand{}
		p.Commands[key] = cmd
	}
	if cmd.Done {
		return StepResult{Status: StepCompleted}, nil
	}
	if cmd.Failed {
		if !retryConfirmedFailure {
			return promoteFailed(fmt.Errorf("Sonarr %s command failed; import cannot be repeated because partial side effects require reconciliation", key))
		}
		// This step previously returned failed, so only explicit action_retry
		// re-enters here. Reconcile its observed effect before this helper.
		cmd = &promotionCommand{}
		p.Commands[key] = cmd
	}
	var response promotionCommandResponse
	if cmd.SentAt == "" {
		cmd.SentAt, cmd.Payload = time.Now().UTC().Format(time.RFC3339Nano), payload
		if err := e.savePromotion(ctx, ec, p); err != nil {
			return promoteFailed(err)
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return promoteFailed(err)
		}
		data, code, err := svc.DoRequestOnce(ctx, http.MethodPost, "/api/v3/command", nil, body)
		if err != nil || code < 200 || code >= 300 {
			// A 5xx or lost response may follow acceptance by Sonarr. The
			// durable SentAt prevents a subsequent invocation from resending.
			return promotionUncertain(cmd.SentAt, fmt.Sprintf("Sonarr %s submit outcome is uncertain (HTTP %d)", key, code))
		}
		if err := json.Unmarshal(data, &response); err != nil || response.ID <= 0 {
			return promotionUncertain(cmd.SentAt, "Sonarr command response did not identify the accepted command")
		}
		cmd.ID = response.ID
		if err := e.savePromotion(ctx, ec, p); err != nil {
			return promoteFailed(err)
		}
	} else if cmd.ID > 0 {
		data, code, err := svc.DoRequest(ctx, http.MethodGet, "/api/v3/command/"+strconv.Itoa(cmd.ID), nil, nil)
		if err != nil {
			return promoteWait("Sonarr command status is temporarily unavailable")
		}
		if code == http.StatusNotFound {
			return promotionUncertain(cmd.SentAt, "Sonarr no longer exposes the recorded command")
		}
		if code < 200 || code >= 300 {
			return promoteWait(fmt.Sprintf("Sonarr command status returned HTTP %d", code))
		}
		if err := json.Unmarshal(data, &response); err != nil || response.ID != cmd.ID {
			return promotionUncertain(cmd.SentAt, "Sonarr returned an invalid command status")
		}
	} else {
		found, err := e.promotionFindCommand(ctx, svc, cmd.Payload)
		if err != nil {
			return promotionUncertain(cmd.SentAt, "Unable to identify the uncertain Sonarr command")
		}
		if found == nil {
			return promotionUncertain(cmd.SentAt, "Sonarr has not exposed the submitted command")
		}
		response = *found
		cmd.ID = response.ID
		if err := e.savePromotion(ctx, ec, p); err != nil {
			return promoteFailed(err)
		}
	}
	switch promotionCommandState(response) {
	case "completed":
		cmd.Done = true
		if err := e.savePromotion(ctx, ec, p); err != nil {
			return promoteFailed(err)
		}
		return StepResult{Status: StepCompleted}, nil
	case "failed", "aborted", "cancelled":
		cmd.Failed = true
		if err := e.savePromotion(ctx, ec, p); err != nil {
			return promoteFailed(err)
		}
		return promoteFailed(fmt.Errorf("Sonarr %s command %d failed; recovery copy retained", key, cmd.ID))
	default:
		return promoteWait(fmt.Sprintf("Waiting for Sonarr %s command %d", key, cmd.ID))
	}
}
