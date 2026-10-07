package podcast

import (
	"fmt"
	"strings"
	"testing"
)

func fixture() (Transcript, Policy) {
	t := Transcript{SchemaVersion: Version, SourceHash: "sha256:" + strings.Repeat("a", 64), Provider: "apple_speech", ProviderVersion: "test", Language: "en_US", DurationMS: 120000}
	for i := 0; i < 120; i++ {
		t.Units = append(t.Units, Unit{fmt.Sprintf("u%06d", i+1), int64(i * 1000), int64(i*1000 + 800), "word", "native_attributed_run"})
	}
	p := DefaultPolicy()
	p.WindowMS = 30000
	p.OverlapMS = 5000
	return t, p
}
func decisions(t Transcript, p Policy) map[string]Classification {
	bs, _ := Blocks(t, p)
	cs := map[string]Classification{}
	for _, b := range bs {
		c := Classification{BlockID: b.ID, BlockDigest: b.Digest, TranscriptDigest: Digest(t), PromptVersion: PromptVersion, Model: "orchestrator", Reasoning: "high"}
		for i := b.First; i <= b.Last; i++ {
			label := "content"
			if i >= 20 && i <= 25 {
				label = "paid_ad"
			}
			c.Decisions = append(c.Decisions, Decision{t.Units[i].ID, t.Units[i].ID, label, "evidence"})
		}
		cs[b.ID] = c
	}
	return cs
}
func TestPlannerCrossBlockAdAndNativeEvidence(t *testing.T) {
	tr, p := fixture()
	c, err := Plan(tr, p, decisions(tr, p))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Ranges) != 1 || c.Ranges[0].StartMS != 20000 || c.Ranges[0].EndMS != 25800 || c.RemovedMS != 5800 {
		t.Fatalf("cuts=%+v", c)
	}
	if err := VerifyCuts(tr, c); err != nil {
		t.Fatal(err)
	}
	c.Ranges[0].StartMS++
	c.RemovedMS--
	if VerifyCuts(tr, c) == nil {
		t.Fatal("invented timestamp accepted")
	}
}
func TestPlannerFailsClosed(t *testing.T) {
	for _, kind := range []string{"missing", "gap", "invented", "overlap", "uncertain", "wrong_digest", "excessive", "native_gap"} {
		t.Run(kind, func(t *testing.T) {
			tr, p := fixture()
			cs := decisions(tr, p)
			bs, _ := Blocks(tr, p)
			b := bs[0]
			c := cs[b.ID]
			switch kind {
			case "missing":
				delete(cs, b.ID)
			case "gap":
				c.Decisions = c.Decisions[1:]
				cs[b.ID] = c
			case "invented":
				c.Decisions[0].FirstID = "u999999"
				cs[b.ID] = c
			case "overlap":
				c.Decisions[len(c.Decisions)-1].Label = "house_promo"
				cs[b.ID] = c
			case "uncertain":
				c.Decisions[5].Label = "uncertain"
				cs[b.ID] = c
			case "wrong_digest":
				c.TranscriptDigest = tr.SourceHash
				cs[b.ID] = c
			case "excessive":
				p.MaxRemovedFraction = .01
				cs = decisions(tr, p)
			case "native_gap":
				tr.Units[20].EndMS = 20001
				for i := 21; i < len(tr.Units); i++ {
					tr.Units[i].StartMS += 3000
					tr.Units[i].EndMS += 3000
				}
				tr.DurationMS += 3000
				cs = decisions(tr, p)
			}
			if _, err := Plan(tr, p, cs); err == nil {
				t.Fatal("unsafe classification accepted")
			}
		})
	}
}
func TestTranscriptRejectsFabricatedTimingsAndIdentity(t *testing.T) {
	tr, _ := fixture()
	tr.Units[0].Timing = "estimated"
	if tr.Validate() == nil {
		t.Fatal("estimated times accepted")
	}
	tr.Units[0].Timing = "native_result"
	tr.Units[1].ID = tr.Units[0].ID
	if tr.Validate() == nil {
		t.Fatal("duplicate ID accepted")
	}
}
func TestAllContentHasNoCuts(t *testing.T) {
	tr, p := fixture()
	cs := decisions(tr, p)
	for id, c := range cs {
		for i := range c.Decisions {
			c.Decisions[i].Label = "content"
		}
		cs[id] = c
	}
	cuts, err := Plan(tr, p, cs)
	if err != nil || len(cuts.Ranges) != 0 || cuts.RemovedMS != 0 {
		t.Fatalf("%+v %v", cuts, err)
	}
}
