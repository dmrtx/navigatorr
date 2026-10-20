package transcodeworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

// sampleCacheMockFFmpeg stands in for ffmpeg: it logs every encode ("enc"),
// reference extraction ("ref") and metric pass ("vmaf", "cambi") to the log
// file, and fabricates deterministic per-frame scores from the candidate
// quality and window in the file name, so equal inputs always measure equal.
const sampleCacheMockFFmpeg = `#!/bin/sh
LOG="@LOG@"
case "$*" in
  *"-version"*)
    echo "ffmpeg version 7.1 Copyright (c) 2000-2024 the FFmpeg developers"
    exit 0
    ;;
  *"-buildconf"*)
    echo "configuration:"
    exit 0
    ;;
  *"-h encoder=hevc_videotoolbox"*)
    cat << 'EOF'
Encoder hevc_videotoolbox [VideoToolbox H.265 Encoder]:
    Supported pixel formats: nv12 p010le yuv420p
hevc_videotoolbox AVOptions:
  -profile           <int>        E..V....... Profile (from 0 to 2) (default 0)
     main            1            E..V....... Main Profile
     main10          2            E..V....... Main10 Profile
  -prio_speed        <boolean>    E..V....... Prioritize encoding speed (default false)
  -spatial_aq        <boolean>    E..V....... Spatial AQ (default false)
  -realtime          <boolean>    E..V....... Realtime (default false)
EOF
    exit 0
    ;;
  *"-encoders"*)
    cat << 'EOF'
Encoders:
 V..... hevc_videotoolbox    VideoToolbox H.265
 V..... ffv1                 FFmpeg video codec #1
EOF
    exit 0
    ;;
  *"-filters"*)
    cat << 'EOF'
Filters:
  .. libvmaf           VV->V      Calculate the VMAF between two video streams.
  TS ssim              VV->V      Calculate the SSIM between two video streams.
  .S scale             V->V       Scale the input video size and/or convert the image format.
  .. format            V->V       Convert the input video to one of the specified pixel formats.
  .. null              V->V       Pass the source unchanged to the output.
  .. fps               V->V       Force constant framerate.
EOF
    exit 0
    ;;
esac

case "$*" in
  *"-filter_complex"*"libvmaf"*)
    kind=vmaf
    case "$*" in *"name=cambi"*) kind=cambi ;; esac
    echo "$kind $*" >> "$LOG"
    info=$(echo "$*" | sed -n 's/.*_q\([0-9][0-9]*\)_sample_\([0-9][0-9]*\)\.mkv.*/\1 \2/p')
    q=${info% *}
    s=${info#* }
    for arg in "$@"; do
      case "$arg" in
        *"log_path="*)
          lpath="${arg#*log_path=}"
          lpath="${lpath%%:*}"
          mkdir -p "$(dirname "$lpath")"
          awk -v q="$q" -v s="$s" -v kind="$kind" 'BEGIN {
            printf "{\"version\":\"2.3.1\",\"frames\":["
            for (i = 0; i < 60; i++) {
              if (kind == "cambi") v = (i % 9) * 0.013 + s * 0.002
              else { v = q + (i % 5) * 0.37 + s * 0.11; if (v > 100) v = 100 }
              printf "%s{\"frameNum\":%d,\"metrics\":{\"%s\":%.6f}}", (i ? "," : ""), i, (kind == "cambi" ? "cambi" : "vmaf"), v
            }
            printf "]}\n"
          }' > "$lpath"
          ;;
      esac
    done
    exit 0
    ;;
esac

out=""
for last; do out="$last"; done
case "$*" in
  *"-c:v ffv1"*)
    echo "ref $*" >> "$LOG"
    mkdir -p "$(dirname "$out")"
    head -c 2048 /dev/zero > "$out"
    exit 0
    ;;
  *"hevc_videotoolbox"*)
    echo "enc $*" >> "$LOG"
    q=$(echo "$*" | sed -n 's/.*-q:v \([0-9][0-9]*\).*/\1/p')
    mkdir -p "$(dirname "$out")"
    head -c $((q * 100)) /dev/zero > "$out"
    exit 0
    ;;
esac
exit 0
`

// sampleCacheEnv is a worker home shared by successive searches, so a later
// search sees the cache an earlier one wrote, as a later job process would.
type sampleCacheEnv struct {
	t       *testing.T
	dir     string
	source  string
	sha     string
	ffmpeg  string
	ffprobe string
	log     string
	cfg     *WorkerConfig
	caps    transcode.QualityCapabilities
	build   string
}

