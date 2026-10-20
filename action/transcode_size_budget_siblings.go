package action

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
)

// A sibling budget sizes a file like the other episodes beside it: most of a
// series is often already small, and the one large episode is the one to bring
// in line. The budget is the median total bitrate of the other video files of
// the same resolution class in the season folder (or, with too few there, the
// series folder one level up), times a multiplier, over the source's duration.

const (
	// sizeBudgetSiblingsKey holds the SiblingBudgetEvidence the preflight found.
	sizeBudgetSiblingsKey = "size_budget_siblings"

	siblingBudgetDefaultMultiplier = 1.5
	siblingBudgetMinMultiplier     = 1.0
	siblingBudgetMaxMultiplier     = 4.0
	// siblingBudgetMinimum siblings of the source's class make a median.
	siblingBudgetMinimum = 3
	// siblingBudgetMaxProbed bounds the ffprobe calls of one preflight.
	siblingBudgetMaxProbed = 60
	// siblingBudgetMaxListed bounds the files kept in the evidence.
	siblingBudgetMaxListed = 40
	// Files shorter than this (extras, samples) do not describe an episode.
	siblingBudgetMinSeconds = 300
	siblingProbeTimeout     = 30 * time.Second
)

// SiblingBudgetRules are frozen in the size budget when it comes from the
// sibling files rather than a percent or a byte count.
type SiblingBudgetRules struct {
	Multiplier      float64 `json:"multiplier"`
	MinimumSiblings int     `json:"minimum_siblings"`
	MaxProbed       int     `json:"max_probed"`
}

func (r *SiblingBudgetRules) validate() error {
	if r == nil {
		return nil
	}
	if math.IsNaN(r.Multiplier) || r.Multiplier < siblingBudgetMinMultiplier || r.Multiplier > siblingBudgetMaxMultiplier {
		return fmt.Errorf("size_budget_sibling_multiplier must be between %.0f and %.0f", siblingBudgetMinMultiplier, siblingBudgetMaxMultiplier)
	}
	if r.MinimumSiblings < 2 || r.MaxProbed < r.MinimumSiblings || r.MaxProbed > 500 {
		return fmt.Errorf("invalid sibling budget rules")
	}
	return nil
}

// SiblingFile is one sibling the median was taken over.
type SiblingFile struct {
	Name            string  `json:"name"`
	Bytes           int64   `json:"bytes"`
	DurationSeconds float64 `json:"duration_seconds"`
	BitrateKbps     int     `json:"bitrate_kbps"`
	Width           int     `json:"width"`
	Height          int     `json:"height"`
}

// SiblingBudgetEvidence is what the preflight measured beside the source.
// Reason is set, and BudgetBytes zero, when no budget could be derived; the
// operation then runs as if it had no budget.
type SiblingBudgetEvidence struct {
	Scope             string  `json:"scope,omitempty"` // "season" or "series"
	ResolutionClass   string  `json:"resolution_class"`
	Count             int     `json:"count"`
	MedianBitrateKbps int     `json:"median_bitrate_kbps,omitempty"`
	Multiplier        float64 `json:"multiplier"`
	SourceBitrateKbps int     `json:"source_bitrate_kbps,omitempty"`
	// Ratio is the source's total bitrate over the siblings' median.
	Ratio       float64       `json:"ratio,omitempty"`
	BudgetBytes int64         `json:"budget_bytes,omitempty"`
	Files       []SiblingFile `json:"files,omitempty"`
	Reason      string        `json:"reason,omitempty"`
	Message     string        `json:"message"`
}

func siblingEvidenceOf(state map[string]any) *SiblingBudgetEvidence {
	if state[sizeBudgetSiblingsKey] == nil {
		return nil
	}
	var ev SiblingBudgetEvidence
	if !reencode(state[sizeBudgetSiblingsKey], &ev) {
		return nil
	}
	return &ev
}

// resolutionClass buckets a picture by its larger extent with the height
// scaled to 16:9, so a letterboxed 1920x800 film and a pillarboxed 1440x1080
// one are both 1080p; a label from the height alone would call the first 720p.
func resolutionClass(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	extent := max(width, int(math.Ceil(float64(height)*16/9)))
	switch {
	case extent <= 1100:
		return "sd"
	case extent <= 1500:
		return "720p"
	case extent <= 2600:
		return "1080p"
	}
	return "2160p"
}

var siblingVideoExtensions = map[string]bool{".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true, ".ts": true, ".m2ts": true, ".webm": true, ".wmv": true}

// siblingSkipDir names folders that never hold siblings: navigatorr's own
// candidates and hidden folders.
func siblingSkipDir(name string) bool {
	return strings.HasPrefix(name, ".") || strings.EqualFold(name, ".navigatorr-candidates")
}

// siblingVideoFiles lists the video files of dir in name order, without the
// source, hidden files and anything under a skipped folder. subdirs also lists
// the files of dir's direct subfolders (the series scope).
func siblingVideoFiles(dir string, source os.FileInfo, subdirs bool) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		full := filepath.Join(dir, name)
		if entry.IsDir() {
			if subdirs && !siblingSkipDir(name) {
				out = append(out, siblingVideoFiles(full, source, false)...)
			}
			continue
		}
		if strings.HasPrefix(name, ".") || !entry.Type().IsRegular() || !siblingVideoExtensions[strings.ToLower(filepath.Ext(name))] {
			continue
		}
		if fi, err := os.Stat(full); err != nil || os.SameFile(fi, source) {
			continue
		}
		out = append(out, full)
	}
	sort.Strings(out)
	return out
}

