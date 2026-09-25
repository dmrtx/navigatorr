package quality

import "testing"

func TestCAMBIFullReferenceUsesProvenOutputAndCombines(t *testing.T) {
	data := []byte(`{"version":"3.2.0","frames":[{"frameNum":0,"metrics":{"cambi":9,"cambi_full_reference":0.5}},{"frameNum":1,"metrics":{"cambi":8,"cambi_full_reference":1.5}}],"pooled_metrics":{"cambi_full_reference":{"mean":1.0}}}`)
	a, err := ParseCAMBIFullReference(data, 4096, "cambi_full_reference", 0, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseCAMBIFullReference(data, 4096, "cambi_full_reference", 1, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	got, err := CombineCAMBI([]CAMBIStats{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if got.Mean != 1 || got.Max != 1.5 || got.FramesTotal != 4 || got.WorstSourceSec != 10.5 {
		t.Fatalf("unexpected CAMBI evidence: %+v", got)
	}
	if _, err := ParseCAMBIFullReference(data, 4096, "cambi_source", 0, 10, 2); err == nil {
		t.Fatal("accepted unproven output name")
	}
	if _, err := ParseCAMBIFullReference([]byte(`{"frames":[{"frameNum":0,"metrics":{"cambi_full_reference":-1}}]}`), 4096, "cambi_full_reference", 0, 0, 24); err == nil {
		t.Fatal("accepted negative deterioration")
	}
}
