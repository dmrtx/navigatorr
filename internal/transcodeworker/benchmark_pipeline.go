package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
	"github.com/jakenesler/navigatorr/transcode/quality"
	"github.com/jakenesler/navigatorr/transcode/resilience"
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

// sizeModeMetricWorkers is the metric worker count a size-mode search uses
// when its request leaves concurrency to the worker.
const sizeModeMetricWorkers = 3

// sizeModeMetricConcurrency raises a size-mode search to sizeModeMetricWorkers
// metric workers when the request set no concurrency and the host can give
// every simultaneous pass of every parallel job minMetricThreadsPerPass
// libvmaf threads without oversubscribing; narrower passes measured slower
// than two wider ones. Thread counts never change metric values.
func (w *Worker) sizeModeMetricConcurrency(record *BenchmarkRecord, metricN int) int {
	if record == nil || record.Mode != "size" || record.Concurrency != nil || metricN >= sizeModeMetricWorkers {
		return metricN
	}
	jobs := w.videoJobsSharingCPU()
	threads := w.metricThreads(sizeModeMetricWorkers)
	if threads < minMetricThreadsPerPass || threads*sizeModeMetricWorkers*jobs > hostCPUCount() {
		return metricN
	}
	return sizeModeMetricWorkers
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
	// cambiDeferred marks a unit whose CAMBI waits for the candidate's VMAF
	// verdict (see runDeferredCAMBI); cambiSkipped one that verdict made
	// unnecessary.
	cambiDeferred bool
	cambiSkipped  bool
	ssim          *float64
	vmafDurSec    float64
	ssimDurSec    float64
	vmafScores    []optimization.SampleScore
	ssimScores    []optimization.SampleScore
	entry         *BenchmarkMetricSampleResult
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
	// deferCAMBI measures CAMBI after every VMAF unit, and only for a candidate
	// no VMAF limit rejects (see bandingGate).
	deferCAMBI      bool
	vmafModel       *quality.VMAFModel
	vmafDecimation  int
	qualityCaps     *transcode.QualityCapabilities
	capFingerprint  string
	passesPerSample int
	metricThreads   int
	savings         *savingsGate
	poisons         []*candidatePoison
	encodeOut       []pipelineEncodeOutcome
	metricOut       []pipelineMetricOutcome
	fatal           *pipelineFatal
	// cache and cacheKeys hold the sample cache for this call and each unit's
	// key; both are nil when the cache is off (see newSampleCacheRun).
	cache     *sampleCacheRun
	cacheKeys []string
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
	savings *savingsGate,
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
	runVMAF := normMetric == "vmaf" || normMetric == "both" || normMetric == "vmaf+ssim"
	if record.Allow8BitTo10Bit && (normMetric != "vmaf" || record.Quality == nil || record.Quality.VMAF == nil || record.Quality.VMAF.Model == "") {
		return errors.New("explicit 8-bit to Main10 benchmark requires VMAF with an explicit native-depth model")
	}
	qualityBitDepth := evidence.SourceBitDepth
	for _, vc := range vcs {
		if vc.bitDepth > qualityBitDepth {
			qualityBitDepth = vc.bitDepth
		}
	}
	needQualityProbe := record.Quality != nil && ((record.Quality.VMAF != nil && record.Quality.VMAF.Model != "") || (record.Quality.Banding != nil && record.Quality.Banding.Enabled))
	// Native-depth proof is required even when a legacy request omits the model.
	needQualityProbe = needQualityProbe || (qualityBitDepth > 8 && runVMAF)
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
	runSSIM := normMetric == "ssim" || normMetric == "both" || normMetric == "vmaf+ssim"
	runCAMBI := record.Quality != nil && record.Quality.Banding != nil && record.Quality.Banding.Enabled
	var vmafModel *quality.VMAFModel
	vmafDecimation := 1
	if runVMAF && record.Quality != nil && record.Quality.VMAF != nil && record.Quality.VMAF.Model != "" {
		model, err := quality.Model(record.Quality.VMAF.Model)
		if err != nil {
			return err
		}
		if caps.Quality == nil || caps.Quality.ProbeError != "" || !caps.Quality.Models[model.ID].Available {
			return fmt.Errorf("quality_vmaf_model_unavailable: model %s is not executable on worker (fail closed)", model.ID)
		}
		vmafModel = &model
		// Unknown timing keeps factor 1 and fails per sample in ParseVMAF.
		// RunBenchmark already rejected unsupported HFR; this stays defensive.
		if evidence.SourceFPS > 0 {
			if vmafDecimation, err = quality.TemporalDecimationForStream(evidence.SourceFPS, evidence.SourceFieldOrder); err != nil {
				return fmt.Errorf("quality_vmaf_hfr_model_unavailable: %v (fail closed)", err)
			}
		}
		if vmafDecimation > 1 && !caps.SupportsHFRQuality() {
			return fmt.Errorf("quality_vmaf_hfr_model_unavailable: worker has no proven decimated VMAF path for %.3f FPS (fail closed)", evidence.SourceFPS)
		}
		evidence.VMAFTemporalDecimation = vmafDecimation
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
	if qualityBitDepth > 8 && runVMAF && !caps.SupportsNative10BitQuality() {
		return fmt.Errorf("worker capability unsupported: source or candidate media has bit depth %d (> 8-bit) but worker does not have verified 10-bit VMAF capability; silent 8-bit downconversion is prohibited (fail closed)", qualityBitDepth)
	}
	if qualityBitDepth > 8 && runCAMBI && !caps.SupportsNative10BitQuality() {
		return fmt.Errorf("quality_cambi_native_main10_unverified: native %d-bit media is not eligible for CAMBI (fail closed)", qualityBitDepth)
	}

	encodeN, metricN := resolvePipelineConcurrency(record)
	metricN = w.sizeModeMetricConcurrency(record, metricN)

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
		deferCAMBI:           runCAMBI && !runSSIM && vmafModel != nil && record.bandingGate != bandingGateOff,
		vmafModel:            vmafModel,
		vmafDecimation:       vmafDecimation,
		qualityCaps:          caps.Quality,
		capFingerprint:       caps.CapabilityFingerprint,
		passesPerSample:      passesPerSample,
		metricThreads:        w.metricThreads(metricN),
		savings:              savings,
		poisons:              poisons,
		encodeOut:            make([]pipelineEncodeOutcome, nUnits),
		metricOut:            make([]pipelineMetricOutcome, nUnits),
		fatal:                fatal,
	}

	if savings == nil {
		// Units already measured on the same source bytes are replayed from
		// the sample cache; only the rest are encoded and measured.
		pending := sh.replayCachedUnits(ctx, units)
		sh.runUnits(ctx, units, pending, nil, true, encodeN, metricN)
	} else {
		// Encode each family's most-compressed setting first and hold every
		// metric until those encodes decide which families can still save
		// enough; a doomed family's other settings are never encoded.
		savings.leaderOf = savingsLeaders(vcs)
		var decisive, rest []int
		for idx, u := range units {
			if savings.leaderOf[u.vcOrd] == u.vcOrd {
				decisive = append(decisive, idx)
			} else {
				rest = append(rest, idx)
			}
		}
		sh.runUnits(ctx, units, decisive, nil, false, encodeN, metricN)
		if !fatal.failed() && ctx.Err() == nil {
			sh.decideSavingsFamilies()
			var alive []int
			for _, idx := range rest {
				if _, dominated := savings.dominatedBy(units[idx].vcOrd); dominated {
					progressReporter.SkipUnits(1 + passesPerSample)
					continue
				}
				alive = append(alive, idx)
			}
			sh.runUnits(ctx, units, alive, decisive, true, encodeN, metricN)
		}
	}

	if r.pipelineStageHook != nil {
		r.pipelineStageHook("measured")
	}
	if sh.deferCAMBI && !fatal.failed() && ctx.Err() == nil {
		sh.runDeferredCAMBI(ctx, metricN)
	}
	if ferr, metric := fatal.get(); ferr != nil {
		sh.assembleEvidence(units)
		if metric {
			return fmt.Errorf("metrics calculation failed: %w", ferr)
		}
		return ferr
	}
	sh.assembleEvidence(units)
	return sh.checkComplete(ctx, units)
}

