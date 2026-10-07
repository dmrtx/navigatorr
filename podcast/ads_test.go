package podcast

import "testing"

func TestLearningSplitsCategoriesAndNeverAmplifiesAcousticDecisions(t *testing.T) {
	tr, p := fixture()
	p.KnownAdsFirstPass = true
	p.MaxRemovedFraction = .5
	p.Remove = []string{"paid_ad", "cross_promo"}
	p.WindowMS = 60000
	p.OverlapMS = 5000
	classes := decisions(tr, p)
	for id, c := range classes {
		for j, d := range c.Decisions {
			for i, u := range tr.Units {
				if u.ID != d.FirstID {
					continue
				}
				switch {
				case i < 15:
					d.Label = "paid_ad"
				case i < 30:
					d.Label = "cross_promo"
				case i < 45:
					d.Label = "paid_ad"
					d.Evidence = &AdEvidence{ReferenceID: tr.SourceHash, MatchDigest: tr.SourceHash}
				default:
					d.Label = "content"
				}
			}
			c.Decisions[j] = d
		}
		classes[id] = c
	}
	cuts, err := Plan(tr, p, classes)
	if err != nil {
		t.Fatal(err)
	}
	if len(cuts.Ranges) != 1 {
		t.Fatal("fixture must produce one merged cut")
	}
	learning := AdLearning{Scope: "show", Policy: p, Classifications: classes, ApprovedDigest: Digest(cuts)}
	seeds, err := AdSeeds(tr, learning, cuts)
	if err != nil {
		t.Fatal(err)
	}
	if len(seeds) != 2 || seeds[0].Label != "paid_ad" || seeds[1].Label != "cross_promo" || seeds[0].LastID != tr.Units[14].ID || seeds[1].LastID != tr.Units[29].ID {
		t.Fatalf("mixed or amplified seeds: %+v", seeds)
	}
	learning.ApprovedDigest = ""
	if _, err := AdSeeds(tr, learning, cuts); err == nil {
		t.Fatal("unapproved learning accepted")
	}
}
func TestMixedCoverageRequiresUnknownReadReceiptsAndRejectsConflicts(t *testing.T) {
	tr, p := fixture()
	bs, _ := Blocks(tr, p)
	b := bs[0]
	known := map[string]AdKnownUnit{}
	for i := 3; i < 10; i++ {
		known[tr.Units[i].ID] = AdKnownUnit{Label: "paid_ad", Evidence: AdEvidence{tr.SourceHash, tr.SourceHash}}
	}
	c := Classification{BlockID: b.ID, BlockDigest: b.Digest, TranscriptDigest: Digest(tr), PromptVersion: PromptVersion, Model: "luna", Decisions: []Decision{{FirstID: tr.Units[0].ID, LastID: tr.Units[2].ID, Label: "content", Reason: "case"}, {FirstID: tr.Units[10].ID, LastID: tr.Units[b.Last].ID, Label: "content", Reason: "case"}}}
	reads := map[int]bool{}
	for i := 0; i <= b.Last; i++ {
		if _, ok := known[tr.Units[i].ID]; !ok {
			reads[i] = true
		}
	}
	merged, err := MergeKnownAdDecisions(tr, b, c, known, reads)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Decisions) != 3 || merged.Decisions[1].Evidence == nil {
		t.Fatalf("missing evidence: %+v", merged)
	}
	delete(reads, 0)
	if _, err := MergeKnownAdDecisions(tr, b, c, known, reads); err == nil {
		t.Fatal("unread unknown ID accepted")
	}
	reads[0] = true
	c.Decisions = []Decision{{FirstID: tr.Units[0].ID, LastID: tr.Units[b.Last].ID, Label: "content", Reason: "case"}}
	if _, err := MergeKnownAdDecisions(tr, b, c, known, reads); err == nil {
		t.Fatal("machine label overwritten")
	}
}
func TestAcousticEvidenceUsesOnlyInteriorUnitsAndConfiguredCategory(t *testing.T) {
	tr, p := fixture()
	ref := AdReference{TextDigest: NativeAdTextDigest(tr.Units[:10]), ID: tr.SourceHash, Digest: tr.SourceHash, SourceHash: tr.SourceHash, CutsDigest: tr.SourceHash, DurationMS: 10000, Label: "cross_promo"}
	report := AdMatchReport{Algorithm: AdAlgorithm, SourceHash: tr.SourceHash, Catalog: AdCatalog{Algorithm: AdAlgorithm, Scope: "show", References: []AdReference{ref}}, DurationMS: tr.DurationMS, Matches: []AdMatch{{ReferenceID: ref.ID, Label: ref.Label, StartMS: 0, EndMS: 10000, MinCorrelation: .99}}}
	known, err := KnownAdUnits(tr, report, p)
	if err != nil || len(known) != 0 {
		t.Fatalf("policy category ignored: %+v %v", known, err)
	}
	p.Remove = append(p.Remove, "cross_promo")
	known, err = KnownAdUnits(tr, report, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := known[tr.Units[0].ID]; ok {
		t.Fatal("first boundary unit autolabelled")
	}
	if len(known) != 9 {
		t.Fatalf("wrong interior coverage %d", len(known))
	}
	tr.Units[5].Text = "a different offer underneath the same music bed"
	known, err = KnownAdUnits(tr, report, p)
	if err != nil || len(known) != 0 {
		t.Fatalf("shared-bed changed voice auto-labelled: %+v %v", known, err)
	}
	report.Matches[0].EndMS++
	if _, err := KnownAdUnits(tr, report, p); err == nil {
		t.Fatal("invented match duration accepted")
	}
}