func newSampleCacheEnv(t *testing.T) *sampleCacheEnv {
	t.Helper()
	dir := t.TempDir()
	e := &sampleCacheEnv{t: t, dir: dir, build: "build-a"}
	e.log = filepath.Join(dir, "calls.log")
	e.ffmpeg = filepath.Join(dir, "mock_ffmpeg.sh")
	e.ffprobe = filepath.Join(dir, "mock_ffprobe.sh")
	if err := os.WriteFile(e.ffmpeg, []byte(strings.ReplaceAll(sampleCacheMockFFmpeg, "@LOG@", e.log)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.ffprobe, []byte("#!/bin/sh\ncat << 'EOF'\n"+sdr8BitProbeJSON+"\nEOF\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.source = filepath.Join(dir, "source.mkv")
	e.writeSource("original source media")
	e.cfg = &WorkerConfig{StateDir: filepath.Join(dir, "state"), AllowedRoots: []string{dir}, MaxParallelJobs: 1, FFmpeg: e.ffmpeg, FFprobe: e.ffprobe}
	e.caps = transcode.QualityCapabilities{Native10Bit: true, CAMBIFullRef: true, CAMBIOutput: "cambi", LibvmafVersion: "3.0.0", Models: map[string]transcode.QualityModelCapability{"v1_1080p_3h": {Available: true}}}
	prev := sampleCacheHardwareIdentity
	sampleCacheHardwareIdentity = func(context.Context) (string, error) { return "hw-a", nil }
	t.Cleanup(func() {
		sampleCacheHardwareIdentity = prev
		info, err := os.Stat(e.ffmpeg)
		if err == nil {
			qualityProbeCache.Lock()
			delete(qualityProbeCache.results, fmt.Sprintf("%s:%d:%d", e.ffmpeg, info.Size(), info.ModTime().UnixNano()))
			qualityProbeCache.Unlock()
		}
	})
	return e
}

func (e *sampleCacheEnv) writeSource(content string) {
	e.t.Helper()
	if err := os.WriteFile(e.source, []byte(content), 0o600); err != nil {
		e.t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	e.sha = hex.EncodeToString(sum[:])
}

func (e *sampleCacheEnv) seedCaps() {
	info, err := os.Stat(e.ffmpeg)
	if err != nil {
		e.t.Fatal(err)
	}
	key := fmt.Sprintf("%s:%d:%d", e.ffmpeg, info.Size(), info.ModTime().UnixNano())
	qualityProbeCache.Lock()
	qualityProbeCache.results[key] = struct {
		caps transcode.QualityCapabilities
		at   time.Time
	}{e.caps, time.Now()}
	qualityProbeCache.Unlock()
}

func (e *sampleCacheEnv) cacheDir() string {
	return NewWorker(e.cfg).sampleCacheDir()
}

func (e *sampleCacheEnv) entries() []string {
	var out []string
	_ = filepath.WalkDir(e.cacheDir(), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".json") {
			out = append(out, path)
		}
		return nil
	})
	return out
}

func (e *sampleCacheEnv) record() *BenchmarkRecord {
	return &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "sample-cache",
		Status:          "running",
		Source:          e.source,
		SourceSHA256:    e.sha,
		Metric:          "vmaf",
		Attempt:         1,
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5, DurationSeconds: 5},
			{Index: 1, StartSeconds: 20, DurationSeconds: 5},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_q70", Quality: 70},
			{ID: "cand_q75", Quality: 75},
			{ID: "cand_q80", Quality: 80},
			{ID: "cand_q85", Quality: 85},
		},
		Quality: &transcode.BenchmarkQualityConfig{VMAF: &transcode.BenchmarkQualityThresholds{Model: "v1_1080p_3h", Target: 78, Minimum: 72, WorstWindowSeconds: 2}},
	}
}

type sampleCacheCalls struct{ enc, ref, vmaf, cambi int }

func (e *sampleCacheEnv) calls() sampleCacheCalls {
	data, _ := os.ReadFile(e.log)
	var c sampleCacheCalls
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "enc "):
			c.enc++
		case strings.HasPrefix(line, "ref "):
			c.ref++
		case strings.HasPrefix(line, "vmaf "):
			c.vmaf++
		case strings.HasPrefix(line, "cambi "):
			c.cambi++
		}
	}
	return c
}

