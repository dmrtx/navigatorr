package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

// resolvePipelineConcurrency resolves bounded encode/metric worker counts.
// Nil Concurrency or zero values select conservative defaults (2 encoders,
// 2 metrics); explicit values are clamped defensively to [1, MaxBenchmarkConcurrency].
func resolvePipelineConcurrency(record *BenchmarkRecord) (encodeN, metricN int) {
	encodeN, metricN = transcode.DefaultEncodeConcurrency, transcode.DefaultMetricConcurrency
	if record != nil && record.Concurrency != nil {
		if record.Concurrency.EncodeConcurrency > 0 {
			encodeN = record.Concurrency.EncodeConcurrency
		}
		if record.Concurrency.MetricConcurrency > 0 {
			metricN = record.Concurrency.MetricConcurrency
		}
	}
	if encodeN < 1 {
		encodeN = 1
	}
	if metricN < 1 {
		metricN = 1
	}
	if encodeN > transcode.MaxBenchmarkConcurrency {
		encodeN = transcode.MaxBenchmarkConcurrency
	}
	if metricN > transcode.MaxBenchmarkConcurrency {
		metricN = transcode.MaxBenchmarkConcurrency
	}
	return encodeN, metricN
}

// pipelineUnit identifies one (candidate, sample) work item in deterministic order.
type pipelineUnit struct {
	vcOrd   int // position in the vcs slice passed to the executor
	sampOrd int // position in record.Samples
}

// pipelineEncodeOutcome is the encode result for one unit.
type pipelineEncodeOutcome struct {
	entry BenchmarkCandidateSampleResult
	err   error // job-fatal encode error, if any
}

// pipelineMetricOutcome is the metric result for one unit.
type pipelineMetricOutcome struct {
	executed    bool
	vmaf        *float64
	vmafStats   *quality.VMAFStats
	cambi       *quality.CAMBIStats
	cambiErr    string
	cambiDurSec float64
	ssim        *float64
	vmafDurSec  float64
	ssimDurSec  float64
	vmafScores  []optimization.SampleScore
	ssimScores  []optimization.SampleScore
	entry       *BenchmarkMetricSampleResult
}

// candidatePoison tracks candidate-level metric failure across concurrent units.
// The first failure wins deterministically enough: any failure marks the whole
// candidate ineligible via an invalid aggregate, matching sequential semantics
// where one bad sample poisons the candidate's remaining samples.
type candidatePoison struct {
	mu     sync.Mutex
	failed bool
	reason string
}

func (c *candidatePoison) isFailed() (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failed, c.reason
}

func (c *candidatePoison) setFailed(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.failed {
		c.failed = true
		c.reason = reason
	}
}

// pipelineFatal records the first job-fatal error and stops remaining work
// from starting. It deliberately does NOT cancel the pipeline context: every
// unit runs its ffmpeg invocation via exec.CommandContext on that context, so
// cancelling it would SIGKILL in-flight sibling encodes/metrics whose outcomes
// would then be discarded, leaving nondeterministic partial evidence behind.
// In-flight units always run to completion and record their outcomes; the
// feeder and encode workers consult failed() to avoid starting new units after
// a fatal error. Caller cancellation still flows through the parent context
// and aborts promptly via the ctx.Done/ctx.Err() checks in the worker loops.
// A mutex (not sync.Once) guards the fields because readers use get() rather
// than Once.Do, so Once alone cannot order the handoff to the joining goroutine.
type pipelineFatal struct {
	mu     sync.Mutex
	err    error
	metric bool // true when the fatal error came from the metric stage
}

func (f *pipelineFatal) set(err error, metric bool) {
	if err == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return
	}
	f.err = err
	f.metric = metric
}

func (f *pipelineFatal) get() (error, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err, f.metric
}

func (f *pipelineFatal) failed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err != nil
}

// pipelineShared carries read-mostly execution context plus shared pipeline state.
type pipelineShared struct {
	w                    *Worker
	record               *BenchmarkRecord
	evidence             *BenchmarkExecutionEvidence
	samplesDir           string
	vcs                  []validatedCandidate
	sourceInitialSize    int64
	sourceInitialModTime time.Time
	progressReporter     *benchmarkProgressReporter
	runVMAF              bool
	runSSIM              bool
	runCAMBI             bool
	vmafModel            *quality.VMAFModel
	qualityCaps          *transcode.QualityCapabilities
	capFingerprint       string
	passesPerSample      int
	poisons              []*candidatePoison
	encodeOut            []pipelineEncodeOutcome
	metricOut            []pipelineMetricOutcome
	fatal                *pipelineFatal
}

