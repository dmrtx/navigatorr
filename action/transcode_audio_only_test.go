package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
)

// audioOnlyReport is a 100 min 1080p HEVC film whose video is already
// efficient, with two DTS-HD MA 5.1 tracks at ~4 Mbps and an AC-3 commentary.
func audioOnlyReport() *mediainspect.DetailedReport {
	return &mediainspect.DetailedReport{DurationSec: 6000,
		Video: []mediainspect.DetailedStream{{Index: 0, Codec: "hevc", BitDepth: 8, Width: 1920, Height: 1080, FPS: 24, BitRate: 6000000}},
		Audio: []mediainspect.DetailedStream{
			{Index: 1, Codec: "dts", Profile: "DTS-HD MA", Channels: 6, ChannelLayout: "5.1(side)", Language: "eng", Tags: map[string]string{"NUMBER_OF_BYTES-eng": "3000000000"}},
			{Index: 2, Codec: "dts", Profile: "DTS-HD MA", Channels: 6, ChannelLayout: "5.1(side)", Language: "spa", BitRate: 4000000},
			{Index: 3, Codec: "ac3", Channels: 2, ChannelLayout: "stereo", Language: "eng", BitRate: 192000},
		}}
}

func TestEstimateAudioOnly(t *testing.T) {
	rep := audioOnlyReport()
	const source = int64(10_700_000_000)
	e := estimateAudioOnly(rep, source)
	_, kbps := transcode.AudioTarget(transcode.AudioModeCompactLossless, "dts", "DTS-HD MA", 6, "5.1(side)")
	compacted := int64(kbps) * 1000 * 6000 / 8
	want := source - (3_000_000_000 - compacted) - (3_000_000_000 - compacted)
	if e.Compacted != 2 || e.Unknown != 0 || e.CandidateBytes != want {
		t.Fatalf("estimate %+v, want %d bytes", e, want)
	}
	// A lossless track with no size is counted but saves nothing.
	rep.Audio[1].BitRate = 0
	if e := estimateAudioOnly(rep, source); e.Compacted != 2 || e.Unknown != 1 || e.CandidateBytes != source-(3_000_000_000-compacted) {
		t.Fatalf("unknown track estimate %+v", e)
	}
	lossy := &mediainspect.DetailedReport{DurationSec: 6000, Audio: rep.Audio[2:]}
	if e := estimateAudioOnly(lossy, source); e.Compacted != 0 || e.CandidateBytes != source {
		t.Fatalf("lossy-only estimate %+v", e)
	}
}

type audioOnlyCase struct {
	ec       *ExecutionContext
	engine   *Engine
	outputs  map[string]any
	base     *transcode.Plan
	snapshot *ModePolicySnapshot
}

func newAudioOnlyCase(t *testing.T, caps transcode.WorkerCapabilities, capsErr error) *audioOnlyCase {
	t.Helper()
	ec := &ExecutionContext{ActionName: "transcode_media", InstanceID: "audio-only", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"original_sha256": strings.Repeat("a", 64), "original_size": int64(10_700_000_000), "source_report": audioOnlyReport(), "size_main10": true}}
	snapshot, err := freezeModePolicy(ec)
	if err != nil {
		t.Fatal(err)
	}
	base := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 24, Preset: "medium", VideoProfile: "main10", PixelFormat: "p010le", ExpectedBitDepth: 10, AudioMode: snapshot.Profile.Audio.Mode}
	base.QualityValidation = &transcode.QualityValidationPlan{Metric: "vmaf", Quality: *ensureBenchmarkQualityMetric(buildBenchmarkQualityConfig(snapshot.Profile.Optimization.Quality), "vmaf"), BenchmarkRequestDigest: "request"}
	if err := applyModePlan(base, snapshot); err != nil {
		t.Fatal(err)
	}
	ec.State["plan"] = base
	executor := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) { return caps, capsErr }}
	return &audioOnlyCase{ec: ec, engine: NewEngine(EngineDeps{Transcode: executor}), outputs: map[string]any{}, base: base, snapshot: snapshot}
}