// search runs one benchmark on a fresh worker (as a new job process would),
// after mutate adjusts the base record, and reports what it executed.
func (e *sampleCacheEnv) search(mutate func(*BenchmarkRecord)) (*BenchmarkRecord, sampleCacheCalls) {
	e.t.Helper()
	e.seedCaps()
	_ = os.Remove(e.log)
	record := e.record()
	if mutate != nil {
		mutate(record)
	}
	w := NewWorker(e.cfg)
	build := e.build
	w.sampleCacheBuildFn = func(context.Context, string) (sampleCacheBuildIdentity, error) {
		return sampleCacheBuildIdentity{FFmpeg: build, X265: "x265-test"}, nil
	}
	if err := (&ProductionBenchmarkRunner{}).RunBenchmark(context.Background(), w, record); err != nil {
		e.t.Fatalf("RunBenchmark: %v", err)
	}
	if record.Evidence == nil || record.Evidence.Decision == nil || record.Evidence.Decision.Winner == nil {
		e.t.Fatalf("search produced no winner: %+v", record.Evidence)
	}
	return record, e.calls()
}

// decisionJSON is the decision with the reuse marker removed, the only thing
// that may differ between a measured and a replayed search. A search that
// measured some units afresh also differs in their wall-clock seconds, which
// keepMeasuredSeconds=false drops.
func decisionJSON(t *testing.T, record *BenchmarkRecord, keepMeasuredSeconds bool) string {
	t.Helper()
	d := *record.Evidence.Decision
	d.Evaluations = append([]transcode.BenchmarkCandidateEvaluation(nil), d.Evaluations...)
	for i := range d.Evaluations {
		d.Evaluations[i].Reuse = nil
		if !keepMeasuredSeconds {
			d.Evaluations[i].MeasuredSeconds = nil
		}
	}
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func qualityJSON(t *testing.T, record *BenchmarkRecord) string {
	t.Helper()
	data, err := json.Marshal(struct {
		Quality []CandidateQualityEvidence
		Metrics []BenchmarkCandidateMetricAggregate
	}{record.Evidence.Quality, record.Evidence.CandidateMetrics})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSampleCacheHitGivesIdenticalSelection(t *testing.T) {
	e := newSampleCacheEnv(t)
	cold, calls := e.search(nil)
	if calls.enc != 8 || calls.vmaf != 8 || calls.ref != 2 {
		t.Fatalf("cold run executed %+v, want 8 encodes, 8 vmaf passes, 2 references", calls)
	}
	if len(e.entries()) != 8 {
		t.Fatalf("cold run stored %d entries, want 8", len(e.entries()))
	}
	for _, ev := range cold.Evidence.Decision.Evaluations {
		if ev.Reuse != nil {
			t.Fatalf("cold run marked %s reused", ev.CandidateID)
		}
	}

	warm, calls := e.search(nil)
	if calls.enc != 0 || calls.vmaf != 0 {
		t.Fatalf("warm run re-measured: %+v", calls)
	}
	if got, want := decisionJSON(t, warm, true), decisionJSON(t, cold, true); got != want {
		t.Fatalf("replayed decision differs from the measured one\n got: %s\nwant: %s", got, want)
	}
	// Pooled p5/mean come from the stored per-frame scores.
	if got, want := qualityJSON(t, warm), qualityJSON(t, cold); got != want {
		t.Fatalf("replayed quality evidence differs\n got: %s\nwant: %s", got, want)
	}
	for _, cs := range warm.Evidence.CandidateSamples {
		if !cs.Reused || cs.File != "" || cs.SizeBytes <= 0 {
			t.Fatalf("replayed candidate sample not marked or malformed: %+v", cs)
		}
	}
	for _, ms := range warm.Evidence.MetricSamples {
		if !ms.Reused || ms.VMAFStats == nil {
			t.Fatalf("replayed metric sample not marked: %+v", ms)
		}
	}
	for _, ev := range warm.Evidence.Decision.Evaluations {
		if ev.Reuse == nil || ev.Reuse.Reused != 2 || ev.Reuse.Total != 2 {
			t.Fatalf("evaluation %s reuse = %+v, want 2 of 2", ev.CandidateID, ev.Reuse)
		}
	}
	// A replay costs this run nothing, so it reports no sample work.
	for _, phase := range []string{"sample_encode", "sample_validation"} {
		if cold.PhaseCosts[phase].DurationMs <= 0 {
			t.Fatalf("cold run recorded no %s cost", phase)
		}
		if got := warm.PhaseCosts[phase]; got.DurationMs != 0 {
			t.Fatalf("replayed samples were charged to %s: %+v", phase, got)
		}
	}
	if warm.Progress != 99 {
		t.Fatalf("progress = %v, want 99", warm.Progress)
	}
}

func TestSampleCacheMeasuresOnlyNewCandidates(t *testing.T) {
	e := newSampleCacheEnv(t)
	e.search(nil)
	record, calls := e.search(func(r *BenchmarkRecord) {
		r.Candidates = append(r.Candidates, transcode.BenchmarkCandidate{ID: "cand_q90", Quality: 90})
	})
	if calls.enc != 2 || calls.vmaf != 2 {
		t.Fatalf("expected only the new candidate's 2 samples to run, got %+v", calls)
	}
	for _, ev := range record.Evidence.Decision.Evaluations {
		wantReused := 2
		if ev.CandidateID == "cand_q90" {
			wantReused = 0
		}
		got := 0
		if ev.Reuse != nil {
			got = ev.Reuse.Reused
		}
		if got != wantReused {
			t.Fatalf("%s reused %d samples, want %d", ev.CandidateID, got, wantReused)
		}
	}
}

// A new label names the same encode, so it hits; the thread settings are
// bitstream- and score-neutral, so they hit too.
func TestSampleCacheIgnoresLabelsAndThreads(t *testing.T) {
	e := newSampleCacheEnv(t)
	e.search(nil)
	e.cfg.MetricThreads = 3
	_, calls := e.search(func(r *BenchmarkRecord) {
		for i := range r.Candidates {
			r.Candidates[i].ID = "renamed_" + r.Candidates[i].ID
		}
		r.ID = "another-job"
		r.Concurrency = &transcode.BenchmarkConcurrencyConfig{EncodeConcurrency: 1, MetricConcurrency: 1}
	})
	if calls.enc != 0 || calls.vmaf != 0 {
		t.Fatalf("labels or thread settings caused a miss: %+v", calls)
	}
}

// Every part of the key must invalidate: a changed part re-measures exactly
// the affected units and never replays a stale one.
func TestSampleCacheMissWhenAnyKeyPartChanges(t *testing.T) {
	boolPtr := func(v bool) *bool { return &v }
	intPtr := func(v int) *int { return &v }
	floatPtr := func(v float64) *float64 { return &v }
	cases := []struct {
		name    string
		prepare func(e *sampleCacheEnv) // runs before the second search
		mutate  func(*BenchmarkRecord)
		enc     int // encodes the second search must run
		cambi   int
	}{
		{name: "source sha256", prepare: func(e *sampleCacheEnv) { e.sha = strings.Repeat("ab", 32) }, enc: 8},
		{name: "source size", prepare: func(e *sampleCacheEnv) {
			sha := e.sha
			e.writeSource("original source media, longer")
			e.sha = sha
		}, enc: 8},
		{name: "window start", mutate: func(r *BenchmarkRecord) { r.Samples[1].StartSeconds = 21 }, enc: 4},
		{name: "window duration", mutate: func(r *BenchmarkRecord) { r.Samples[1].DurationSeconds = 6 }, enc: 4},
		{name: "window index", mutate: func(r *BenchmarkRecord) { r.Samples[1].Index = 2 }, enc: 4},
		{name: "candidate gop", mutate: func(r *BenchmarkRecord) {
			for i := range r.Candidates {
				r.Candidates[i].GOPSize = intPtr(48)
			}
		}, enc: 8},
		{name: "candidate b-frames", mutate: func(r *BenchmarkRecord) {
			for i := range r.Candidates {
				r.Candidates[i].BFrames = intPtr(1)
			}
		}, enc: 8},
		{name: "candidate rate control", mutate: func(r *BenchmarkRecord) {
			for i := range r.Candidates {
				r.Candidates[i].SpatialAQ = boolPtr(true)
			}
		}, enc: 8},
		{name: "one candidate's quality", mutate: func(r *BenchmarkRecord) { r.Candidates[3].Quality = 86 }, enc: 2},
		{name: "vmaf worst window", mutate: func(r *BenchmarkRecord) { r.Quality.VMAF.WorstWindowSeconds = 1 }, enc: 8},
		{name: "vmaf frame threshold", mutate: func(r *BenchmarkRecord) { r.Quality.VMAF.FrameThreshold = floatPtr(60) }, enc: 8},
		{name: "cambi enabled", mutate: func(r *BenchmarkRecord) {
			r.Quality.Banding = &transcode.BenchmarkBandingConfig{Enabled: true, Mode: "full_reference"}
		}, enc: 8, cambi: 8},
		{name: "worker capability fingerprint", prepare: func(e *sampleCacheEnv) { e.caps.LibvmafVersion = "3.0.1" }, enc: 8},
		{name: "ffmpeg build", prepare: func(e *sampleCacheEnv) { e.build = "build-b" }, enc: 8},
		{name: "hardware encoder identity", prepare: func(e *sampleCacheEnv) {
			sampleCacheHardwareIdentity = func(context.Context) (string, error) { return "hw-b", nil }
		}, enc: 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newSampleCacheEnv(t)
			e.search(nil)
			if tc.prepare != nil {
				tc.prepare(e)
			}
			_, calls := e.search(tc.mutate)
			if calls.enc != tc.enc {
				t.Fatalf("second search ran %d encodes, want %d", calls.enc, tc.enc)
			}
			if tc.cambi != 0 && calls.cambi != tc.cambi {
				t.Fatalf("second search ran %d cambi passes, want %d", calls.cambi, tc.cambi)
			}
		})
	}
}

func TestSampleCacheIgnoresCorruptEntries(t *testing.T) {
	rewrite := func(t *testing.T, path string, edit func(body map[string]any)) {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var file struct {
			Body json.RawMessage `json:"body"`
		}
		if err := json.Unmarshal(data, &file); err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(file.Body, &body); err != nil {
			t.Fatal(err)
		}
		edit(body)
		raw, _ := json.Marshal(body)
		sum := sha256.Sum256(raw)
		out, _ := json.Marshal(map[string]any{"body": json.RawMessage(raw), "sha256": hex.EncodeToString(sum[:])})
		if err := os.WriteFile(path, out, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	corruptions := map[string]func(t *testing.T, path string){
		"truncated": func(t *testing.T, p string) {
			data, _ := os.ReadFile(p)
			_ = os.WriteFile(p, data[:len(data)/2], 0o600)
		},
		"empty":   func(t *testing.T, p string) { _ = os.WriteFile(p, nil, 0o600) },
		"garbage": func(t *testing.T, p string) { _ = os.WriteFile(p, []byte("not json at all"), 0o600) },
		"bad sha256": func(t *testing.T, p string) {
			data, _ := os.ReadFile(p)
			_ = os.WriteFile(p, []byte(strings.Replace(string(data), `"sha256":"`, `"sha256":"00`, 1)), 0o600)
		},
		"wrong schema": func(t *testing.T, p string) { rewrite(t, p, func(b map[string]any) { b["schema"] = 99 }) },
		"wrong key echo": func(t *testing.T, p string) {
			rewrite(t, p, func(b map[string]any) { b["key"] = strings.Repeat("0", 64) })
		},
		"zero size":     func(t *testing.T, p string) { rewrite(t, p, func(b map[string]any) { b["size_bytes"] = 0 }) },
		"unknown field": func(t *testing.T, p string) { rewrite(t, p, func(b map[string]any) { b["surprise"] = true }) },
		"vmaf missing":  func(t *testing.T, p string) { rewrite(t, p, func(b map[string]any) { delete(b, "vmaf") }) },
		"scores disagree with their summary": func(t *testing.T, p string) {
			rewrite(t, p, func(b map[string]any) {
				v := b["vmaf"].(map[string]any)
				v["frame_scores"] = encodeFrameScores(make([]float64, 60))
			})
		},
		"mean disagrees with stats": func(t *testing.T, p string) {
			rewrite(t, p, func(b map[string]any) { b["vmaf"].(map[string]any)["mean"] = 1.5 })
		},
		"stats for another window": func(t *testing.T, p string) {
			rewrite(t, p, func(b map[string]any) {
				b["vmaf"].(map[string]any)["stats"].(map[string]any)["worst_window_sample_index"] = 7
			})
		},
		"cambi unexpected": func(t *testing.T, p string) {
			rewrite(t, p, func(b map[string]any) { b["cambi"] = map[string]any{"output_metric": "cambi", "frames_total": 3} })
		},
	}
	for name, corrupt := range corruptions {
		t.Run(name, func(t *testing.T) {
			e := newSampleCacheEnv(t)
			cold, _ := e.search(nil)
			entries := e.entries()
			if len(entries) != 8 {
				t.Fatalf("stored %d entries", len(entries))
			}
			corrupt(t, entries[0])
			again, calls := e.search(nil)
			if calls.enc != 1 || calls.vmaf != 1 {
				t.Fatalf("corrupt entry was trusted or caused more than its own re-measure: %+v", calls)
			}
			if decisionJSON(t, again, false) != decisionJSON(t, cold, false) {
				t.Fatal("decision changed after a corrupt entry")
			}
			// The bad entry was replaced by a good one.
			if _, calls = e.search(nil); calls.enc != 0 {
				t.Fatalf("entry was not repaired: %+v", calls)
			}
		})
	}
}

func TestSampleCacheDisabledAndUnkeyed(t *testing.T) {
	t.Run("disabled by config", func(t *testing.T) {
		e := newSampleCacheEnv(t)
		e.cfg.DisableSampleCache = true
		e.search(nil)
		if _, calls := e.search(nil); calls.enc != 8 {
			t.Fatalf("disabled cache still replayed: %+v", calls)
		}
		if _, err := os.Stat(e.cacheDir()); !os.IsNotExist(err) {
			t.Fatalf("disabled cache wrote %s", e.cacheDir())
		}
	})
	for name, sha := range map[string]string{"missing source sha256": "", "malformed source sha256": "xyz"} {
		t.Run(name, func(t *testing.T) {
			e := newSampleCacheEnv(t)
			mutate := func(r *BenchmarkRecord) { r.SourceSHA256 = sha }
			e.search(mutate)
			if _, calls := e.search(mutate); calls.enc != 8 {
				t.Fatalf("unkeyed source was replayed: %+v", calls)
			}
			if len(e.entries()) != 0 {
				t.Fatal("unkeyed source was stored")
			}
		})
	}
	t.Run("hardware identity unknown", func(t *testing.T) {
		e := newSampleCacheEnv(t)
		sampleCacheHardwareIdentity = func(context.Context) (string, error) { return "", fmt.Errorf("no sysctl") }
		e.search(nil)
		if _, calls := e.search(nil); calls.enc != 8 || len(e.entries()) != 0 {
			t.Fatalf("hardware encodes were cached without a hardware identity: %+v", calls)
		}
	})
	t.Run("savings gate keeps its exact semantics", func(t *testing.T) {
		e := newSampleCacheEnv(t)
		sh := &pipelineShared{w: NewWorker(e.cfg), savings: &savingsGate{}, record: e.record(), runVMAF: true}
		if sh.newSampleCacheRun(context.Background(), 1) != nil {
			t.Fatal("savings-gated search must not use the cache")
		}
	})
}

// A replayed unit takes no budget: it resolves progress but is not search
// work, so it neither extends an elastic budget nor looks like steady progress.
func TestSampleCacheReusedUnitsConsumeNoBudget(t *testing.T) {
	e := newSampleCacheEnv(t)
	// noteUnit reads the clock exactly once per unit of work, so the clock
	// reads count the units that fed the budget.
	meter := func(reads *int) *searchBudget {
		b := newSearchBudget(time.Now(), 3600, nil)
		b.now = func() time.Time { *reads++; return time.Now() }
		return b
	}
	var coldReads, warmReads int
	cold := meter(&coldReads)
	e.search(func(r *BenchmarkRecord) { r.searchBudget = cold })
	// 2 reference extractions + 8 encodes + 8 VMAF passes.
	if coldReads != 18 {
		t.Fatalf("measured units fed the budget %d times, want 18", coldReads)
	}
	warm := meter(&warmReads)
	record, calls := e.search(func(r *BenchmarkRecord) { r.searchBudget = warm })
	if calls.enc != 0 {
		t.Fatalf("expected a full replay, got %+v", calls)
	}
	// Only the 2 reference extractions, which are still real work.
	if warmReads != 2 {
		t.Fatalf("replayed units fed the budget: %d notes, want 2", warmReads)
	}
	if record.Progress != 99 {
		t.Fatalf("replayed units did not resolve progress: %v", record.Progress)
	}
}

// Holdout windows are ordinary (candidate, window) units, so a candidate
// verified on them is replayed like any other.
func TestSampleCacheHoldoutWindowsAreReused(t *testing.T) {
	e := newSampleCacheEnv(t)
	holdout := func(r *BenchmarkRecord) {
		r.Samples = []transcode.BenchmarkSampleWindow{{Index: 3, StartSeconds: 40, DurationSeconds: 5}, {Index: 4, StartSeconds: 50, DurationSeconds: 5}}
		r.Candidates = r.Candidates[2:3]
	}
	e.search(nil)
	if _, calls := e.search(holdout); calls.enc != 2 {
		t.Fatalf("new windows must be measured: %+v", calls)
	}
	if _, calls := e.search(holdout); calls.enc != 0 {
		t.Fatalf("holdout windows were not replayed: %+v", calls)
	}
}

func TestSampleCacheEvictionAndExpiry(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	c := &sampleCache{dir: dir, maxBytes: 1 << 20, maxEntries: 10, ttl: 24 * time.Hour, now: func() time.Time { return now }}
	keyN := func(n int) string { return fmt.Sprintf("%064x", n+1) }
	age := func(key string, d time.Duration) {
		when := now.Add(-d)
		if err := os.Chtimes(c.path(key), when, when); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(key string) bool { _, err := os.Stat(c.path(key)); return err == nil }
	put := func(n int) string {
		key := keyN(n)
		c.store(key, sampleCacheBody{SizeBytes: 1, EncodeDurationSec: 1})
		return key
	}

	t.Run("entry count bound evicts least recently used first", func(t *testing.T) {
		for i := 0; i < 14; i++ {
			age(put(i), time.Duration(100-i)*time.Minute) // entry 0 is the oldest
		}
		// Using an old entry refreshes it.
		_, _, _ = c.lookup(keyN(0), sampleCacheExpect{})
		c.sweep()
		if !exists(keyN(0)) {
			t.Fatal("a recently used entry was evicted")
		}
		if exists(keyN(1)) {
			t.Fatal("the least recently used entry survived")
		}
		count := 0
		for i := 0; i < 14; i++ {
			if exists(keyN(i)) {
				count++
			}
		}
		if count > 9 {
			t.Fatalf("%d entries survive a bound of 10 (target 90%%)", count)
		}
		if !exists(keyN(13)) {
			t.Fatal("the newest entry was evicted")
		}
	})

	t.Run("byte bound", func(t *testing.T) {
		dir := t.TempDir()
		c := &sampleCache{dir: dir, maxBytes: 1500, maxEntries: 100, ttl: 24 * time.Hour, now: func() time.Time { return now }}
		for i := 0; i < 8; i++ {
			key := keyN(100 + i)
			c.store(key, sampleCacheBody{SizeBytes: 1, EncodeDurationSec: 1})
			when := now.Add(-time.Duration(100-i) * time.Minute)
			_ = os.Chtimes(c.path(key), when, when)
		}
		c.sweep()
		var total int64
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				info, _ := d.Info()
				total += info.Size()
			}
			return nil
		})
		if total > 1500 {
			t.Fatalf("cache is %d bytes after a sweep, bound 1500", total)
		}
		if _, err := os.Stat(c.path(keyN(107))); err != nil {
			t.Fatal("the newest entry was evicted by the byte bound")
		}
	})

	t.Run("expired entries are swept and never hit", func(t *testing.T) {
		key := put(50)
		age(key, 48*time.Hour)
		if _, _, ok := c.lookup(key, sampleCacheExpect{}); ok || exists(key) {
			t.Fatal("an expired entry was served")
		}
		key = put(51)
		age(key, 48*time.Hour)
		c.sweep()
		if exists(key) {
			t.Fatal("an expired entry survived a sweep")
		}
	})

	t.Run("stray files are cleaned", func(t *testing.T) {
		sub := filepath.Join(dir, "ab")
		_ = os.MkdirAll(sub, 0o700)
		stale := filepath.Join(sub, ".tmp-orphan")
		fresh := filepath.Join(sub, ".tmp-live")
		foreign := filepath.Join(sub, "notes.txt")
		for _, p := range []string{stale, fresh, foreign} {
			_ = os.WriteFile(p, []byte("x"), 0o600)
		}
		old := now.Add(-2 * time.Hour)
		_ = os.Chtimes(stale, old, old)
		c.sweep()
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Fatal("an orphaned temp file survived")
		}
		if _, err := os.Stat(fresh); err != nil {
			t.Fatal("a temp file of a write in progress was removed")
		}
		if _, err := os.Stat(foreign); !os.IsNotExist(err) {
			t.Fatal("a foreign file in the cache survived")
		}
	})
}

