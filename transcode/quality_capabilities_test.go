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