func audioOnlyCaps() transcode.WorkerCapabilities {
	caps := modeTestCapabilities()
	caps.SizeAudioOnly = true
	return caps
}

func TestSizeAudioOnlyFallbackBuildsCopyPlan(t *testing.T) {
	c := newAudioOnlyCase(t, audioOnlyCaps(), nil)
	result, handled := c.engine.sizeAudioOnlyFallback(context.Background(), c.ec, c.outputs)
	if !handled || result.Status != StepCompleted || getBool(c.ec.State, "skip_transcode") {
		t.Fatalf("fallback %+v handled=%v state=%+v", result, handled, c.ec.State)
	}
	plan := getPlan(c.ec.State["plan"])
	if !transcode.IsSizeAudioOnlyPlan(plan) || transcode.ValidateSizeAudioOnlyShape(plan) != nil || plan.ExpectedBitDepth != 8 || plan.PolicyDigest != c.base.PolicyDigest || plan.SizePolicy.MinSavingsPercent != 15 {
		t.Fatalf("audio-only plan %+v", plan)
	}
	if c.ec.State["plan_source"] != "audio_only" || c.ec.State["plan_digest"] != plan.PlanDigest || !getBool(c.ec.State, "size_audio_only") || c.ec.State["size_main10"] != nil || getInt64(c.ec.State, "audio_only_estimated_bytes") <= 0 {
		t.Fatalf("state %+v", c.ec.State)
	}
}

func TestSizeAudioOnlyFallbackDeclines(t *testing.T) {
	for name, tc := range map[string]struct {
		caps    transcode.WorkerCapabilities
		capsErr error
		mutate  func(*audioOnlyCase)
		handled bool
		reason  string
		status  StepStatus
	}{
		"lossy audio only": {caps: audioOnlyCaps(), mutate: func(c *audioOnlyCase) {
			rep := audioOnlyReport()
			rep.Audio = rep.Audio[2:]
			c.ec.State["source_report"] = rep
		}},
		"small lossless track": {caps: audioOnlyCaps(), handled: true, reason: "audio_compaction_insufficient", status: StepCompleted, mutate: func(c *audioOnlyCase) {
			c.ec.State["original_size"] = int64(60_000_000_000)
		}},
		"two programme videos": {caps: audioOnlyCaps(), handled: true, reason: "unsupported_source", status: StepCompleted, mutate: func(c *audioOnlyCase) {
			rep := audioOnlyReport()
			rep.Video = append(rep.Video, rep.Video[0])
			c.ec.State["source_report"] = rep
		}},
		"old worker":         {caps: modeTestCapabilities(), handled: true, reason: "audio_only_worker_unsupported", status: StepCompleted},
		"capability failure": {caps: audioOnlyCaps(), capsErr: errors.New("boom"), handled: true, status: StepFailed},
		"quality mode": {caps: audioOnlyCaps(), mutate: func(c *audioOnlyCase) {
			c.ec.Inputs["mode"] = "quality"
		}},
		"frozen policy without the fallback": {caps: audioOnlyCaps(), mutate: func(c *audioOnlyCase) {
			delete(c.ec.State, "mode_policy")
			delete(c.ec.State, "policy_digest")
			s := *c.snapshot
			size := *s.Size
			size.AudioOnlyFallback = false
			s.Size = &size
			s.Digest = ""
			s.Digest = digestModePolicySnapshot(t, s)
			c.ec.State["mode_policy"] = s
		}},
	} {
		t.Run(name, func(t *testing.T) {
			c := newAudioOnlyCase(t, tc.caps, tc.capsErr)
			if tc.mutate != nil {
				tc.mutate(c)
			}
			result, handled := c.engine.sizeAudioOnlyFallback(context.Background(), c.ec, c.outputs)
			if handled != tc.handled {
				t.Fatalf("handled=%v want %v (%+v)", handled, tc.handled, result)
			}
			if !handled {
				if transcode.IsSizeAudioOnlyPlan(getPlan(c.ec.State["plan"])) {
					t.Fatal("plan replaced although not handled")
				}
				return
			}
			if result.Status != tc.status || getString(c.ec.State, "reason_code") != tc.reason || (tc.reason != "") != getBool(c.ec.State, "skip_transcode") {
				t.Fatalf("result %+v state %+v", result, c.ec.State)
			}
			if transcode.IsSizeAudioOnlyPlan(getPlan(c.ec.State["plan"])) {
				t.Fatal("declined fallback still installed a copy plan")
			}
		})
	}
}