func TestVMAFStatsFrameScoresRoundTrip(t *testing.T) {
	model, err := quality.Model("v1_1080p_3h")
	if err != nil {
		t.Fatal(err)
	}
	frames := make([]map[string]any, 40)
	for i := range frames {
		frames[i] = map[string]any{"frameNum": i, "metrics": map[string]any{"vmaf": 80 + float64(i%7)*1.3}}
	}
	data, _ := json.Marshal(map[string]any{"frames": frames})
	threshold := 82.0
	stats, err := quality.ParseVMAF(data, MaxMetricLogSizeBytes, model, 2, 10, 25, 1, 1, &threshold)
	if err != nil {
		t.Fatal(err)
	}
	scores := stats.FrameScores()
	decoded, err := decodeFrameScores(encodeFrameScores(scores))
	if err != nil {
		t.Fatal(err)
	}
	persisted, _ := json.Marshal(stats)
	var restored quality.VMAFStats
	if err := json.Unmarshal(persisted, &restored); err != nil {
		t.Fatal(err)
	}
	restored, err = restored.WithFrameScores(decoded)
	if err != nil {
		t.Fatal(err)
	}
	a, err := quality.CombineVMAF([]quality.VMAFStats{stats})
	if err != nil {
		t.Fatal(err)
	}
	b, err := quality.CombineVMAF([]quality.VMAFStats{restored})
	if err != nil {
		t.Fatal(err)
	}
	if a.Mean != b.Mean || a.P5 != b.P5 || a.FramesBelowThreshold != b.FramesBelowThreshold {
		t.Fatalf("restored scores combine differently: %+v vs %+v", a, b)
	}
	// Scores that do not belong to the summary are refused.
	bad := append([]float64(nil), decoded...)
	bad[0] = 12
	if _, err := restored.WithFrameScores(bad); err == nil {
		t.Fatal("scores disagreeing with the summary were accepted")
	}
	if _, err := restored.WithFrameScores(decoded[:10]); err == nil {
		t.Fatal("a short score list was accepted")
	}
	oob := append([]float64(nil), decoded...)
	oob[1] = 1000
	if _, err := restored.WithFrameScores(oob); err == nil {
		t.Fatal("an out-of-range score was accepted")
	}
}

