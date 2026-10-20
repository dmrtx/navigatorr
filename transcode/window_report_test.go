package transcode

import "testing"

func TestCompareWindowsJoinsSearchAndFinalByStart(t *testing.T) {
	search := []BenchmarkWindowScore{
		{StartSeconds: 600, DurationSeconds: 10, Role: WindowRoleDiscovery, VMAFMean: 93, VMAFP5: 90, VMAFMin: 88},
		{StartSeconds: 1200, DurationSeconds: 10, Role: WindowRoleVerification, VMAFMean: 92, VMAFP5: 89, VMAFMin: 86},
	}
	final := []FinalWindowScore{
		{StartSeconds: 600.004, DurationSeconds: 10, Role: "grid", VMAFMean: 91, VMAFP5: 88, VMAFMin: 85},
		{StartSeconds: 900, DurationSeconds: 10, Role: "grid", VMAFMean: 84, VMAFP5: 80, VMAFMin: 77},
		{StartSeconds: 1200, DurationSeconds: 10, Role: "grid", VMAFMean: 90, VMAFP5: 87, VMAFMin: 84},
		{StartSeconds: 1500, DurationSeconds: 10, Role: "fresh", VMAFMean: 89, VMAFP5: 86, VMAFMin: 83},
	}
	got := CompareWindows(search, final)
	if len(got) != 4 {
		t.Fatalf("want 4 windows, got %d: %+v", len(got), got)
	}
	if got[0].Search == nil || got[0].Final == nil || got[0].Final.VMAFMean != 91 || got[0].Role != WindowRoleDiscovery {
		t.Fatalf("first window should carry both sides: %+v", got[0])
	}
	if got[1].Role != WindowRoleUnsearched || got[1].Search != nil || got[1].Final == nil || got[1].Final.VMAFMean != 84 {
		t.Fatalf("a final window no sample covered is unsearched: %+v", got[1])
	}
	if got[2].Role != WindowRoleVerification || got[2].Final.VMAFMean != 90 {
		t.Fatalf("third window wrong: %+v", got[2])
	}
	if got[3].Role != WindowRoleFresh || got[3].Search != nil {
		t.Fatalf("fresh window has no search side: %+v", got[3])
	}
	if CompareWindows(nil, nil) != nil {
		t.Fatal("no windows, no report")
	}
}