func TestAudioOnlyCandidateValidation(t *testing.T) {
	c := newAudioOnlyCase(t, audioOnlyCaps(), nil)
	if _, handled := c.engine.sizeAudioOnlyFallback(context.Background(), c.ec, c.outputs); !handled {
		t.Fatal("fallback not taken")
	}
	plan := getPlan(c.ec.State["plan"])
	candidate := t.TempDir() + "/candidate.mkv"
	if err := os.WriteFile(candidate, make([]byte, 80), 0600); err != nil {
		t.Fatal(err)
	}
	pass := func() map[string]any {
		return map[string]any{"verdict": "pass", "candidate_sha256": "candidate", "plan_digest": plan.PlanDigest,
			"video_copy": map[string]any{"method": transcode.VideoCopyMethod, "verdict": "pass", "source_hash": "sha256:v", "candidate_hash": "sha256:v", "source_codec": "hevc"}}
	}
	for name, tc := range map[string]struct {
		evidence func() map[string]any
		ok       bool
	}{
		"bit-identical video": {evidence: pass, ok: true},
		"no video copy proof": {evidence: func() map[string]any { e := pass(); delete(e, "video_copy"); return e }},
		"video packets changed": {evidence: func() map[string]any {
			e := pass()
			e["video_copy"].(map[string]any)["candidate_hash"] = "sha256:w"
			return e
		}},
		"other candidate": {evidence: func() map[string]any { e := pass(); e["candidate_sha256"] = "other"; return e }},
		"other plan":      {evidence: func() map[string]any { e := pass(); e["plan_digest"] = "sha256:other"; return e }},
	} {
		t.Run(name, func(t *testing.T) {
			state := map[string]any{}
			for k, v := range c.ec.State {
				state[k] = v
			}
			state["candidate_path"], state["candidate_sha256"], state["quality_evidence"] = candidate, "candidate", tc.evidence()
			state["original_size"], state["original"] = int64(100), map[string]any{"size_bytes": int64(100), "video_codec": "hevc"}
			ec := &ExecutionContext{ActionName: "transcode_media", InstanceID: c.ec.InstanceID, Inputs: c.ec.Inputs, State: state}
			result, err := (&Engine{}).stepTranscodeValidate(context.Background(), ec)
			if err != nil || (result.Status == StepCompleted) != tc.ok || getBool(ec.State, "skip_transcode") {
				t.Fatalf("%+v %v", result, err)
			}
			if tc.ok && result.Outputs["result"].(map[string]any)["video_codec"] != "hevc" {
				t.Fatalf("video codec %v", result.Outputs["result"])
			}
		})
	}
	// The perceptual path is still mandatory for an encode plan.
	c.ec.State["plan"] = &transcode.Plan{Mode: "size", ModePolicyVersion: optimization.ModePolicyV3, VideoCodec: transcode.VideoCodecLibX265, PolicyDigest: plan.PolicyDigest, SizePolicy: plan.SizePolicy}
	c.ec.State["candidate_path"] = candidate
	if result, _ := (&Engine{}).stepTranscodeValidate(context.Background(), c.ec); result.Status != StepFailed {
		t.Fatalf("encode plan without perceptual validation passed: %+v", result)
	}
}

func digestModePolicySnapshot(t *testing.T, p ModePolicySnapshot) string {
	t.Helper()
	p.Digest = ""
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