// sampleCacheKeySH builds the pipeline state key() reads, without a search.
func sampleCacheKeySH(t *testing.T, e *sampleCacheEnv, record *BenchmarkRecord) (*pipelineShared, *sampleCacheRun) {
	t.Helper()
	model, err := quality.Model("v1_1080p_3h")
	if err != nil {
		t.Fatal(err)
	}
	c := record.Candidates[0]
	plan := &transcode.Plan{VideoCodec: transcode.BenchmarkCandidateVideoCodec(c), Quality: c.Quality, VideoProfile: "main", PixelFormat: "yuv420p", ExpectedBitDepth: 8}
	sh := &pipelineShared{
		record:         record,
		evidence:       &BenchmarkExecutionEvidence{SourceBitDepth: 8, SourcePixelFormat: "yuv420p", SourceResolution: "1920x1080", SourceFPS: 24},
		vcs:            []validatedCandidate{{index: 0, candidate: c, plan: plan, profile: "main", pixelFormat: "yuv420p", bitDepth: 8}},
		vmafModel:      &model,
		vmafDecimation: 1,
		capFingerprint: "fp",
	}
	return sh, &sampleCacheRun{build: sampleCacheBuildIdentity{FFmpeg: "b"}, source: sampleCacheSource{SHA256: e.sha, Size: 1}}
}