// sizeBudgetSiblings derives a sibling budget for the source at sourcePath.
// Every sibling is resolved through the read roots like the source itself.
func (e *Engine) sizeBudgetSiblings(ctx context.Context, sourcePath string, rep *mediainspect.DetailedReport, sourceBytes int64, rules *SiblingBudgetRules) *SiblingBudgetEvidence {
	ev := &SiblingBudgetEvidence{Multiplier: rules.Multiplier}
	unavailable := func(reason, message string) *SiblingBudgetEvidence {
		ev.Reason, ev.Message, ev.BudgetBytes = reason, message, 0
		return ev
	}
	video := rep.MainVideo()
	if len(video) != 1 || rep.DurationSec <= 0 || sourceBytes <= 0 {
		return unavailable("source_unreadable", "The source's duration or picture size could not be read, so it cannot be compared with its siblings.")
	}
	ev.ResolutionClass = resolutionClass(video[0].Width, video[0].Height)
	ev.SourceBitrateKbps = int(float64(sourceBytes) * 8 / rep.DurationSec / 1000)
	source, err := os.Stat(sourcePath)
	if err != nil || ev.ResolutionClass == "" {
		return unavailable("source_unreadable", "The source's picture size could not be read, so it cannot be compared with its siblings.")
	}
	probed := 0
	measure := func(paths []string) []SiblingFile {
		var files []SiblingFile
		for _, path := range paths {
			if probed >= rules.MaxProbed || ctx.Err() != nil {
				break
			}
			clean, err := e.deps.Fs.ResolveRead(path)
			if err != nil {
				continue
			}
			probed++
			pctx, cancel := context.WithTimeout(ctx, siblingProbeTimeout)
			r, err := mediainspect.InspectDetailed(pctx, e.deps.Ffprobe, clean)
			cancel()
			v := r.MainVideo()
			if err != nil || len(v) != 1 || r.DurationSec < siblingBudgetMinSeconds || r.SizeBytes <= 0 || resolutionClass(v[0].Width, v[0].Height) != ev.ResolutionClass {
				continue
			}
			files = append(files, SiblingFile{Name: filepath.Base(clean), Bytes: r.SizeBytes, DurationSeconds: r.DurationSec, BitrateKbps: int(float64(r.SizeBytes) * 8 / r.DurationSec / 1000), Width: v[0].Width, Height: v[0].Height})
		}
		return files
	}
	season := filepath.Dir(sourcePath)
	ev.Scope = "season"
	files := measure(siblingVideoFiles(season, source, false))
	if len(files) < rules.MinimumSiblings {
		if series := filepath.Dir(season); series != season {
			if _, err := e.deps.Fs.ResolveRead(series); err == nil {
				ev.Scope = "series"
				probed = 0
				files = measure(siblingVideoFiles(series, source, true))
			}
		}
	}
	ev.Count = len(files)
	if len(files) < rules.MinimumSiblings {
		ev.Files = files
		return unavailable("too_few_siblings", fmt.Sprintf("Only %d other %s video file(s) were found beside the source; a sibling budget needs at least %d. The operation runs without a size budget.", len(files), ev.ResolutionClass, rules.MinimumSiblings))
	}
	rates := make([]float64, len(files))
	for i, f := range files {
		rates[i] = float64(f.Bytes) * 8 / f.DurationSeconds
	}
	sort.Float64s(rates)
	median := rates[len(rates)/2]
	if len(rates)%2 == 0 {
		median = (rates[len(rates)/2-1] + rates[len(rates)/2]) / 2
	}
	ev.MedianBitrateKbps = int(median / 1000)
	ev.Ratio = math.Round(float64(sourceBytes)*8/rep.DurationSec/median*100) / 100
	ev.BudgetBytes = int64(math.Floor(median * rules.Multiplier * rep.DurationSec / 8))
	if len(files) > siblingBudgetMaxListed {
		files = files[:siblingBudgetMaxListed]
	}
	ev.Files = files
	ev.Message = fmt.Sprintf("Budget from %d %s sibling files in the %s folder: median %.1f Mbps × %.2g = %s for this file, which runs at %.1f× the median.", ev.Count, ev.ResolutionClass, ev.Scope, median/1e6, rules.Multiplier, formatBudgetBytes(ev.BudgetBytes), ev.Ratio)
	return ev
}

// recordSizeBudgetSiblings measures the siblings of a size operation whose
// frozen budget comes from them, once, during preflight.
func (e *Engine) recordSizeBudgetSiblings(ctx context.Context, ec *ExecutionContext, snapshot *ModePolicySnapshot, sourcePath string, rep *mediainspect.DetailedReport, sourceBytes int64) {
	if snapshot == nil || snapshot.Size == nil || snapshot.Size.SizeBudget == nil || snapshot.Size.SizeBudget.Siblings == nil || e.deps.Fs == nil {
		return
	}
	if ec.State[sizeBudgetSiblingsKey] != nil {
		return
	}
	ec.State[sizeBudgetSiblingsKey] = e.sizeBudgetSiblings(ctx, sourcePath, rep, sourceBytes, snapshot.Size.SizeBudget.Siblings)
}
