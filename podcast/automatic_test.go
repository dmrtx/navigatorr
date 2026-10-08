package podcast

import "testing"

func TestAutomaticPlannerKeepsUnknownAndChangedCopy(t *testing.T) {
	tr, p := fixture()
	p.KnownAdsFirstPass = true
	ref := AdReference{TextDigest: NativeAdTextDigest(tr.Units[:10]), ID: tr.SourceHash, Digest: tr.SourceHash, SourceHash: tr.SourceHash, CutsDigest: tr.SourceHash, DurationMS: 10000, Label: "paid_ad"}
	report := AdMatchReport{Algorithm: AdAlgorithm, SourceHash: tr.SourceHash, Catalog: AdCatalog{Algorithm: AdAlgorithm, Scope: "show", References: []AdReference{ref}}, DurationMS: tr.DurationMS, Matches: []AdMatch{{ReferenceID: ref.ID, Label: ref.Label, StartMS: 0, EndMS: 10000, MinCorrelation: .99}}}
	cuts, err := PlanKnownAds(tr, p, report)
	if err != nil || cuts.AnalysisMode != AnalysisKnownAdsOnly || len(cuts.Ranges) != 1 || cuts.Ranges[0].FirstID != tr.Units[1].ID || cuts.Ranges[0].LastID != tr.Units[9].ID {
		t.Fatalf("automatic cuts lost native boundaries: %+v %v", cuts, err)
	}
	if err := VerifyCuts(tr, cuts); err != nil {
		t.Fatal(err)
	}
	if _, err := Plan(tr, p, nil); err == nil {
		t.Fatal("automatic pass weakened full classification coverage")
	}
	tr.Units[5].Text = "changed quiet voice"
	cuts, err = PlanKnownAds(tr, p, report)
	if err != nil || len(cuts.Ranges) != 0 || cuts.RemovedMS != 0 {
		t.Fatalf("shared music changed copy must remain intact: %+v %v", cuts, err)
	}
	report.Matches = nil
	cuts, err = PlanKnownAds(tr, p, report)
	if err != nil || len(cuts.Ranges) != 0 || cuts.AnalysisMode != AnalysisKnownAdsOnly {
		t.Fatalf("empty library pass cannot publish unchanged audio: %+v %v", cuts, err)
	}
	p.KnownAdsFirstPass = false
	if _, err := PlanKnownAds(tr, p, report); err == nil {
		t.Fatal("automatic mode bypassed profile opt-in")
	}
}

func TestAutomaticPlannerConflictsAndRemovalLimit(t *testing.T) {
	tr, p := fixture()
	p.KnownAdsFirstPass = true
	p.Remove = []string{"paid_ad", "cross_promo"}
	ref := AdReference{TextDigest: NativeAdTextDigest(tr.Units[:10]), ID: tr.SourceHash, Digest: tr.SourceHash, SourceHash: tr.SourceHash, CutsDigest: tr.SourceHash, DurationMS: 10000, Label: "paid_ad"}
	other := ref
	other.ID = Digest("conflicting reference")
	other.Digest, other.Label = other.ID, "cross_promo"
	report := AdMatchReport{Algorithm: AdAlgorithm, SourceHash: tr.SourceHash, Catalog: AdCatalog{Algorithm: AdAlgorithm, Scope: "show", References: []AdReference{ref, other}}, DurationMS: tr.DurationMS, Matches: []AdMatch{{ReferenceID: ref.ID, Label: ref.Label, StartMS: 0, EndMS: 10000, MinCorrelation: .99}, {ReferenceID: other.ID, Label: other.Label, StartMS: 0, EndMS: 10000, MinCorrelation: .99}}}
	cuts, err := PlanKnownAds(tr, p, report)
	if err != nil || len(cuts.Ranges) != 0 {
		t.Fatalf("ambiguous labels were automatically removed: %+v %v", cuts, err)
	}
	report.Matches = report.Matches[:1]
	p.MaxRemovedFraction = .01
	if _, err := PlanKnownAds(tr, p, report); err == nil {
		t.Fatal("automatic mode bypassed max removed fraction")
	}
}