// runPipelinedEncodeMetrics executes encode and metric work for the given candidate
// set with bounded concurrency and safe encode->metric pipelining: as soon as one
// sample encode completes, its metric evaluation may begin on the metric stage
// while other sample encodes continue on the encode stage.
//
// Evidence is assembled deterministically in (candidate, sample) order after all
// workers join, so concurrent completion order never affects reported results or
// final selection. Progress uses the shared full-plan reporter (thread-safe) so it
// stays monotonic and capped below 100 while running.
//
// Encode errors are job-fatal and returned raw (matching the sequential encode
// loop); metric-stage job-fatal errors are wrapped as "metrics calculation failed".
// Candidate-level metric failures mark that candidate ineligible without aborting
// siblings, matching sequential runMetrics semantics.
func (r *ProductionBenchmarkRunner) runPipelinedEncodeMetrics(
	ctx context.Context,
	w *Worker,
	record *BenchmarkRecord,
	evidence *BenchmarkExecutionEvidence,
	samplesDir string,
	vcs []validatedCandidate,
	sourceInitialSize int64,
	sourceInitialModTime time.Time,
	progressReporter *benchmarkProgressReporter,
) error {
	if len(vcs) == 0 {
		return errors.New("no validated candidates for pipelined evaluation (fail closed)")
	}
	if len(record.Samples) == 0 {
		return errors.New("no benchmark samples for pipelined evaluation (fail closed)")
	}

	normMetric := strings.ToLower(strings.TrimSpace(record.Metric))
	if normMetric == "" {
		normMetric = "vmaf"
	}
	needQualityProbe := record.Quality != nil && ((record.Quality.VMAF != nil && record.Quality.VMAF.Model != "") || (record.Quality.Banding != nil && record.Quality.Banding.Enabled))
	caps, err := probeWorkerCapabilitiesWithScratch(ctx, w.ffmpegPath, filepath.Join(w.cfg.StateDir, "quality-probe-scratch"), needQualityProbe)
	if err != nil {
		return fmt.Errorf("probing worker capabilities: %w (fail closed)", err)
	}
	switch normMetric {
	case "vmaf":
		if !caps.Filters["libvmaf"] {
			return errors.New("required filter 'libvmaf' is not available on worker (fail closed)")
		}
	case "ssim":
		if !caps.Filters["ssim"] {
			return errors.New("required filter 'ssim' is not available on worker (fail closed)")
		}
	case "both", "vmaf+ssim":
		if !caps.Filters["libvmaf"] {
			return errors.New("required filter 'libvmaf' is not available on worker (fail closed)")
		}
		if !caps.Filters["ssim"] {
			return errors.New("required filter 'ssim' is not available on worker (fail closed)")
		}
	default:
		return fmt.Errorf("unsupported benchmark metric %q (fail closed)", record.Metric)
	}
	runVMAF := normMetric == "vmaf" || normMetric == "both" || normMetric == "vmaf+ssim"
	runSSIM := normMetric == "ssim" || normMetric == "both" || normMetric == "vmaf+ssim"
	runCAMBI := record.Quality != nil && record.Quality.Banding != nil && record.Quality.Banding.Enabled
	var vmafModel *quality.VMAFModel
	if runVMAF && record.Quality != nil && record.Quality.VMAF != nil && record.Quality.VMAF.Model != "" {
		model, err := quality.Model(record.Quality.VMAF.Model)
		if err != nil {
			return err
		}
		if caps.Quality == nil || caps.Quality.ProbeError != "" || !caps.Quality.Models[model.ID].Available {
			return fmt.Errorf("quality_vmaf_model_unavailable: model %s is not executable on worker (fail closed)", model.ID)
		}
		vmafModel = &model
	}
	if runCAMBI && (caps.Quality == nil || caps.Quality.ProbeError != "" || !caps.Quality.CAMBIFullRef) {
		return fmt.Errorf("quality_cambi_full_ref_unavailable: CAMBI full-reference is not executable on worker (fail closed)")
	}
	passesPerSample := 0
	if runVMAF {
		passesPerSample++
	}
	if runSSIM {
		passesPerSample++
	}
	if runCAMBI {
		passesPerSample++
	}

	// 10-bit media validation, matching sequential runMetrics fail-closed behavior.
	if evidence.SourceBitDepth > 8 && runVMAF {
		return fmt.Errorf("worker capability unsupported: source media has bit depth %d (> 8-bit) but worker does not have verified 10-bit VMAF capability; silent 8-bit downconversion is prohibited (fail closed)", evidence.SourceBitDepth)
	}
	if evidence.SourceBitDepth > 8 && runCAMBI {
		return fmt.Errorf("quality_cambi_native_main10_unverified: native %d-bit source is not eligible for CAMBI (fail closed)", evidence.SourceBitDepth)
	}

	encodeN, metricN := resolvePipelineConcurrency(record)

	nUnits := len(vcs) * len(record.Samples)
	units := make([]pipelineUnit, 0, nUnits)
	for vcOrd := range vcs {
		for sampOrd := range record.Samples {
			units = append(units, pipelineUnit{vcOrd: vcOrd, sampOrd: sampOrd})
		}
	}

	// Derived context only ties worker commands to caller cancellation; a
	// job-fatal unit error never cancels it so in-flight sibling units always
	// finish and record their outcomes (deterministic partial evidence).
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	fatal := &pipelineFatal{}
	poisons := make([]*candidatePoison, len(vcs))
	for i := range poisons {
		poisons[i] = &candidatePoison{}
	}
	sh := &pipelineShared{
		w:                    w,
		record:               record,
		evidence:             evidence,
		samplesDir:           samplesDir,
		vcs:                  vcs,
		sourceInitialSize:    sourceInitialSize,
		sourceInitialModTime: sourceInitialModTime,
		progressReporter:     progressReporter,
		runVMAF:              runVMAF,
		runSSIM:              runSSIM,
		runCAMBI:             runCAMBI,
		vmafModel:            vmafModel,
		qualityCaps:          caps.Quality,
		capFingerprint:       caps.CapabilityFingerprint,
		passesPerSample:      passesPerSample,
		poisons:              poisons,
		encodeOut:            make([]pipelineEncodeOutcome, nUnits),
		metricOut:            make([]pipelineMetricOutcome, nUnits),
		fatal:                fatal,
	}

	encQ := make(chan int, nUnits)
	metQ := make(chan int, nUnits)

	// Feeder enqueues units in deterministic order; stops early on caller
	// cancellation or the first job-fatal error so no new work starts after
	// the job is already doomed. In-flight units are never interrupted.
	go func() {
		defer close(encQ)
		for i := range units {
			if fatal.failed() {
				return
			}
			select {
			case <-ctx.Done():
				return
			case encQ <- i:
			}
		}
	}()

	var encWG sync.WaitGroup
	for i := 0; i < encodeN; i++ {
		encWG.Add(1)
		go func() {
			defer encWG.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case idx, ok := <-encQ:
					if !ok {
						return
					}
					if ctx.Err() != nil {
						return
					}
					if fatal.failed() {
						return
					}
					sh.runEncodeUnit(ctx, units[idx], idx)
					if ctx.Err() != nil {
						return
					}
					select {
					case <-ctx.Done():
						return
					case metQ <- idx:
					}
				}
			}
		}()
	}

	// Close the metric queue once every encode worker has exited, so metric
	// workers always terminate even when encodes were skipped after cancel.
	go func() {
		encWG.Wait()
		close(metQ)
	}()

	var metWG sync.WaitGroup
	for i := 0; i < metricN; i++ {
		metWG.Add(1)
		go func() {
			defer metWG.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case idx, ok := <-metQ:
					if !ok {
						return
					}
					if ctx.Err() != nil {
						return
					}
					sh.runMetricUnit(ctx, units[idx], idx)
				}
			}
		}()
	}
	metWG.Wait()

	if ferr, metric := fatal.get(); ferr != nil {
		sh.assembleEvidence(units)
		if metric {
			return fmt.Errorf("metrics calculation failed: %w", ferr)
		}
		return ferr
	}
	sh.assembleEvidence(units)
	return nil
}

