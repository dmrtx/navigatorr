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
			c.Decisions = append(c.Decisions, Decision{FirstID: t.Units[i].ID, LastID: t.Units[i].ID, Label: label, Reason: "evidence"})
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

func TestClassificationReasoningCompatibilityAndByteLimit(t *testing.T) {
	tr, p := fixture()
	bs, _ := Blocks(tr, p)
	base := decisions(tr, p)[bs[0].ID]
	for _, reasoning := range []string{"", "high", strings.Repeat("a", 65), strings.Repeat("a", MaxClassificationReasoningBytes), strings.Repeat("á", MaxClassificationReasoningBytes/2)} {
		c := base
		c.Reasoning = reasoning
		if _, err := Labels(tr, bs[0], c); err != nil {
			t.Fatalf("valid %d-byte reasoning rejected: %v", len(reasoning), err)
		}
	}
	for _, reasoning := range []string{strings.Repeat("a", MaxClassificationReasoningBytes+1), strings.Repeat("á", MaxClassificationReasoningBytes/2+1)} {
		c := base
		c.Reasoning = reasoning
		_, err := Labels(tr, bs[0], c)
		if err == nil || !strings.Contains(err.Error(), "reasoning exceeds 1024 bytes") || !strings.Contains(err.Error(), "decisions[].reason") {
			t.Fatalf("oversized metadata needs an actionable error: %v", err)
		}
	}
}

func TestClassificationErrorsIdentifyTheIncorrectField(t *testing.T) {
	tr, p := fixture()
	bs, _ := Blocks(tr, p)
	base := decisions(tr, p)[bs[0].ID]
	for _, tc := range []struct {
		name string
		edit func(*Classification)
		want string
	}{
		{"block_id", func(c *Classification) { c.BlockID = "wrong" }, "block_id mismatch"},
		{"block_digest", func(c *Classification) { c.BlockDigest = tr.SourceHash }, "block_digest mismatch"},
		{"transcript_digest", func(c *Classification) { c.TranscriptDigest = tr.SourceHash }, "transcript_digest mismatch"},
		{"prompt_version", func(c *Classification) { c.PromptVersion = "wrong" }, "prompt_version mismatch"},
		{"missing_model", func(c *Classification) { c.Model = "" }, "model must be non-empty"},
		{"blank_model", func(c *Classification) { c.Model = "  " }, "model must be non-empty"},
		{"oversized_model", func(c *Classification) { c.Model = strings.Repeat("m", 129) }, "model must be non-empty"},
		{"empty_decisions", func(c *Classification) { c.Decisions = nil }, "decisions must contain"},
		{"too_many_decisions", func(c *Classification) { c.Decisions = append(c.Decisions, c.Decisions[0]) }, "decisions must contain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			c.Reasoning = strings.Repeat("valid provenance ", 10)
			tc.edit(&c)
			_, err := Labels(tr, bs[0], c)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
}

func TestClassificationCoverageNamesMissingBlocksAndRejectsExtras(t *testing.T) {
	tr, p := fixture()
	bs, _ := Blocks(tr, p)
	cs := decisions(tr, p)
	delete(cs, bs[0].ID)
	delete(cs, bs[2].ID)
	_, err := Plan(tr, p, cs)
	if err == nil || !strings.Contains(err.Error(), "missing blocks: "+bs[0].ID+", "+bs[2].ID) {
		t.Fatalf("missing blocks must be named in transcript order: %v", err)
	}
	cs = decisions(tr, p)
	cs["invented"] = cs[bs[0].ID]
	if _, err := Plan(tr, p, cs); err == nil || !strings.Contains(err.Error(), "outside this transcript") {
		t.Fatalf("unexpected block accepted: %v", err)
	}
}
