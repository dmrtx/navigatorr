package action

import (
	"fmt"
	"log"
	"slices"
	"strings"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
)

// warmStartKey holds the sibling prior a size search was sent, for status.
const warmStartKey = "warm_start"

// warmStartLookback bounds the sibling rows read per basis.
const warmStartLookback = 60

// warmStartReport is the prior shown in status: the CRF, which siblings it
// came from and how many validated winners agreed on it.
type warmStartReport struct {
	QualityValue int      `json:"quality_value"`
	Basis        string   `json:"basis"`
	Siblings     int      `json:"siblings"`
	FromActions  []string `json:"from_actions"`
	Note         string   `json:"note"`
}

// warmStartTier is one basis of the prior, closest evidence first.
type warmStartTier struct {
	basis string
	query store.SiblingQuery
}

// applySiblingWarmStart gives a freshly built size search the CRF its
// siblings' validated x265 winners settled on (the median, rounded down to
// the safer side), from the same series and season, else the series, else the
// folder. The worker only orders its initial x265 ladder from it and stops
// that walk early; limits, verification and final validation are unchanged.
// It is sent only to a worker that advertises automatic_warm_start, and never
// with a continuation (which reuses its measurements) or with reuse turned off.
func (e *Engine) applySiblingWarmStart(ec *ExecutionContext, req *transcode.BenchmarkRequest, caps transcode.WorkerCapabilities) {
	delete(ec.State, warmStartKey)
	req.WarmStart = nil
	if !caps.AutomaticWarmStart || !e.outcomeLibraryApplies(ec) || !libraryUseAllowed(ec) {
		return
	}
	if !req.Automatic || !req.QualityPruning || req.AutomaticSearch == nil || req.ContinuesBenchmark != nil {
		return
	}
	snapshot, err := freezeModePolicy(ec)
	if err != nil || snapshot == nil || snapshot.Version != optimization.ModePolicyV3 {
		return
	}
	preset := initialX265Preset(req)
	if preset == "" {
		return
	}
	id := e.transcodeLibraryIdentity(ec)
	sha := getString(ec.State, "original_sha256")
	var tiers []warmStartTier
	if id.hasLibrary() && id.Season >= 0 {
		tiers = append(tiers, warmStartTier{transcode.WarmStartSeriesSeason, store.SiblingQuery{LibraryService: id.Service, LibraryID: id.ID, Season: id.Season}})
	}
	if id.hasLibrary() {
		tiers = append(tiers, warmStartTier{transcode.WarmStartSeries, store.SiblingQuery{LibraryService: id.Service, LibraryID: id.ID, Season: -1}})
	}
	if id.FolderKey != "" {
		tiers = append(tiers, warmStartTier{transcode.WarmStartFolder, store.SiblingQuery{FolderKey: id.FolderKey, Season: -1}})
	}
	for _, tier := range tiers {
		tier.query.ExcludeSHA256, tier.query.PolicyVersion, tier.query.Limit = sha, snapshot.Version, warmStartLookback
		rows, err := e.deps.Store.SiblingTranscodeOutcomes(tier.query)
		if err != nil {
			log.Printf("outcome library: reading siblings for action %s: %v", ec.InstanceID, err)
			return
		}
		crfs, actions := siblingWinningCRFs(rows, preset)
		if len(crfs) == 0 {
			continue
		}
		prior := slices.Sorted(slices.Values(crfs))[(len(crfs)-1)/2]
		req.WarmStart = &transcode.WarmStart{QualityValue: prior, Basis: tier.basis, Siblings: len(crfs)}
		ec.State[warmStartKey] = &warmStartReport{QualityValue: prior, Basis: tier.basis, Siblings: len(crfs), FromActions: actions,
			Note: fmt.Sprintf("Search starts near CRF %d, the median of %d validated %s winner(s); limits are unchanged.", prior, len(crfs), warmStartBasisLabel(tier.basis))}
		return
	}
}

// initialX265Preset is the preset of the request's initial libx265 ladder, the
// rungs a warm start orders ("" when it has none).
func initialX265Preset(req *transcode.BenchmarkRequest) string {
	for _, c := range req.Candidates {
		if !strings.HasPrefix(c.ID, "fallback-") && transcode.BenchmarkCandidateVideoCodec(c) == transcode.VideoCodecLibX265 && c.Quality > 0 && c.AverageBitrateKbps == 0 {
			return c.Preset
		}
	}
	return ""
}

// siblingWinningCRFs reads the libx265 constant-quality CRFs of validated
// winners at the ladder's preset, newest first, one per source.
func siblingWinningCRFs(rows []store.TranscodeOutcome, preset string) (crfs []int, actions []string) {
	seen := map[string]bool{}
	for _, row := range rows {
		if row.Outcome != store.OutcomeWinner || row.FinalVerdict != "pass" || seen[row.SourceSHA256] {
			continue
		}
		rec := winnerRecordOf(row)
		if rec == nil {
			continue
		}
		c := rec.Candidate
		if transcode.BenchmarkCandidateVideoCodec(c) != transcode.VideoCodecLibX265 || c.AverageBitrateKbps != 0 || c.Quality < 1 || c.Quality > 51 || !strings.EqualFold(c.Preset, preset) {
			continue
		}
		seen[row.SourceSHA256] = true
		crfs = append(crfs, c.Quality)
		actions = append(actions, row.ActionID)
		if len(crfs) == transcode.MaxWarmStartSiblings {
			break
		}
	}
	return crfs, actions
}

func warmStartBasisLabel(basis string) string {
	switch basis {
	case transcode.WarmStartSeriesSeason:
		return "same-season"
	case transcode.WarmStartSeries:
		return "same-series"
	}
	return "same-folder"
}