// The compact chain is part of what every reference (hence every candidate
// sample) went through, so a different chain or lookup table must not replay.
func TestSampleCacheKeyCoversCompactChain(t *testing.T) {
	useTempCompactLUTDir(t)
	e := newSampleCacheEnv(t)
	keyFor := func(mutate func(sh *pipelineShared)) string {
		record := e.record()
		sh, run := sampleCacheKeySH(t, e, record)
		if mutate != nil {
			mutate(sh)
		}
		key, _, ok := run.key(sh, pipelineUnit{})
		if !ok {
			t.Fatal("unit could not be keyed")
		}
		return key
	}
	withCompact := func(plan *transcode.CompactPlan, lut string) func(sh *pipelineShared) {
		return func(sh *pipelineShared) {
			digest := lut
			ev, err := compactChainEvidence(plan, digest)
			if err != nil {
				t.Fatal(err)
			}
			sh.record.Compact, sh.evidence.Compact = plan, ev
		}
	}
	plain := keyFor(nil)
	sdr := keyFor(withCompact(compactSDRPlan(), ""))
	if sdr == plain {
		t.Fatal("a compact chain did not change the key")
	}
	if keyFor(withCompact(compactSDRPlan(), "")) != sdr {
		t.Fatal("the key is not deterministic")
	}
	resized := compactSDRPlan()
	resized.MaxHeight, resized.TargetWidth, resized.TargetHeight = 720, 1280, 720
	if keyFor(withCompact(resized, "")) == sdr {
		t.Fatal("a different target size did not change the key")
	}
	lutA := strings.Repeat("a", 64)
	lutB := strings.Repeat("b", 64)
	if keyFor(withCompact(compactPQPlan(), lutA)) == keyFor(withCompact(compactPQPlan(), lutB)) {
		t.Fatal("a different tone-map table did not change the key")
	}
	// The label of a candidate names its result, not its encode.
	if keyFor(func(sh *pipelineShared) { sh.vcs[0].candidate.ID = "other" }) != plain {
		t.Fatal("a candidate label changed the key")
	}
}