// runEncodeUnit encodes one (candidate, sample) unit. Any error is job-fatal,
// matching the sequential encode loop.
func (sh *pipelineShared) runEncodeUnit(ctx context.Context, u pipelineUnit, idx int) {
	vc := sh.vcs[u.vcOrd]
	window := sh.record.Samples[u.sampOrd]
	fail := func(err error) {
		sh.fatal.set(err, false)
	}

	if ctx.Err() != nil {
		fail(ctx.Err())
		return
	}
	sh.progressReporter.StartSampleUnit("encoding_candidates", u.sampOrd+1, vc.index+1, vc.candidate.ID, "")
	if err := verifySourceUnchanged(sh.record.Source, sh.sourceInitialSize, sh.sourceInitialModTime); err != nil {
		fail(err)
		return
	}
	refPath := filepath.Join(sh.samplesDir, "ref_sample_"+strconv.Itoa(window.Index)+".mkv")
	if _, err := os.Stat(refPath); err != nil {
		fail(fmt.Errorf("reference sample %d not found at %s: %w", window.Index, refPath, err))
		return
	}
	candFileName := vc.fileKey + "_sample_" + strconv.Itoa(window.Index) + ".mkv"
	candPath := filepath.Join(sh.samplesDir, candFileName)
	if err := verifyChildPath(sh.samplesDir, candPath); err != nil {
		fail(fmt.Errorf("invalid candidate sample path: %w", err))
		return
	}
	if err := prepareOutputFile(candPath); err != nil {
		fail(err)
		return
	}
	candArgs, err := BuildCandidateEncodeArgs(refPath, candPath, vc.plan)
	if err != nil {
		fail(fmt.Errorf("building candidate %q args: %w", vc.candidate.ID, err))
		return
	}
	start := time.Now()
	cmd := exec.CommandContext(ctx, sh.w.ffmpegPath, candArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	stderrBuf := newBoundedBuffer(16 * 1024)
	cmd.Stderr = stderrBuf
	cmd.Stdout = nil
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			fail(ctx.Err())
			return
		}
		errStr := fmt.Sprintf("encoding candidate %q for sample %d failed: %v: %s",
			vc.candidate.ID, window.Index, err, boundedStderr(stderrBuf, 1024))
		sh.encodeOut[idx] = pipelineEncodeOutcome{
			entry: BenchmarkCandidateSampleResult{
				CandidateID:        vc.candidate.ID,
				SampleIndex:        window.Index,
				File:               filepath.Base(candPath),
				Quality:            vc.candidate.Quality,
				AverageBitrateKbps: vc.candidate.AverageBitrateKbps,
				VideoProfile:       vc.profile,
				PixelFormat:        vc.pixelFormat,
				Error:              errStr,
			},
			err: errors.New(errStr),
		}
		fail(sh.encodeOut[idx].err)
		return
	}
	elapsed := time.Since(start).Seconds()
	fi, err := os.Stat(candPath)
	if err != nil || fi.Size() == 0 {
		errStr := fmt.Sprintf("candidate %q for sample %d produced empty or missing file at %s", vc.candidate.ID, window.Index, candPath)
		sh.encodeOut[idx] = pipelineEncodeOutcome{
			entry: BenchmarkCandidateSampleResult{
				CandidateID:        vc.candidate.ID,
				SampleIndex:        window.Index,
				File:               filepath.Base(candPath),
				Quality:            vc.candidate.Quality,
				AverageBitrateKbps: vc.candidate.AverageBitrateKbps,
				VideoProfile:       vc.profile,
				PixelFormat:        vc.pixelFormat,
				Error:              errStr,
			},
			err: errors.New(errStr),
		}
		fail(sh.encodeOut[idx].err)
		return
	}
	sh.encodeOut[idx] = pipelineEncodeOutcome{
		entry: BenchmarkCandidateSampleResult{
			CandidateID:        vc.candidate.ID,
			SampleIndex:        window.Index,
			File:               filepath.Base(candPath),
			SizeBytes:          fi.Size(),
			EncodeDurationSec:  elapsed,
			Quality:            vc.candidate.Quality,
			AverageBitrateKbps: vc.candidate.AverageBitrateKbps,
			VideoProfile:       vc.profile,
			PixelFormat:        vc.pixelFormat,
		},
	}
	sh.progressReporter.CompleteUnit()
}

