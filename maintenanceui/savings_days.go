package maintenanceui

import (
	"net/http"
	"sort"
	"strconv"
	"time"
)

// savingsDays is how many local days the per-day chart covers.
const savingsDays = 30

// savingsReady is how many ready-to-replace files the Stats view lists.
const savingsReady = 8

// savingsDay is the space freed on one local day. CumulativeBytes is the
// lifetime total at the end of that day, so the chart can say how it grew.
type savingsDay struct {
	Date            string `json:"date"`
	SavedBytes      int64  `json:"saved_bytes"`
	Count           int    `json:"count"`
	CumulativeBytes int64  `json:"cumulative_bytes"`
}

// savingsCandidate is a finished encode that would free space once it replaces
// its original. ActionID opens the encode's job.
type savingsCandidate struct {
	ActionID       string `json:"action_id"`
	SourcePath     string `json:"source_path"`
	SourceBytes    int64  `json:"source_bytes"`
	CandidateBytes int64  `json:"candidate_bytes"`
	SavedBytes     int64  `json:"saved_bytes"`
}

// clientZone reads the browser's offset from UTC (minutes east, as JavaScript's
// negated getTimezoneOffset) so the days match the ones the person sees. A
// missing or odd value means UTC.
func clientZone(req *http.Request) *time.Location {
	minutes, err := strconv.Atoi(req.URL.Query().Get("tz_offset"))
	if err != nil || minutes < -14*60 || minutes > 14*60 {
		return time.UTC
	}
	return time.FixedZone("client", minutes*60)
}

// dailySavings buckets verified replacements by local day over the last
// savingsDays days, oldest first, with empty days kept so the chart has a
// steady axis. history must be oldest first and carry lifetime cumulative bytes.
func dailySavings(history []savingsReplacement, now time.Time, zone *time.Location) []savingsDay {
	today := now.In(zone)
	start := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, zone).AddDate(0, 0, -(savingsDays - 1))
	days := make([]savingsDay, savingsDays)
	index := map[string]int{}
	for i := range days {
		date := start.AddDate(0, 0, i).Format("2006-01-02")
		days[i].Date = date
		index[date] = i
	}
	var before int64
	for _, item := range history {
		done, err := time.Parse(time.RFC3339Nano, item.CompletedAt)
		if err != nil {
			continue
		}
		local := done.In(zone)
		if local.Before(start) {
			before = item.CumulativeBytes
			continue
		}
		if i, ok := index[local.Format("2006-01-02")]; ok {
			days[i].SavedBytes += item.SavedBytes
			days[i].Count++
			days[i].CumulativeBytes = item.CumulativeBytes
		}
	}
	running := before
	for i := range days {
		if days[i].CumulativeBytes < running {
			days[i].CumulativeBytes = running
		}
		running = days[i].CumulativeBytes
	}
	return days
}

// largestCandidates keeps the files that would free the most space.
func largestCandidates(all []savingsCandidate) []savingsCandidate {
	sort.SliceStable(all, func(i, j int) bool { return all[i].SavedBytes > all[j].SavedBytes })
	if len(all) > savingsReady {
		all = all[:savingsReady]
	}
	return all
}
