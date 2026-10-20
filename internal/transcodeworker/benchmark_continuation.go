package transcodeworker

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"

	"github.com/jakenesler/navigatorr/transcode"
)

// seedContinuation reuses the measurements of the earlier search a continued
// search follows. The earlier search is read from this worker's own persisted
// record, never from the request, and a candidate is reused only when it was
// measured on exactly the discovery windows, under the same source, quality
// policy and capabilities, with the same encoder settings. Anything else is
// simply measured again: reuse is an optimisation and never an error.
//
// It returns the IDs of the reused candidates, whose rows now sit in evidence
// under their index in this search.
func (r *ProductionBenchmarkRunner) seedContinuation(ctx context.Context, w *Worker, record *BenchmarkRecord, evidence *BenchmarkExecutionEvidence, vcs []validatedCandidate) map[string]bool {
	reused := map[string]bool{}
	ref := record.ContinuesBenchmark
	if ref == nil || ref.Validate() != nil || len(record.Samples) < 3 || w == nil {
		return reused
	}
	prior, err := LoadBenchmark(filepath.Join(filepath.Clean(w.cfg.StateDir), ref.ID, "benchmark.json"))
	if err != nil || prior.ID != ref.ID || prior.RequestDigest != ref.RequestDigest || prior.Status != "completed" || prior.Evidence == nil {
		return reused
	}
	if !sameMeasurementBasis(prior, record) || !sameSourceEvidence(prior.Evidence, evidence) {
		return reused
	}
	fingerprint := r.currentCapabilityFingerprint(ctx, w, record)
	if fingerprint == "" {
		return reused
	}
	discovery := map[int]bool{}
	for _, window := range record.Samples[:3] {
		discovery[window.Index] = true
	}
	priorCandidates := map[string]transcode.BenchmarkCandidate{}
	for _, c := range prior.Candidates {
		priorCandidates[c.ID] = c
	}
	old := prior.Evidence
	for _, vc := range vcs {
		id := vc.candidate.ID
		if before, ok := priorCandidates[id]; !ok || !sameJSON(before, vc.candidate) || isVideoToolbox(vc.candidate.VideoCodec) {
			continue
		}
		samples, ok := discoverySamples(old.CandidateSamples, id, discovery)
		if !ok {
			continue
		}
		metrics, ok := discoveryMetricSamples(old.MetricSamples, id, discovery)
		if !ok {
			continue
		}
		var quality *CandidateQualityEvidence
		for i := range old.Quality {
			if old.Quality[i].CandidateID == id {
				if quality != nil {
					quality = nil
					break
				}
				quality = &old.Quality[i]
			}
		}
		var aggregates []BenchmarkCandidateMetricAggregate
		for _, a := range old.CandidateMetrics {
			if a.CandidateID == id {
				aggregates = append(aggregates, a)
			}
		}
		if quality == nil || quality.CapabilityFingerprint != fingerprint || len(aggregates) == 0 {
			continue
		}
		evidence.CandidateSamples = append(evidence.CandidateSamples, samples...)
		for _, m := range metrics {
			m.CandidateIndex = vc.index
			evidence.MetricSamples = append(evidence.MetricSamples, m)
		}
		evidence.Quality = append(evidence.Quality, *quality)
		for _, a := range aggregates {
			a.CandidateIndex = vc.index
			evidence.CandidateMetrics = append(evidence.CandidateMetrics, a)
		}
		reused[id] = true
	}
	return reused
}

// currentCapabilityFingerprint is the fingerprint the quality evidence of this
// run would carry; empty when it cannot be established.
func (r *ProductionBenchmarkRunner) currentCapabilityFingerprint(ctx context.Context, w *Worker, record *BenchmarkRecord) string {
	if r.capabilityFingerprint != nil {
		fingerprint, err := r.capabilityFingerprint(ctx, w)
		if err != nil {
			return ""
		}
		return fingerprint
	}
	probe := record.Quality != nil && (record.Quality.VMAF != nil && record.Quality.VMAF.Model != "" || record.Quality.Banding != nil && record.Quality.Banding.Enabled)
	caps, err := probeWorkerCapabilitiesWithScratch(ctx, w.ffmpegPath, filepath.Join(w.cfg.StateDir, "quality-probe-scratch"), probe)
	if err != nil {
		return ""
	}
	return caps.CapabilityFingerprint
}

// sameMeasurementBasis reports whether two searches measure the same source
// the same way: everything a candidate's sample measurements depend on except
// the candidate itself.
func sameMeasurementBasis(a, b *BenchmarkRecord) bool {
	if a.Source != b.Source || a.SourceSHA256 == "" || a.SourceSHA256 != b.SourceSHA256 || a.Metric != b.Metric || a.Mode != b.Mode || a.ModePolicyVersion != b.ModePolicyVersion ||
		a.Automatic != b.Automatic || a.Allow8BitTo10Bit != b.Allow8BitTo10Bit || a.AudioMode != b.AudioMode || a.SourceDuration != b.SourceDuration ||
		a.DeclaredVideoBitrateBps != b.DeclaredVideoBitrateBps || a.AttachmentBytes != b.AttachmentBytes || a.FallbackAudioBitrateBps != b.FallbackAudioBitrateBps || a.FallbackSubtitleSizeBytes != b.FallbackSubtitleSizeBytes {
		return false
	}
	return sameJSON(a.Samples, b.Samples) && sameJSON(a.Quality, b.Quality) && sameJSON(a.AutomaticSearch, b.AutomaticSearch) && sameJSON(a.Compact, b.Compact)
}

// sameSourceEvidence reports whether both searches saw the source the same way.
// The VMAF temporal decimation is not compared: it is derived from the frame
// rate and field order, which are, and a search records it only once its
// metric step has run, so a search still starting out always reads zero.
func sameSourceEvidence(a, b *BenchmarkExecutionEvidence) bool {
	return a.SourceVideoIndex == b.SourceVideoIndex && a.SourceBitDepth == b.SourceBitDepth && a.SourcePixelFormat == b.SourcePixelFormat &&
		a.SourceResolution == b.SourceResolution && a.SourceFPS == b.SourceFPS && a.SourceFieldOrder == b.SourceFieldOrder
}

func sameJSON(a, b any) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(x, y)
}

// discoverySamples returns a candidate's encoded sample rows when they are
// exactly one clean row for each discovery window and nothing else.
func discoverySamples(rows []BenchmarkCandidateSampleResult, id string, discovery map[int]bool) ([]BenchmarkCandidateSampleResult, bool) {
	var out []BenchmarkCandidateSampleResult
	seen := map[int]bool{}
	for _, row := range rows {
		if row.CandidateID != id {
			continue
		}
		if !discovery[row.SampleIndex] || seen[row.SampleIndex] || row.Error != "" || row.SizeBytes <= 0 {
			return nil, false
		}
		seen[row.SampleIndex] = true
		out = append(out, row)
	}
	return out, len(out) == len(discovery)
}

// discoveryMetricSamples is discoverySamples for the quality metric rows.
func discoveryMetricSamples(rows []BenchmarkMetricSampleResult, id string, discovery map[int]bool) ([]BenchmarkMetricSampleResult, bool) {
	var out []BenchmarkMetricSampleResult
	seen := map[int]bool{}
	for _, row := range rows {
		if row.CandidateID != id {
			continue
		}
		if !discovery[row.SampleIndex] || seen[row.SampleIndex] || row.Error != "" || row.VMAF == nil {
			return nil, false
		}
		seen[row.SampleIndex] = true
		out = append(out, row)
	}
	return out, len(out) == len(discovery)
}
