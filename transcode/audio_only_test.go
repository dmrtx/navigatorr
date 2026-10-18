package transcode

import "testing"

func TestVideoCopyEvidenceCheck(t *testing.T) {
	pass := VideoCopyEvidence{Method: VideoCopyMethod, Verdict: "pass", SourceHash: "sha256:a", CandidateHash: "sha256:a"}
	if err := pass.Check(); err != nil {
		t.Fatal(err)
	}
	var missing *VideoCopyEvidence
	if missing.Check() == nil {
		t.Fatal("nil evidence passed")
	}
	for name, mutate := range map[string]func(*VideoCopyEvidence){
		"other method":   func(e *VideoCopyEvidence) { e.Method = "framemd5" },
		"failed verdict": func(e *VideoCopyEvidence) { e.Verdict = "fail" },
		"changed stream": func(e *VideoCopyEvidence) { e.CandidateHash = "sha256:b" },
		"no hashes":      func(e *VideoCopyEvidence) { e.SourceHash, e.CandidateHash = "", "" },
	} {
		e := pass
		mutate(&e)
		if e.Check() == nil {
			t.Errorf("%s passed", name)
		}
	}
}

func TestNewSizeAudioOnlyPlanNeedsFrozenSizeV3(t *testing.T) {
	if IsSizeAudioOnlyPlan(&Plan{Mode: "size", VideoCodec: VideoCodecCopy}) {
		t.Fatal("a plan without policy v3 is not audio-only")
	}
	if _, err := NewSizeAudioOnlyPlan(&Plan{Mode: "size", VideoCodec: VideoCodecLibX265, AudioMode: AudioModeCompactLossless}, 8); err == nil {
		t.Fatal("unfrozen plan accepted")
	}
	if _, err := NewSizeAudioOnlyPlan(nil, 8); err == nil {
		t.Fatal("nil plan accepted")
	}
}
