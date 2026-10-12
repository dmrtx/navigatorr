package tools

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/podcast"
	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/server"
)

func TestPodcastReviewDeliversBoundedPagesBeforeApproval(t *testing.T) {
	for _, text := range []string{strings.Repeat("漢", 260), strings.Repeat("<", 260)} {
		t.Run(text[:3], func(t *testing.T) {
			dir := t.TempDir()
			st, err := store.Open(filepath.Join(dir, "actions.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			policy := podcast.DefaultPolicy()
			tr := podcast.Transcript{SchemaVersion: podcast.Version, Provider: "apple_speech", ProviderVersion: "fixture", Language: "en_US", SourceHash: "sha256:" + strings.Repeat("a", 64), DurationMS: 170000}
			for i := 0; i < 170; i++ {
				tr.Units = append(tr.Units, podcast.Unit{ID: fmt.Sprintf("u%06d", i), StartMS: int64(i * 1000), EndMS: int64(i*1000 + 500), Text: text, Timing: "native_result"})
			}
			blocks, err := podcast.Blocks(tr, policy)
			if err != nil {
				t.Fatal(err)
			}
			classes := map[string]podcast.Classification{}
			for _, block := range blocks {
				c := podcast.Classification{BlockID: block.ID, BlockDigest: block.Digest, TranscriptDigest: podcast.Digest(tr), PromptVersion: podcast.PromptVersion, Model: "fixture"}
				for i := block.First; i <= block.Last; i++ {
					label := "content"
					if i%8 == 3 {
						label = "paid_ad"
					}
					c.Decisions = append(c.Decisions, podcast.Decision{FirstID: tr.Units[i].ID, LastID: tr.Units[i].ID, Label: label, Reason: "fixture label"})
				}
				classes[block.ID] = c
			}
			cuts, err := podcast.Plan(tr, policy, classes)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint := filepath.Join(dir, "session.json")
			if err := podcast.WriteJSON(checkpoint, map[string]any{"version": podcast.Version, "policy": policy, "transcript": tr, "blocks": blocks, "classifications": classes, "cuts": cuts}); err != nil {
				t.Fatal(err)
			}
			state, err := json.Marshal(map[string]any{"podcast_session": checkpoint, "source_sha256": strings.TrimPrefix(tr.SourceHash, "sha256:"), "podcast": map[string]any{"policy_digest": podcast.Digest(policy), "transcript_digest": podcast.Digest(tr)}})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.CreateActionInstance(store.ActionInstance{ID: "review", ActionName: "clean_podcast_ads", Status: action.StatusWaitingDecision, WaitingCondition: "podcast_review", StateJSON: string(state)}); err != nil {
				t.Fatal(err)
			}
			s := server.NewMCPServer("test", "0.0.0")
			registerPodcastTools(s, action.NewEngine(action.EngineDeps{Store: st, Config: &config.Config{}}))
			read := func(offset int) map[string]any {
				t.Helper()
				result := callTool(t, s, "podcast_review", map[string]any{"id": "review", "offset": offset})
				encoded := resultText(t, result)
				if result.IsError || len(encoded) > MaxActionResponseBytes {
					t.Fatalf("review page failed or exceeded MCP limit: %d bytes, %s", len(encoded), encoded)
				}
				var page map[string]any
				if err := json.Unmarshal([]byte(encoded), &page); err != nil {
					t.Fatal(err)
				}
				if page["error"] != nil || page["digest"] != podcast.Digest(cuts) {
					t.Fatalf("review page was replaced with a size fallback: %v", page)
				}
				return page
			}
			page := read(0)
			next := int(page["next_offset"].(float64))
			if next <= 0 || next >= 10 || page["has_more"] != true {
				t.Fatalf("large contexts must reduce page size: next=%d has_more=%v", next, page["has_more"])
			}
			var session struct {
				ReviewReads map[int]bool `json:"review_reads"`
			}
			if err := podcast.ReadJSON(checkpoint, &session); err != nil {
				t.Fatal(err)
			}
			if len(session.ReviewReads) != next || session.ReviewReads[next] {
				t.Fatalf("receipts include undelivered cuts: next=%d reads=%v", next, session.ReviewReads)
			}
			approve := func() bool {
				return callTool(t, s, "podcast_review", map[string]any{"id": "review", "digest": podcast.Digest(cuts), "approve": true}).IsError
			}
			if !approve() {
				t.Fatal("approval accepted before all pages were delivered")
			}
			for page["has_more"] == true {
				page = read(next)
				at := int(page["next_offset"].(float64))
				if at <= next || len(page["boundaries"].([]any)) != at-next {
					t.Fatal("review pagination did not deliver every intermediate cut")
				}
				next = at
			}
			if next != len(cuts.Ranges) || approve() {
				t.Fatal("complete delivered review could not be approved")
			}
		})
	}
}

func TestPodcastClassificationRetryPreservesTranscriptAndSavedWork(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "actions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	policy := podcast.DefaultPolicy()
	tr := podcast.Transcript{SchemaVersion: podcast.Version, Provider: "apple_speech", ProviderVersion: "fixture", Language: "en_US", SourceHash: "sha256:" + strings.Repeat("a", 64), DurationMS: 3000}
	for i := 0; i < 3; i++ {
		tr.Units = append(tr.Units, podcast.Unit{ID: fmt.Sprintf("u%06d", i+1), StartMS: int64(i * 1000), EndMS: int64(i*1000 + 900), Text: "episode discussion", Timing: "native_result"})
	}
	blocks, err := podcast.Blocks(tr, policy)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := filepath.Join(dir, "session.json")
	if err := podcast.WriteJSON(checkpoint, map[string]any{"version": podcast.Version, "policy": policy, "transcript": tr, "blocks": blocks}); err != nil {
		t.Fatal(err)
	}
	state, _ := json.Marshal(map[string]any{"podcast_session": checkpoint, "source_sha256": strings.TrimPrefix(tr.SourceHash, "sha256:"), "podcast": map[string]any{"policy_digest": podcast.Digest(policy), "transcript_digest": podcast.Digest(tr)}})
	if err := st.CreateActionInstance(store.ActionInstance{ID: "classify", ActionName: "clean_podcast_ads", Status: action.StatusWaitingDecision, WaitingCondition: "podcast_classification", StateJSON: string(state)}); err != nil {
		t.Fatal(err)
	}
	newServer := func() *server.MCPServer {
		s := server.NewMCPServer("test", "0.0.0")
		registerPodcastTools(s, action.NewEngine(action.EngineDeps{Store: st, Config: &config.Config{}}))
		return s
	}
	s := newServer()
	b := blocks[0]
	c := podcast.Classification{BlockID: b.ID, BlockDigest: b.Digest, TranscriptDigest: podcast.Digest(tr), PromptVersion: podcast.PromptVersion, Model: "orchestrator", Reasoning: strings.Repeat("This block contains substantive episode discussion. ", 4), Decisions: []podcast.Decision{{FirstID: tr.Units[0].ID, LastID: tr.Units[2].ID, Label: "content", Reason: "episode discussion"}}}
	classify := func(c podcast.Classification, want string) {
		t.Helper()
		raw, _ := json.Marshal(c)
		result := callTool(t, s, "podcast_classify", map[string]any{"id": "classify", "classification": string(raw)})
		if want == "" {
			if result.IsError {
				t.Fatal(resultText(t, result))
			}
		} else if !result.IsError || !strings.Contains(resultText(t, result), want) {
			t.Fatalf("expected %q, got %s", want, resultText(t, result))
		}
	}
	classify(c, "read every podcast_block page")
	page := callTool(t, s, "podcast_block", map[string]any{"id": "classify", "block_id": b.ID})
	if page.IsError {
		t.Fatal(resultText(t, page))
	}
	for _, tc := range []struct {
		edit func(*podcast.Classification)
		want string
	}{
		{func(c *podcast.Classification) {
			c.Reasoning = strings.Repeat("r", podcast.MaxClassificationReasoningBytes+1)
		}, "reasoning exceeds 1024 bytes"},
		{func(c *podcast.Classification) { c.TranscriptDigest = tr.SourceHash }, "transcript_digest mismatch"},
		{func(c *podcast.Classification) { c.BlockDigest = tr.SourceHash }, "block_digest mismatch"},
	} {
		bad := c
		tc.edit(&bad)
		classify(bad, tc.want)
	}
	classify(c, "")
	// A later failed replacement must not erase the already accepted block.
	bad := c
	bad.Reasoning = strings.Repeat("r", podcast.MaxClassificationReasoningBytes+1)
	classify(bad, "reasoning exceeds 1024 bytes")
	var saved struct {
		Transcript      podcast.Transcript                `json:"transcript"`
		Classifications map[string]podcast.Classification `json:"classifications"`
		Reads           map[string]map[int]bool           `json:"reads"`
	}
	if err := podcast.ReadJSON(checkpoint, &saved); err != nil {
		t.Fatal(err)
	}
	if podcast.Digest(saved.Transcript) != podcast.Digest(tr) || podcast.Digest(saved.Classifications[b.ID]) != podcast.Digest(c) || len(saved.Reads[b.ID]) != len(tr.Units) {
		t.Fatal("retry changed the transcript, accepted classification or read receipts")
	}
	s = newServer()
	manifest := callTool(t, s, "podcast_blocks", map[string]any{"id": "classify"})
	var data struct {
		Classified int            `json:"classified_blocks"`
		Limits     map[string]int `json:"classification_limits"`
	}
	if err := json.Unmarshal([]byte(resultText(t, manifest)), &data); err != nil {
		t.Fatal(err)
	}
	if manifest.IsError || data.Classified != 1 || data.Limits["reasoning_bytes"] != podcast.MaxClassificationReasoningBytes {
		t.Fatalf("restart lost classification or limits: %+v", data)
	}
}
