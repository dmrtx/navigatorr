package transcode

import "testing"

func TestQualityCapabilityChangesFingerprint(t *testing.T) {
	caps := WorkerCapabilities{ProtocolVersion: WorkerProtocolVersion, FFmpegVersion: "9.0.1", Encoders: map[string]bool{}, Filters: map[string]bool{"libvmaf": true}, Quality: &QualityCapabilities{Models: map[string]QualityModelCapability{"v1_1080p_3h": {Available: false, MeasurementBitDepth: 10, ScoreMin: 0, ScoreMax: 100}}}}
	before, err := ComputeCapabilityFingerprint(caps)
	if err != nil {
		t.Fatal(err)
	}
	caps.Quality.Models["v1_1080p_3h"] = QualityModelCapability{Available: true, MeasurementBitDepth: 10, ScoreMin: 0, ScoreMax: 100}
	after, err := ComputeCapabilityFingerprint(caps)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("model capability did not change fingerprint")
	}
	caps.Quality.CAMBIFullRef = true
	withCAMBI, err := ComputeCapabilityFingerprint(caps)
	if err != nil {
		t.Fatal(err)
	}
	if after == withCAMBI {
		t.Fatal("CAMBI capability did not change fingerprint")
	}
}

// Coordinator and worker share this gate, so a coordinator that admits an HFR
// source is talking to a worker that proved it can measure one.
func TestSupportsQualityFPSRequiresProvenHFRDecimation(t *testing.T) {
	model := map[string]QualityModelCapability{"v1_1080p_3h": {Available: true, MeasurementBitDepth: 10, ScoreMax: 100}}
	old := WorkerCapabilities{Quality: &QualityCapabilities{Models: model}}
	hfr := WorkerCapabilities{Quality: &QualityCapabilities{Models: model, HFRTemporalDecimation: 2}}
	broken := WorkerCapabilities{Quality: &QualityCapabilities{Models: model, HFRTemporalDecimation: 2, ProbeError: "vmaf_probe_execution_failed"}}
	wrongFactor := WorkerCapabilities{Quality: &QualityCapabilities{Models: model, HFRTemporalDecimation: 3}}
	for _, tc := range []struct {
		fps       float64
		old, want bool
	}{{0, false, false}, {25, true, true}, {29.97, true, true}, {30, true, true}, {50, false, true}, {59.94, false, true}, {60, false, true}, {120, false, false}} {
		if got := old.SupportsQualityFPS(tc.fps, "progressive"); got != tc.old {
			t.Fatalf("old worker %.3f fps: %v", tc.fps, got)
		}
		if got := hfr.SupportsQualityFPS(tc.fps, ""); got != tc.want {
			t.Fatalf("HFR worker %.3f fps: %v", tc.fps, got)
		}
		if tc.fps >= 45 && (broken.SupportsQualityFPS(tc.fps, "") || wrongFactor.SupportsQualityFPS(tc.fps, "")) {
			t.Fatalf("unproven worker admitted %.3f fps", tc.fps)
		}
		// Interlaced HFR-class streams may report their field rate.
		for _, order := range []string{"tt", "bb", "tb", "bt"} {
			want := tc.want && tc.fps < 45
			if got := hfr.SupportsQualityFPS(tc.fps, order); got != want {
				t.Fatalf("HFR worker %.3f fps field order %s: %v", tc.fps, order, got)
			}
		}
	}
	if (WorkerCapabilities{}).SupportsHFRQuality() {
		t.Fatal("missing quality report claimed HFR")
	}
	before, _ := ComputeCapabilityFingerprint(WorkerCapabilities{ProtocolVersion: WorkerProtocolVersion, Quality: old.Quality})
	after, _ := ComputeCapabilityFingerprint(WorkerCapabilities{ProtocolVersion: WorkerProtocolVersion, Quality: hfr.Quality})
	if before == after {
		t.Fatal("HFR capability did not change fingerprint")
	}
}

func TestHardwareSearchCapabilityChangesFingerprint(t *testing.T) {
	caps := WorkerCapabilities{ProtocolVersion: WorkerProtocolVersion, FFmpegVersion: "9.0.1", Encoders: map[string]bool{}}
	before, err := ComputeCapabilityFingerprint(caps)
	if err != nil {
		t.Fatal(err)
	}
	caps.HardwareSearch = true
	after, err := ComputeCapabilityFingerprint(caps)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("hardware_search did not change the fingerprint")
	}
}