// contextStop is the error of a context that ended. It satisfies errors.Is for
// the context's error (context.Canceled or context.DeadlineExceeded, which the
// budget handling keys on) and, when the cancellation carried a cause such as
// errSearchBudgetStopped, for that cause too.
func contextStop(ctx context.Context) error {
	err := ctx.Err()
	if err == nil {
		return nil
	}
	cause := context.Cause(ctx)
	if cause == nil || errors.Is(err, cause) {
		return err
	}
	return fmt.Errorf("%w: %w", err, cause)
}

// errPipelineIncomplete reports a pipeline that returned with a unit that was
// neither run nor replayed from the cache, with no failure and no cancellation
// to explain it. Reporting success then would hand incomplete evidence to the
// selection, which can only fail later with a message that names none of this.
type errPipelineIncomplete struct {
	candidate string
	window    int
}

func (e errPipelineIncomplete) Error() string {
	return fmt.Sprintf("benchmark pipeline ended without a result for candidate %q window %d: evidence incomplete (internal invariant violated)", e.candidate, e.window)
}

// FailureClass lets the failure keep its own class instead of being read as an
// ffmpeg failure by its text.
func (e errPipelineIncomplete) FailureClass() string { return failureClassEvidenceIncomplete }

// unitComplete reports whether a unit has its encode and metric outcome:
// measured here, replayed from the sample cache, or recorded as a failure of
// the candidate. A deferred banding measurement that has neither run nor been
// skipped leaves its unit incomplete.
func (sh *pipelineShared) unitComplete(idx int) bool {
	if idx < 0 || idx >= len(sh.encodeOut) || idx >= len(sh.metricOut) {
		return false
	}
	if sh.encodeOut[idx].entry.CandidateID == "" {
		return false
	}
	met := sh.metricOut[idx]
	if !met.executed {
		return false
	}
	return !met.cambiDeferred || met.cambiSkipped
}