// runMetricUnit evaluates metrics for one unit whose encode already completed.
// Job-fatal conditions (cancellation, source tampering, missing reference)
// abort the job; candidate-level failures poison only that candidate, matching
// sequential runMetrics fallback/safety behavior.
func (sh *pipelineShared) runMetricUnit(ctx context.Context, u pipelineUnit, idx int) {
	vc := sh.vcs[u.vcOrd]
	window := sh.record.Samples[u.sampOrd]
	candIdx := vc.index
	fail := func(err error) {
		sh.fatal.set(err, true)
	}

	if ctx.Err() != nil {
		fail(ctx.Err())
		return
	}
	if err := verifySourceUnchanged(sh.record.Source, sh.sourceInitialSize, sh.sourceInitialModTime); err != nil {
		fail(fmt.Errorf("source media modified during benchmark (integrity violation, fail closed): %w", err))
		return
	}
	refPath := filepath.Join(sh.samplesDir, "ref_sample_"+strconv.Itoa(window.Index)+".mkv")
	refStat, err := os.Stat(refPath)
	if err != nil || !refStat.Mode().IsRegular() || refStat.Size() == 0 {
		fail(fmt.Errorf("lossless reference sample %s missing or invalid (job-fatal integrity violation, fail closed)", refPath))
		return
	}
	candPath := filepath.Join(sh.samplesDir, vc.fileKey+"_sample_"+strconv.Itoa(window.Index)+".mkv")

	poisoned, reason := sh.poisons[u.vcOrd].isFailed()
	candStat, statErr := os.Stat(candPath)
	if statErr != nil || !candStat.Mode().IsRegular() || candStat.Size() == 0 {
		reason = "candidate sample " + candPath + " missing or invalid"
		sh.poisons[u.vcOrd].setFailed(reason)
		poisoned = true
	}
	if poisoned {
		if reason == "" {
			_, reason = sh.poisons[u.vcOrd].isFailed()
		}
		sh.metricOut[idx] = invalidMetricOutcome(vc.candidate.ID, candIdx, window.Index, reason, sh.runVMAF, sh.runSSIM)
		sh.progressReporter.SkipUnits(sh.passesPerSample)
		return
	}

	var vmafScore *float64
	var vmafStats *quality.VMAFStats
	var ssimScore *float64
	var vmafDurationSec float64
	var ssimDurationSec float64
	candidateFailed := false
	var candidateFailReason string

	if sh.runVMAF {
		logPath, err := derivedMetricLogPath(sh.samplesDir, "vmaf", candIdx, vc.candidate.ID, window.Index)
		if err != nil {
			fail(err)
			return
		}
		if err := prepareOutputFile(logPath); err != nil {
			fail(fmt.Errorf("preparing vmaf log path %s: %w", logPath, err))
			return
		}
		sh.progressReporter.StartSampleUnit("evaluating_metrics", u.sampOrd+1, vc.index+1, vc.candidate.ID, "vmaf")
		args := BuildVMAFArgs(candPath, refPath, logPath)
		if sh.vmafModel != nil {
			args, err = BuildVMAFModelArgs(candPath, refPath, logPath, *sh.vmafModel)
			if err != nil {
				fail(err)
				return
			}
		}
		cmd := exec.CommandContext(ctx, sh.w.ffmpegPath, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			if cmd.Process != nil && cmd.Process.Pid > 0 {
				return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			return nil
		}
		cmd.WaitDelay = 2 * time.Second
		stderrBuf := newTailBuffer(64 * 1024)
		cmd.Stderr = stderrBuf
		cmd.Stdout = nil
		start := time.Now()
		runErr := cmd.Run()
		vmafDurationSec = time.Since(start).Seconds()
		if ctx.Err() != nil {
			fail(ctx.Err())
			return
		}
		if runErr != nil {
			candidateFailed = true
			candidateFailReason = fmt.Sprintf("ffmpeg vmaf failed for cand %q sample %d: %v; stderr: %s",
				vc.candidate.ID, window.Index, runErr, stderrBuf.String())
		} else {
			logData, err := readMetricLogFile(logPath)
			if err != nil {
				candidateFailed = true
				candidateFailReason = fmt.Sprintf("read vmaf log for cand %q sample %d: %v", vc.candidate.ID, window.Index, err)
			} else {
				var score float64
				if sh.vmafModel != nil {
					windowSec := sh.record.Quality.VMAF.WorstWindowSeconds
					stats, parseErr := quality.ParseVMAF(logData, MaxMetricLogSizeBytes, *sh.vmafModel, window.Index, window.StartSeconds, sh.evidence.SourceFPS, windowSec, sh.record.Quality.VMAF.FrameThreshold)
					err = parseErr
					if err == nil {
						score = stats.Mean
						vmafStats = &stats
					}
				} else {
					score, err = ParseVMAFJSON(logData)
				}
				if err != nil {
					candidateFailed = true
					candidateFailReason = fmt.Sprintf("parse vmaf log for cand %q sample %d: %v", vc.candidate.ID, window.Index, err)
				} else {
					vmafScore = &score
				}
			}
		}
		sh.progressReporter.ResolveUnit()
		if candidateFailed {
			sh.poisons[u.vcOrd].setFailed(candidateFailReason)
			out := pipelineMetricOutcome{executed: true, vmafDurSec: vmafDurationSec}
			out.vmafScores = append(out.vmafScores, optimization.SampleScore{
				SampleIndex: window.Index,
				Valid:       false,
				Error:       candidateFailReason,
			})
			if sh.runSSIM {
				out.ssimScores = append(out.ssimScores, optimization.SampleScore{
					SampleIndex: window.Index,
					Valid:       false,
					Error:       candidateFailReason,
				})
				sh.progressReporter.SkipUnits(1)
			}
			if sh.runCAMBI {
				sh.progressReporter.SkipUnits(1)
			}
			out.entry = &BenchmarkMetricSampleResult{
				CandidateID:       vc.candidate.ID,
				CandidateIndex:    candIdx,
				SampleIndex:       window.Index,
				VMAFDurationSec:   vmafDurationSec,
				MetricDurationSec: vmafDurationSec,
				Error:             candidateFailReason,
			}
			sh.metricOut[idx] = out
			return
		}
		// Record the valid VMAF sample score now; the SSIM branch appends below.
		sh.metricOut[idx].vmafScores = append(sh.metricOut[idx].vmafScores, optimization.SampleScore{
			SampleIndex: window.Index,
			Score:       *vmafScore,
			Valid:       true,
		})
		sh.metricOut[idx].vmaf = vmafScore
		sh.metricOut[idx].vmafStats = vmafStats
		sh.metricOut[idx].vmafDurSec = vmafDurationSec
	}

	if sh.runSSIM {
		statsPath, err := derivedMetricLogPath(sh.samplesDir, "ssim", candIdx, vc.candidate.ID, window.Index)
		if err != nil {
			fail(err)
			return
		}
		if err := prepareOutputFile(statsPath); err != nil {
			fail(fmt.Errorf("preparing ssim stats path %s: %w", statsPath, err))
			return
		}
		sh.progressReporter.StartSampleUnit("evaluating_metrics", u.sampOrd+1, vc.index+1, vc.candidate.ID, "ssim")
		args := BuildSSIMArgs(candPath, refPath, statsPath)
		cmd := exec.CommandContext(ctx, sh.w.ffmpegPath, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			if cmd.Process != nil && cmd.Process.Pid > 0 {
				return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			return nil
		}
		cmd.WaitDelay = 2 * time.Second
		stderrBuf := newTailBuffer(64 * 1024)
		cmd.Stderr = stderrBuf
		cmd.Stdout = nil
		start := time.Now()
		runErr := cmd.Run()
		ssimDurationSec = time.Since(start).Seconds()
		if ctx.Err() != nil {
			fail(ctx.Err())
			return
		}
		if runErr != nil {
			candidateFailed = true
			candidateFailReason = fmt.Sprintf("ffmpeg ssim failed for cand %q sample %d: %v; stderr: %s",
				vc.candidate.ID, window.Index, runErr, stderrBuf.String())
		} else {
			var score float64
			statsData, err := readMetricLogFile(statsPath)
			if err == nil {
				score, err = ParseSSIMStatsFile(statsData)
			}
			if err != nil {
				score, err = ParseSSIMStderr(stderrBuf.String())
			}
			if err != nil {
				candidateFailed = true
				candidateFailReason = fmt.Sprintf("read/parse ssim stats for cand %q sample %d: %v", vc.candidate.ID, window.Index, err)
			} else {
				ssimScore = &score
			}
		}
		sh.progressReporter.ResolveUnit()
		if candidateFailed {
			sh.poisons[u.vcOrd].setFailed(candidateFailReason)
			out := sh.metricOut[idx]
			out.executed = true
			out.vmaf = vmafScore
			out.vmafStats = vmafStats
			out.vmafDurSec = vmafDurationSec
			out.ssimDurSec = ssimDurationSec
			// Preserve any valid VMAF score recorded above.
			if sh.runVMAF && vmafScore != nil && len(out.vmafScores) == 0 {
				out.vmafScores = append(out.vmafScores, optimization.SampleScore{
					SampleIndex: window.Index,
					Score:       *vmafScore,
					Valid:       true,
				})
			}
			out.ssimScores = append(out.ssimScores, optimization.SampleScore{
				SampleIndex: window.Index,
				Valid:       false,
				Error:       candidateFailReason,
			})
			out.entry = &BenchmarkMetricSampleResult{
				CandidateID:       vc.candidate.ID,
				CandidateIndex:    candIdx,
				SampleIndex:       window.Index,
				VMAF:              vmafScore,
				VMAFStats:         vmafStats,
				VMAFDurationSec:   vmafDurationSec,
				SSIMDurationSec:   ssimDurationSec,
				MetricDurationSec: vmafDurationSec + ssimDurationSec,
				Error:             candidateFailReason,
			}
			sh.metricOut[idx] = out
			if sh.runCAMBI {
				sh.progressReporter.SkipUnits(1)
			}
			return
		}
		sh.metricOut[idx].ssimScores = append(sh.metricOut[idx].ssimScores, optimization.SampleScore{
			SampleIndex: window.Index,
			Score:       *ssimScore,
			Valid:       true,
		})
		sh.metricOut[idx].ssim = ssimScore
		sh.metricOut[idx].ssimDurSec = ssimDurationSec
	}

	out := sh.metricOut[idx]
	if sh.runCAMBI {
		sh.progressReporter.StartSampleUnit("evaluating_metrics", u.sampOrd+1, vc.index+1, vc.candidate.ID, "cambi_full_ref")
		start := time.Now()
		stats, err := runCAMBIMetricSample(ctx, sh.w.ffmpegPath, sh.samplesDir, candPath, refPath, candIdx, vc.candidate.ID, window.Index, window.StartSeconds, sh.evidence.SourceFPS, sh.qualityCaps.CAMBIOutput)
		out.cambiDurSec = time.Since(start).Seconds()
		sh.progressReporter.ResolveUnit()
		if ctx.Err() != nil {
			fail(ctx.Err())
			return
		}
		if err != nil {
			out.cambiErr = err.Error()
		} else {
			out.cambi = &stats
		}
	}
	out.executed = true
	out.vmaf = vmafScore
	out.vmafStats = vmafStats
	out.ssim = ssimScore
	out.vmafDurSec = vmafDurationSec
	out.ssimDurSec = ssimDurationSec
	// Valid VMAF scores were recorded incrementally above; ensure presence when
	// only SSIM ran (runVMAF false leaves vmafScores empty, as in sequential code).
	out.entry = &BenchmarkMetricSampleResult{
		CandidateID:       vc.candidate.ID,
		CandidateIndex:    candIdx,
		SampleIndex:       window.Index,
		VMAF:              vmafScore,
		VMAFStats:         vmafStats,
		CAMBI:             out.cambi,
		CAMBIError:        out.cambiErr,
		CAMBIDurationSec:  out.cambiDurSec,
		SSIM:              ssimScore,
		VMAFDurationSec:   vmafDurationSec,
		SSIMDurationSec:   ssimDurationSec,
		MetricDurationSec: vmafDurationSec + ssimDurationSec + out.cambiDurSec,
	}
	sh.metricOut[idx] = out
}

// invalidMetricOutcome builds the poisoned-window outcome: invalid sample scores
// for each enabled metric plus an error metric entry, matching sequential behavior.
func invalidMetricOutcome(candidateID string, candIdx, sampleIdx int, reason string, runVMAF, runSSIM bool) pipelineMetricOutcome {
	out := pipelineMetricOutcome{executed: true}
	if runVMAF {
		out.vmafScores = append(out.vmafScores, optimization.SampleScore{
			SampleIndex: sampleIdx,
			Valid:       false,
			Error:       reason,
		})
	}
	if runSSIM {
		out.ssimScores = append(out.ssimScores, optimization.SampleScore{
			SampleIndex: sampleIdx,
			Valid:       false,
			Error:       reason,
		})
	}
	out.entry = &BenchmarkMetricSampleResult{
		CandidateID:    candidateID,
		CandidateIndex: candIdx,
		SampleIndex:    sampleIdx,
		Error:          reason,
	}
	return out
}

func runCAMBIMetricSample(ctx context.Context, ffmpegPath, samplesDir, candPath, refPath string, candIdx int, candidateID string, sampleIdx int, sourceStartSec, fps float64, outputMetric string) (quality.CAMBIStats, error) {
	logPath, err := derivedMetricLogPath(samplesDir, "cambi", candIdx, candidateID, sampleIdx)
	if err != nil {
		return quality.CAMBIStats{}, err
	}
	if err := prepareOutputFile(logPath); err != nil {
		return quality.CAMBIStats{}, err
	}
	args := BuildCAMBIArgs(candPath, refPath, logPath)
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	stderrBuf := newTailBuffer(64 * 1024)
	cmd.Stderr = stderrBuf
	if err := cmd.Run(); err != nil {
		return quality.CAMBIStats{}, fmt.Errorf("ffmpeg CAMBI full-reference failed: %v: %s", err, stderrBuf.String())
	}
	data, err := readMetricLogFile(logPath)
	if err != nil {
		return quality.CAMBIStats{}, err
	}
	return quality.ParseCAMBIFullReference(data, MaxMetricLogSizeBytes, outputMetric, sampleIdx, sourceStartSec, fps)
}

// assembleEvidence persists per-unit outcomes into evidence in deterministic
// (candidate, sample) order, backfills metric scores onto candidate samples,
// and records per-candidate aggregates in deterministic order. It runs once on
// the calling goroutine after all workers join, so completion order is invisible.
func (sh *pipelineShared) assembleEvidence(units []pipelineUnit) {
	for _, u := range units {
		idx := u.vcOrd*len(sh.record.Samples) + u.sampOrd
		if idx < 0 || idx >= len(sh.encodeOut) {
			continue
		}
		enc := sh.encodeOut[idx]
		// Zero-value entries (unit never started because a fatal error or
		// caller cancellation stopped new work) are skipped; every started
		// unit records its outcome, so completed partial evidence is never
		// lost to a sibling unit's failure.
		if enc.entry.CandidateID == "" {
			continue
		}
		sh.evidence.CandidateSamples = append(sh.evidence.CandidateSamples, enc.entry)
	}
	for _, u := range units {
		idx := u.vcOrd*len(sh.record.Samples) + u.sampOrd
		if idx < 0 || idx >= len(sh.metricOut) {
			continue
		}
		met := sh.metricOut[idx]
		if !met.executed || met.entry == nil {
			continue
		}
		sh.evidence.MetricSamples = append(sh.evidence.MetricSamples, *met.entry)
	}
	// Backfill metric scores onto candidate sample entries by (candidate, sample).
	for i := range sh.evidence.CandidateSamples {
		cs := &sh.evidence.CandidateSamples[i]
		if cs.Error != "" {
			continue
		}
		for j := range sh.evidence.MetricSamples {
			ms := &sh.evidence.MetricSamples[j]
			if ms.CandidateID == cs.CandidateID && ms.SampleIndex == cs.SampleIndex && ms.Error == "" {
				cs.VMAF = ms.VMAF
				cs.SSIM = ms.SSIM
				cs.MetricDurationSec = ms.MetricDurationSec
				break
			}
		}
	}
	// Per-candidate aggregates in deterministic candidate order.
	for vcOrd, vc := range sh.vcs {
		var vmafScores []optimization.SampleScore
		var ssimScores []optimization.SampleScore
		for sampOrd := range sh.record.Samples {
			idx := vcOrd*len(sh.record.Samples) + sampOrd
			if idx < 0 || idx >= len(sh.metricOut) {
				continue
			}
			met := sh.metricOut[idx]
			if !met.executed {
				continue
			}
			vmafScores = append(vmafScores, met.vmafScores...)
			ssimScores = append(ssimScores, met.ssimScores...)
		}
		qualityEvidence, rejectReason := sh.evaluateCandidateQuality(vcOrd, vc.candidate.ID)
		if qualityEvidence != nil {
			sh.evidence.Quality = append(sh.evidence.Quality, *qualityEvidence)
		}
		// A candidate with no executed metric units (cancelled mid-flight) gets an
		// invalid aggregate rather than a fabricated one; the job fails regardless.
		if sh.runVMAF {
			var agg optimization.MetricAggregate
			if vmafScores == nil {
				agg = optimization.MetricAggregate{
					MetricType:       optimization.MetricTypeVMAF,
					Valid:            false,
					IneligibleReason: optimization.ReasonIncompleteSampleScores,
				}
			} else {
				agg = optimization.AggregateSampleScores(optimization.MetricTypeVMAF, vmafScores)
			}
			if rejectReason != "" {
				agg.Valid = false
				agg.IneligibleReason = rejectReason
			}
			sh.evidence.CandidateMetrics = append(sh.evidence.CandidateMetrics, BenchmarkCandidateMetricAggregate{
				CandidateID:    vc.candidate.ID,
				CandidateIndex: vc.index,
				MetricType:     optimization.MetricTypeVMAF,
				Aggregate:      agg,
			})
		}
		if sh.runSSIM {
			var agg optimization.MetricAggregate
			if ssimScores == nil {
				agg = optimization.MetricAggregate{
					MetricType:       optimization.MetricTypeSSIM,
					Valid:            false,
					IneligibleReason: optimization.ReasonIncompleteSampleScores,
				}
			} else {
				agg = optimization.AggregateSampleScores(optimization.MetricTypeSSIM, ssimScores)
			}
			if rejectReason != "" {
				agg.Valid = false
				agg.IneligibleReason = rejectReason
			}
			sh.evidence.CandidateMetrics = append(sh.evidence.CandidateMetrics, BenchmarkCandidateMetricAggregate{
				CandidateID:    vc.candidate.ID,
				CandidateIndex: vc.index,
				MetricType:     optimization.MetricTypeSSIM,
				Aggregate:      agg,
			})
		}
	}
}

func (sh *pipelineShared) evaluateCandidateQuality(vcOrd int, candidateID string) (*CandidateQualityEvidence, string) {
	if sh.vmafModel == nil && !sh.runCAMBI {
		return nil, ""
	}
	e := &CandidateQualityEvidence{CandidateID: candidateID, CapabilityFingerprint: sh.capFingerprint, Verdict: "pass"}
	var vmafSamples []quality.VMAFStats
	var cambiSamples []quality.CAMBIStats
	for sampOrd := range sh.record.Samples {
		idx := vcOrd*len(sh.record.Samples) + sampOrd
		if idx >= len(sh.metricOut) {
			continue
		}
		met := sh.metricOut[idx]
		if met.vmafStats != nil {
			vmafSamples = append(vmafSamples, *met.vmafStats)
		}
		if met.cambi != nil {
			cambiSamples = append(cambiSamples, *met.cambi)
		}
	}
	var reject string
	if sh.vmafModel != nil {
		if len(vmafSamples) != len(sh.record.Samples) {
			e.ReasonCodes = append(e.ReasonCodes, "quality_vmaf_evidence_incomplete")
		}
		if len(vmafSamples) == len(sh.record.Samples) {
			combined, err := quality.CombineVMAF(vmafSamples)
			if err != nil {
				e.ReasonCodes = append(e.ReasonCodes, "quality_vmaf_evidence_incomplete")
			} else {
				e.VMAF = &combined
				policy := sh.record.Quality.VMAF
				if policy.P5Minimum != nil && combined.P5 < *policy.P5Minimum {
					e.ReasonCodes = append(e.ReasonCodes, "quality_vmaf_p5_below_minimum")
				}
				if policy.WorstWindowMinimum != nil && combined.WorstWindowMean < *policy.WorstWindowMinimum {
					e.ReasonCodes = append(e.ReasonCodes, "quality_vmaf_worst_window_below_minimum")
				}
				if policy.MaxFramesBelowThreshold != nil && combined.FramesBelowThreshold > *policy.MaxFramesBelowThreshold {
					e.ReasonCodes = append(e.ReasonCodes, "quality_vmaf_frames_below_limit_exceeded")
				}
			}
		}
		if sh.record.Quality.VMAF.GuardrailEnforcement == "reject" && len(e.ReasonCodes) > 0 {
			reject = e.ReasonCodes[0]
		}
	}
	if sh.runCAMBI {
		cambiReasonStart := len(e.ReasonCodes)
		if len(cambiSamples) != len(sh.record.Samples) {
			e.ReasonCodes = append(e.ReasonCodes, "quality_cambi_evidence_incomplete")
		} else {
			combined, err := quality.CombineCAMBI(cambiSamples)
			if err != nil {
				e.ReasonCodes = append(e.ReasonCodes, "quality_cambi_evidence_incomplete")
			} else {
				e.CAMBI = &combined
				b := sh.record.Quality.Banding
				if b.MaxMean != nil && combined.Mean > *b.MaxMean {
					e.ReasonCodes = append(e.ReasonCodes, "quality_cambi_mean_above_maximum")
				}
				if b.MaxPeak != nil && combined.Max > *b.MaxPeak {
					e.ReasonCodes = append(e.ReasonCodes, "quality_cambi_peak_above_maximum")
				}
			}
		}
		if sh.record.Quality.Banding.Enforcement == "reject" && len(e.ReasonCodes) > cambiReasonStart && reject == "" {
			reject = e.ReasonCodes[cambiReasonStart]
		}
	}
	if reject != "" {
		e.Verdict = "fail"
	} else if len(e.ReasonCodes) > 0 {
		e.Verdict = "observe"
	}
	return e, reject
}
