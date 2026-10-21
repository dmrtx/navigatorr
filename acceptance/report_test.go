package acceptance

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"text/tabwriter"
	"time"
)

// caseReport is one row of the acceptance table.
type caseReport struct {
	Case         string  `json:"case"`
	Goal         string  `json:"goal"`
	Outcome      string  `json:"outcome"`
	ReasonCode   string  `json:"reason_code,omitempty"`
	SourceBytes  int64   `json:"source_bytes,omitempty"`
	CandidateB   int64   `json:"candidate_bytes,omitempty"`
	SavedPercent float64 `json:"saved_percent,omitempty"`
	VMAFMean     float64 `json:"vmaf_mean,omitempty"`
	VMAFP5       float64 `json:"vmaf_p5,omitempty"`
	VMAFWorst    float64 `json:"vmaf_worst_window,omitempty"`
	Encoder      string  `json:"encoder,omitempty"`
	Rung         int     `json:"vt_rung,omitempty"`
	LadderStep   int     `json:"ladder_step,omitempty"`
	WallSeconds  float64 `json:"wall_seconds"`
	Passed       bool    `json:"passed"`
	Skipped      bool    `json:"skipped,omitempty"`
	Failure      string  `json:"failure,omitempty"`
}

type reporter struct {
	mu   sync.Mutex
	rows []*caseReport
}

func (r *reporter) add(row *caseReport) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, row)
}

// finish prints the table and writes the JSON report.
func (r *reporter) finish(t *testing.T) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.rows) == 0 {
		return
	}
	var sb strings.Builder
	w := tabwriter.NewWriter(&sb, 2, 0, 2, ' ', 0)
	fmt.Fprintln(w, "CASE\tGOAL\tRESULT\tSOURCE\tCANDIDATE\tSAVED\tVMAF mean/p5/worst\tENCODER\tWALL")
	var total float64
	failed := 0
	for _, row := range r.rows {
		result := row.Outcome
		if row.ReasonCode != "" {
			result += "/" + row.ReasonCode
		}
		switch {
		case row.Skipped:
			result = "SKIPPED"
		case !row.Passed:
			result = "FAIL " + result
			failed++
		}
		cand, saved, vmaf, enc := "-", "-", "-", "-"
		if row.CandidateB > 0 {
			cand = megabytes(row.CandidateB)
			saved = fmt.Sprintf("%.1f%%", row.SavedPercent)
		}
		if row.VMAFMean > 0 {
			vmaf = fmt.Sprintf("%.1f/%.1f/%.1f", row.VMAFMean, row.VMAFP5, row.VMAFWorst)
		}
		if row.Encoder != "" {
			enc = row.Encoder
			if row.Rung > 0 {
				enc += fmt.Sprintf(" rung %d", row.Rung)
			}
			if row.LadderStep > 0 {
				enc += fmt.Sprintf(" step %d", row.LadderStep)
			}
		}
		src := "-"
		if row.SourceBytes > 0 {
			src = megabytes(row.SourceBytes)
		}
		total += row.WallSeconds
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", row.Case, row.Goal, result, src, cand, saved, vmaf, enc, (time.Duration(row.WallSeconds * float64(time.Second))).Round(time.Second))
	}
	_ = w.Flush()
	t.Logf("\n%s\nactions %s, %d failed", sb.String(), (time.Duration(total * float64(time.Second))).Round(time.Second), failed)

	if path := os.Getenv(envReport); path != "" {
		b, err := json.MarshalIndent(map[string]any{"cases": r.rows, "action_wall_seconds": total, "failed": failed}, "", "  ")
		if err == nil {
			err = os.WriteFile(path, append(b, '\n'), 0o644)
		}
		if err != nil {
			t.Errorf("writing %s: %v", envReport, err)
		}
	}
}

func megabytes(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/1e6) }