// checkComplete returns nil when every unit that had to run did, or when a
// job-fatal error was recorded (the caller reports that one). Otherwise the
// work was cut off without a recorded cause: the context's cause when it ended,
// else errPipelineIncomplete naming the first unit that is missing. It never
// returns nil with gaps. Candidates dominated on size are skipped on purpose.
func (sh *pipelineShared) checkComplete(ctx context.Context, units []pipelineUnit) error {
	if sh.fatal.failed() {
		return nil
	}
	n := len(sh.record.Samples)
	for _, u := range units {
		if _, dominated := sh.savings.dominatedBy(u.vcOrd); dominated {
			continue
		}
		if sh.unitComplete(u.vcOrd*n + u.sampOrd) {
			continue
		}
		if ctx.Err() != nil {
			return contextStop(ctx)
		}
		return errPipelineIncomplete{candidate: sh.vcs[u.vcOrd].candidate.ID, window: sh.record.Samples[u.sampOrd].Index}
	}
	return nil
}

// runUnits encodes encodeIdx with bounded concurrency. When measure is set,
// each newly encoded unit and every already encoded readyIdx unit is then
// evaluated on the metric stage while remaining encodes continue.
func (sh *pipelineShared) runUnits(ctx context.Context, units []pipelineUnit, encodeIdx, readyIdx []int, measure bool, encodeN, metricN int) {
	encQ := make(chan int, len(encodeIdx))
	metQ := make(chan int, len(encodeIdx)+len(readyIdx))
	if measure {
		for _, idx := range readyIdx {
			metQ <- idx
		}
	}

	// Feeder enqueues units in deterministic order; stops early on caller
	// cancellation or the first job-fatal error so no new work starts after
	// the job is already doomed. In-flight units are never interrupted.
	go func() {
		defer close(encQ)
		for _, idx := range encodeIdx {
			if sh.fatal.failed() {
				return
			}
			select {
			case <-ctx.Done():
				// A unit that was never queued is a unit that never runs.
				sh.fatal.set(contextStop(ctx), false)
				return
			case encQ <- idx:
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
						// The unit was taken off the queue and will not run.
						sh.fatal.set(contextStop(ctx), false)
						return
					}
					if sh.fatal.failed() {
						return
					}
					sh.runEncodeUnit(ctx, units[idx], idx)
					if !measure {
						continue
					}
					// A successful encode must not lose its metric silently: when
					// the context ends first, the dropped measurement is recorded.
					if ctx.Err() != nil {
						sh.fatal.set(contextStop(ctx), true)
						return
					}
					select {
					case <-ctx.Done():
						sh.fatal.set(contextStop(ctx), true)
						return
					case metQ <- idx:
					}
				}
			}
		}()
	}
	if !measure {
		encWG.Wait()
		return
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
						sh.fatal.set(contextStop(ctx), true)
						return
					}
					sh.runMetricUnit(ctx, units[idx], idx)
				}
			}
		}()
	}
	metWG.Wait()
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
		fail(contextStop(ctx))
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
	candArgs = withX265ThreadParams(candArgs, sh.w.x265ThreadParams())
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
	if err := runEncoderCommand(ctx, cmd, candArgs, vc.plan, candPath); err != nil {
		if ctx.Err() != nil {
			fail(contextStop(ctx))
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
			err: &ffmpegExecutionError{cause: errors.New(errStr)},
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
			err: &ffmpegExecutionError{cause: errors.New(errStr)},
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
	ctx = asMetricPhase(ctx)
	vc := sh.vcs[u.vcOrd]
	window := sh.record.Samples[u.sampOrd]
	candIdx := vc.index
	fail := func(err error) {
		sh.fatal.set(err, true)
	}

	if ctx.Err() != nil {
		fail(contextStop(ctx))
		return
	}
	if err := verifySourceUnchanged(sh.record.Source, sh.sourceInitialSize, sh.sourceInitialModTime); err != nil {
		fail(fmt.Errorf("source media modified during benchmark (integrity violation, fail closed): %w", err))
		return
	}
	refPath := filepath.Join(sh.samplesDir, "ref_sample_"+strconv.Itoa(window.Index)+".mkv")
	refStat, err := os.Stat(refPath)
	if err != nil || !refStat.Mode().IsRegular() || refStat.Size() == 0 {
		fail(internalInvariantError{fmt.Sprintf("lossless reference sample %s missing or invalid (job-fatal integrity violation, fail closed)", refPath)})
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
			args, err = BuildVMAFModelArgs(candPath, refPath, logPath, *sh.vmafModel, vc.bitDepth, sh.vmafDecimation, sh.metricThreads)
			if err != nil {
				fail(err)
				return
			}
		}
		if sh.evidence.SourceBitDepth > 8 {
			args = append([]string{"-noauto_conversion_filters"}, args...)
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
		runErr := runCharged(ctx, cmd)
		vmafDurationSec = time.Since(start).Seconds()
		if ctx.Err() != nil {
			fail(contextStop(ctx))
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
					stats, parseErr := quality.ParseVMAF(logData, MaxMetricLogSizeBytes, *sh.vmafModel, window.Index, window.StartSeconds, sh.evidence.SourceFPS, sh.vmafDecimation, windowSec, sh.record.Quality.VMAF.FrameThreshold)
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
		runErr := runCharged(ctx, cmd)
		ssimDurationSec = time.Since(start).Seconds()
		if ctx.Err() != nil {
			fail(contextStop(ctx))
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
	if sh.deferCAMBI {
		out.cambiDeferred = true
	} else if sh.runCAMBI {
		sh.progressReporter.StartSampleUnit("evaluating_metrics", u.sampOrd+1, vc.index+1, vc.candidate.ID, "cambi_full_ref")
		start := time.Now()
		stats, err := runCAMBIMetricSample(ctx, sh.w.ffmpegPath, sh.samplesDir, candPath, refPath, candIdx, vc.candidate.ID, window.Index, window.StartSeconds, sh.evidence.SourceFPS, vc.bitDepth, sh.qualityCaps.CAMBIOutput, sh.metricThreads)
		out.cambiDurSec = time.Since(start).Seconds()
		sh.progressReporter.ResolveUnit()
		if ctx.Err() != nil {
			fail(contextStop(ctx))
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
	sh.storeCachedUnit(idx)
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

func runCAMBIMetricSample(ctx context.Context, ffmpegPath, samplesDir, candPath, refPath string, candIdx int, candidateID string, sampleIdx int, sourceStartSec, fps float64, encodedBitDepth int, outputMetric string, threads int) (quality.CAMBIStats, error) {
	logPath, err := derivedMetricLogPath(samplesDir, "cambi", candIdx, candidateID, sampleIdx)
	if err != nil {
		return quality.CAMBIStats{}, err
	}
	if err := prepareOutputFile(logPath); err != nil {
		return quality.CAMBIStats{}, err
	}
	if encodedBitDepth != 8 && encodedBitDepth != 10 {
		return quality.CAMBIStats{}, fmt.Errorf("unsupported CAMBI encoded bit depth %d", encodedBitDepth)
	}
	args := BuildCAMBIArgs(candPath, refPath, logPath, encodedBitDepth, threads)
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
	if err := runCharged(ctx, cmd); err != nil {
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
	// Per-candidate aggregates in deterministic candidate order. Candidates
	// dominated on size were never encoded and are explained by selection.
	for vcOrd, vc := range sh.vcs {
		if _, dominated := sh.savings.dominatedBy(vcOrd); dominated {
			continue
		}
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
	cambiSkipped := false
	for sampOrd := range sh.record.Samples {
		idx := vcOrd*len(sh.record.Samples) + sampOrd
		if idx >= len(sh.metricOut) {
			continue
		}
		met := sh.metricOut[idx]
		cambiSkipped = cambiSkipped || met.cambiSkipped
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
				if policy.MaxFramesBelowThreshold != nil && combined.SourceEquivalentFramesBelowThreshold() > *policy.MaxFramesBelowThreshold {
					e.ReasonCodes = append(e.ReasonCodes, "quality_vmaf_frames_below_limit_exceeded")
				}
			}
		}
		if sh.record.Quality.VMAF.GuardrailEnforcement == "reject" && len(e.ReasonCodes) > 0 {
			reject = e.ReasonCodes[0]
		}
	}
	if sh.runCAMBI && cambiSkipped {
		// A VMAF limit rejected the candidate, whatever CAMBI says: its banding
		// was not measured, which is not incomplete evidence.
		e.ReasonCodes = append(e.ReasonCodes, reasonCAMBINotMeasured)
	} else if sh.runCAMBI {
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

const (
	savingsGateReason      = "insufficient_savings_before_metrics"
	savingsDominatedReason = "skipped_savings_dominated"
)

// savingsGate saves quality measurement when the caller sets a minimum
// savings. Each family's most-compressed setting is encoded first; when its
// estimate is already below the minimum, no setting of that family that
// spends more bits can save enough, so those settings are never encoded and
// the leader's metrics are skipped. When every family is doomed, no quality
// result can produce an acceptable winner, so the verdict does not change.
type savingsGate struct {
	base       optimization.OutputEstimateInput
	minSavings float64
	// leaderOf maps each candidate ordinal to its family's most-compressed
	// member; familyDoomed is indexed by that leader ordinal.
	leaderOf     []int
	familyDoomed []bool
	// doomed reports that every family's leader is below the minimum.
	doomed bool
}

// savingsLeaders maps each candidate to its family's most-compressed member
// (highest x265 CRF, lowest VideoToolbox -q:v); singletons lead themselves.
func savingsLeaders(vcs []validatedCandidate) []int {
	ord := make(map[string]int, len(vcs))
	for i, vc := range vcs {
		ord[vc.candidate.ID] = i
	}
	leaderOf := make([]int, len(vcs))
	for _, f := range groupQualityFamilies(vcs) {
		leader := ord[f.members[f.pick(false)].candidate.ID]
		for _, vc := range f.members {
			leaderOf[ord[vc.candidate.ID]] = leader
		}
	}
	return leaderOf
}

// dominatedBy reports the doomed leader of a candidate that was skipped.
// Leaders themselves are encoded and are never dominated.
func (g *savingsGate) dominatedBy(vcOrd int) (int, bool) {
	if g == nil || g.familyDoomed == nil || vcOrd >= len(g.leaderOf) {
		return 0, false
	}
	leader := g.leaderOf[vcOrd]
	return leader, leader != vcOrd && g.familyDoomed[leader]
}

// decideSavingsFamilies runs once, after every leader encode. A failed encode
// or unusable estimate keeps the family alive so the normal selection decides.
// Doomed leaders are poisoned so their queued metric units do not run.
func (sh *pipelineShared) decideSavingsFamilies() {
	g := sh.savings
	n := len(sh.record.Samples)
	g.familyDoomed = make([]bool, len(sh.vcs))
	g.doomed = true
	for vcOrd := range sh.vcs {
		if g.leaderOf[vcOrd] != vcOrd {
			continue
		}
		if !sh.leaderBelowMinimum(vcOrd, n) {
			g.doomed = false
			continue
		}
		g.familyDoomed[vcOrd] = true
		sh.poisons[vcOrd].setFailed(savingsGateReason)
	}
}

func (sh *pipelineShared) leaderBelowMinimum(vcOrd, n int) bool {
	var sampleBytes int64
	for sampOrd := 0; sampOrd < n; sampOrd++ {
		out := sh.encodeOut[vcOrd*n+sampOrd]
		if out.err != nil || out.entry.Error != "" || out.entry.SizeBytes <= 0 {
			return false
		}
		sampleBytes += out.entry.SizeBytes
	}
	in := sh.savings.base
	in.SampleVideoBytes = sampleBytes
	est, err := optimization.EstimateOutput(in)
	return err == nil && est.SuitableForSelection && est.SavingsPercent < sh.savings.minSavings
}

// selectable drops the candidates that were dominated on size and never encoded.
func (g *savingsGate) selectable(vcs []validatedCandidate) []validatedCandidate {
	if g == nil || g.familyDoomed == nil {
		return vcs
	}
	out := make([]validatedCandidate, 0, len(vcs))
	for vcOrd, vc := range vcs {
		if _, dominated := g.dominatedBy(vcOrd); !dominated {
			out = append(out, vc)
		}
	}
	return out
}

// explain records the gate's verdict on a decision built from selectable(vcs):
// doomed leaders fail on savings and dominated settings are listed as skipped,
// in candidate order.
func (g *savingsGate) explain(d *transcode.BenchmarkDecision, vcs []validatedCandidate) {
	if g == nil || d == nil || g.familyDoomed == nil {
		return
	}
	if g.doomed {
		markInsufficientSavings(d)
	}
	ord := make(map[string]int, len(vcs))
	for i, vc := range vcs {
		ord[vc.candidate.ID] = i
	}
	for i := range d.Evaluations {
		if vcOrd, ok := ord[d.Evaluations[i].CandidateID]; ok && g.familyDoomed[vcOrd] {
			d.Evaluations[i].Eligible = false
			d.Evaluations[i].EvaluationReason = "insufficient_savings"
		}
	}
	for vcOrd, vc := range vcs {
		leader, dominated := g.dominatedBy(vcOrd)
		if !dominated {
			continue
		}
		d.Evaluations = append(d.Evaluations, transcode.BenchmarkCandidateEvaluation{
			CandidateID: vc.candidate.ID, CandidateIndex: vc.index, VideoCodec: transcode.BenchmarkCandidateVideoCodec(vc.candidate),
			Quality: vc.candidate.Quality, Preset: norm(vc.candidate.Preset), Tune: norm(vc.candidate.Tune), VideoProfile: vc.profile, PixelFormat: vc.pixelFormat,
			ExpectedBitDepth: vc.bitDepth, EvaluationReason: savingsDominatedReason,
			Uncertainties: []string{"not measured: " + vcs[leader].candidate.ID + " is below minimum savings"},
		})
	}
	sort.SliceStable(d.Evaluations, func(i, j int) bool {
		return d.Evaluations[i].CandidateIndex < d.Evaluations[j].CandidateIndex
	})
}

// reasonCAMBINotMeasured is the quality reason code of a candidate whose
// banding was not measured because a VMAF limit had already rejected it.
const reasonCAMBINotMeasured = "quality_cambi_not_measured_vmaf_rejected"

// bandingGate says how far a stage's VMAF result settles a candidate, and so
// when its CAMBI can be skipped (see AutomaticSearchRules.DeferBanding).
type bandingGate int

const (
	// bandingGateOff measures CAMBI with VMAF, the unit by unit pipeline.
	bandingGateOff bandingGate = iota
	// bandingGateComplete is a stage whose windows are all the candidate will be
	// judged on: any VMAF limit that rejects it ends its search for good.
	bandingGateComplete
	// bandingGateMonotone is a stage whose windows another stage will join, as
	// the verification windows join the discovery ones. Only a limit that more
	// windows cannot undo counts: a window below the minimum, or the worst window
	// (a minimum over windows). A mean or a percentile can recover.
	bandingGateMonotone
)

// vmafRejects reports whether a candidate's VMAF result alone rejects it under
// the stage's gate, so its CAMBI cannot change the verdict. The checks are the
// ones selection applies: a sample below the minimum, the pooled mean below it,
// and, when the policy rejects on its guardrails, the 5th percentile, the worst
// window and the frames below the threshold.
func (sh *pipelineShared) vmafRejects(vcOrd int) bool {
	n := len(sh.record.Samples)
	stats := make([]quality.VMAFStats, 0, n)
	scores := make([]optimization.SampleScore, 0, n)
	for sampOrd := 0; sampOrd < n; sampOrd++ {
		met := sh.metricOut[vcOrd*n+sampOrd]
		if met.vmaf == nil || met.vmafStats == nil {
			return false
		}
		stats = append(stats, *met.vmafStats)
		scores = append(scores, optimization.SampleScore{SampleIndex: sh.record.Samples[sampOrd].Index, Score: *met.vmaf, Valid: true})
	}
	policy := sh.record.Quality.VMAF
	for _, s := range scores {
		if s.Score < policy.Minimum {
			return true
		}
	}
	complete := sh.record.bandingGate == bandingGateComplete
	if complete && optimization.AggregateSampleScores(optimization.MetricTypeVMAF, scores).MeanScore < policy.Minimum {
		return true
	}
	if policy.GuardrailEnforcement != "reject" {
		return false
	}
	combined, err := quality.CombineVMAF(stats)
	if err != nil {
		return false
	}
	if policy.WorstWindowMinimum != nil && combined.WorstWindowMean < *policy.WorstWindowMinimum {
		return true
	}
	if !complete {
		return false
	}
	return policy.P5Minimum != nil && combined.P5 < *policy.P5Minimum ||
		policy.MaxFramesBelowThreshold != nil && combined.SourceEquivalentFramesBelowThreshold() > *policy.MaxFramesBelowThreshold
}

// runDeferredCAMBI measures the banding of every candidate whose VMAF results are
// all in and that no VMAF limit rejects; the others are marked skipped. It runs
// after every encode and VMAF unit of the call, on the metric workers.
func (sh *pipelineShared) runDeferredCAMBI(ctx context.Context, metricN int) {
	sh.runDeferredCAMBIUnits(ctx, metricN, sh.planDeferredCAMBI())
}

// planDeferredCAMBI marks the deferred units of the candidates a VMAF limit
// rejects as skipped and returns the units whose banding is still to measure.
func (sh *pipelineShared) planDeferredCAMBI() []int {
	n := len(sh.record.Samples)
	var todo []int
	for vcOrd := range sh.vcs {
		var deferred []int
		for sampOrd := 0; sampOrd < n; sampOrd++ {
			if idx := vcOrd*n + sampOrd; sh.metricOut[idx].cambiDeferred {
				deferred = append(deferred, idx)
			}
		}
		if len(deferred) == 0 {
			continue
		}
		if !sh.vmafRejects(vcOrd) {
			todo = append(todo, deferred...)
			continue
		}
		for _, idx := range deferred {
			sh.metricOut[idx].cambiSkipped = true
			sh.metricOut[idx].entry.CAMBISkipped = true
			sh.progressReporter.SkipUnits(1)
		}
	}
	return todo
}

// runDeferredCAMBIUnits runs the banding units on the metric workers.
func (sh *pipelineShared) runDeferredCAMBIUnits(ctx context.Context, metricN int, todo []int) {
	queue := make(chan int, len(todo))
	for _, idx := range todo {
		queue <- idx
	}
	close(queue)
	var wg sync.WaitGroup
	for i := 0; i < max(1, min(metricN, len(todo))); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range queue {
				if ctx.Err() != nil || sh.fatal.failed() {
					return
				}
				sh.runCAMBIUnit(ctx, idx)
			}
		}()
	}
	wg.Wait()
}

// runCAMBIUnit measures the banding of one unit whose VMAF is already in.
func (sh *pipelineShared) runCAMBIUnit(ctx context.Context, idx int) {
	ctx = asMetricPhase(ctx)
	n := len(sh.record.Samples)
	vc := sh.vcs[idx/n]
	sampOrd := idx % n
	window := sh.record.Samples[sampOrd]
	if err := verifySourceUnchanged(sh.record.Source, sh.sourceInitialSize, sh.sourceInitialModTime); err != nil {
		sh.fatal.set(fmt.Errorf("source media modified during benchmark (integrity violation, fail closed): %w", err), true)
		return
	}
	refPath := filepath.Join(sh.samplesDir, "ref_sample_"+strconv.Itoa(window.Index)+".mkv")
	candPath := filepath.Join(sh.samplesDir, vc.fileKey+"_sample_"+strconv.Itoa(window.Index)+".mkv")
	sh.progressReporter.StartSampleUnit("evaluating_metrics", sampOrd+1, vc.index+1, vc.candidate.ID, "cambi_full_ref")
	start := time.Now()
	stats, err := runCAMBIMetricSample(ctx, sh.w.ffmpegPath, sh.samplesDir, candPath, refPath, vc.index, vc.candidate.ID, window.Index, window.StartSeconds, sh.evidence.SourceFPS, vc.bitDepth, sh.qualityCaps.CAMBIOutput, sh.metricThreads)
	elapsed := time.Since(start).Seconds()
	sh.progressReporter.ResolveUnit()
	if ctx.Err() != nil {
		sh.fatal.set(contextStop(ctx), true)
		return
	}
	out := sh.metricOut[idx]
	out.cambiDeferred = false
	out.cambiDurSec = elapsed
	if err != nil {
		out.cambiErr = err.Error()
	} else {
		out.cambi = &stats
	}
	out.entry.CAMBI, out.entry.CAMBIError, out.entry.CAMBIDurationSec = out.cambi, out.cambiErr, elapsed
	out.entry.MetricDurationSec += elapsed
	sh.metricOut[idx] = out
	sh.storeCachedUnit(idx)
}

// Failure classes the worker assigns itself, for failures that are not an
// ffmpeg process failure and must not be read as one.
const (
	failureClassEvidenceIncomplete = string(resilience.BenchmarkEvidenceIncomplete)
	failureClassInternalInvariant  = string(resilience.InternalInvariant)
)

// benchmarkFailureClass returns the class a failed benchmark carries to the
// coordinator, or "" when the error does not declare one. Only typed errors
// count: guessing from the message is what mislabeled non-ffmpeg failures as
// ffmpeg_unknown, so the coordinator keeps its own text fallback for the rest.
func benchmarkFailureClass(err error) string {
	var classified interface{ FailureClass() string }
	if errors.As(err, &classified) {
		return classified.FailureClass()
	}
	return ""
}

// evidenceIncompleteError is a benchmark whose evidence has a gap that nothing
// else (a failure, a cancellation) explains. It is the worker's own invariant
// speaking, not an ffmpeg failure.
type evidenceIncompleteError struct{ msg string }

func (e evidenceIncompleteError) Error() string        { return e.msg }
func (e evidenceIncompleteError) FailureClass() string { return failureClassEvidenceIncomplete }

// internalInvariantError is a state the worker's own earlier steps should have
// made impossible (a reference it extracted is gone), not a tool failure.
type internalInvariantError struct{ msg string }

func (e internalInvariantError) Error() string        { return e.msg }
func (e internalInvariantError) FailureClass() string { return failureClassInternalInvariant }
