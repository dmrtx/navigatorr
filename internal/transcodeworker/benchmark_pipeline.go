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
	executed   bool
	vmaf       *float64
	ssim       *float64
	vmafDurSec float64
	ssimDurSec float64
	vmafScores []optimization.SampleScore
	ssimScores []optimization.SampleScore
	entry      *BenchmarkMetricSampleResult
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
	caps, err := w.Capabilities(ctx)
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
	passesPerSample := 0
	if runVMAF {
		passesPerSample++
	}
	if runSSIM {
		passesPerSample++
	}

	// 10-bit media validation, matching sequential runMetrics fail-closed behavior.
	if evidence.SourceBitDepth > 8 && runVMAF {
		return fmt.Errorf("worker capability unsupported: source media has bit depth %d (> 8-bit) but worker does not have verified 10-bit VMAF capability; silent 8-bit downconversion is prohibited (fail closed)", evidence.SourceBitDepth)
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
	sh.progressReporter.StartUnit("encoding_candidates")
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
				CandidateID:  vc.candidate.ID,
				SampleIndex:  window.Index,
				File:         filepath.Base(candPath),
				Quality:      vc.candidate.Quality,
				VideoProfile: vc.profile,
				PixelFormat:  vc.pixelFormat,
				Error:        errStr,
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
				CandidateID:  vc.candidate.ID,
				SampleIndex:  window.Index,
				File:         filepath.Base(candPath),
				Quality:      vc.candidate.Quality,
				VideoProfile: vc.profile,
				PixelFormat:  vc.pixelFormat,
				Error:        errStr,
			},
			err: errors.New(errStr),
		}
		fail(sh.encodeOut[idx].err)
		return
	}
	sh.encodeOut[idx] = pipelineEncodeOutcome{
		entry: BenchmarkCandidateSampleResult{
			CandidateID:       vc.candidate.ID,
			SampleIndex:       window.Index,
			File:              filepath.Base(candPath),
			SizeBytes:         fi.Size(),
			EncodeDurationSec: elapsed,
			Quality:           vc.candidate.Quality,
			VideoProfile:      vc.profile,
			PixelFormat:       vc.pixelFormat,
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
		sh.progressReporter.StartUnit("evaluating_metrics")
		args := BuildVMAFArgs(candPath, refPath, logPath)
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
				score, err := ParseVMAFJSON(logData)
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
		sh.progressReporter.StartUnit("evaluating_metrics")
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
				VMAFDurationSec:   vmafDurationSec,
				SSIMDurationSec:   ssimDurationSec,
				MetricDurationSec: vmafDurationSec + ssimDurationSec,
				Error:             candidateFailReason,
			}
			sh.metricOut[idx] = out
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
	out.executed = true
	out.vmaf = vmafScore
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
		SSIM:              ssimScore,
		VMAFDurationSec:   vmafDurationSec,
		SSIMDurationSec:   ssimDurationSec,
		MetricDurationSec: vmafDurationSec + ssimDurationSec,
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
			sh.evidence.CandidateMetrics = append(sh.evidence.CandidateMetrics, BenchmarkCandidateMetricAggregate{
				CandidateID:    vc.candidate.ID,
				CandidateIndex: vc.index,
				MetricType:     optimization.MetricTypeSSIM,
				Aggregate:      agg,
			})
		}
	}
}
