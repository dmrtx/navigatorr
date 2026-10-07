package podcast

import "fmt"

func Blocks(t Transcript, p Policy) ([]Block, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	var blocks []Block
	td, pd := Digest(t), Digest(p)
	for first := 0; first < len(t.Units); {
		end := t.Units[first].StartMS + p.WindowMS
		last := first
		for last+1 < len(t.Units) && t.Units[last+1].StartMS < end {
			last++
		}
		b := Block{ID: fmt.Sprintf("b%04d", len(blocks)+1), First: first, Last: last}
		b.Digest = Digest(struct {
			Transcript  string
			Policy      string
			First, Last int
		}{td, pd, first, last})
		blocks = append(blocks, b)
		if last == len(t.Units)-1 {
			break
		}
		next := first + 1
		for next <= last && t.Units[next].StartMS < end-p.OverlapMS {
			next++
		}
		first = next
	}
	return blocks, nil
}
func Labels(t Transcript, b Block, c Classification) ([]string, error) {
	if c.BlockID != b.ID || c.BlockDigest != b.Digest || c.TranscriptDigest != Digest(t) || c.PromptVersion != PromptVersion || c.Model == "" || len(c.Model) > 128 || len(c.Reasoning) > 64 || len(c.Decisions) == 0 || len(c.Decisions) > b.Last-b.First+1 {
		return nil, fmt.Errorf("classification is not bound to this transcript/block/prompt")
	}
	labels := make([]string, b.Last-b.First+1)
	idx := b.First
	for _, d := range c.Decisions {
		if d.Evidence != nil && (!ValidHash(d.Evidence.ReferenceID) || !ValidHash(d.Evidence.MatchDigest)) {
			return nil, fmt.Errorf("invalid acoustic evidence identity")
		}
		if !Label(d.Label) || d.Reason == "" || len(d.Reason) > 1024 || idx > b.Last || t.Units[idx].ID != d.FirstID {
			return nil, fmt.Errorf("decisions must cover every unit in order, without gaps or duplicates")
		}
		found := false
		for ; idx <= b.Last; idx++ {
			labels[idx-b.First] = d.Label
			if t.Units[idx].ID == d.LastID {
				idx++
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("last_id is outside this block")
		}
	}
	if idx != b.Last+1 {
		return nil, fmt.Errorf("block coverage is incomplete")
	}
	return labels, nil
}

// Plan fails closed on missing coverage, uncertainty, conflicting overlap and
// untimed gaps within cuts. Times are exclusively derived from ASR IDs.
func Plan(t Transcript, p Policy, cs map[string]Classification) (Cuts, error) {
	bs, err := Blocks(t, p)
	if err != nil {
		return Cuts{}, err
	}
	if len(cs) != len(bs) {
		return Cuts{}, fmt.Errorf("classification coverage incomplete")
	}
	labels := make([]string, len(t.Units))
	ordered := make([]Classification, 0, len(bs))
	for _, b := range bs {
		c, ok := cs[b.ID]
		if !ok {
			return Cuts{}, fmt.Errorf("missing block %s", b.ID)
		}
		ls, err := Labels(t, b, c)
		if err != nil {
			return Cuts{}, err
		}
		ordered = append(ordered, c)
		for i, l := range ls {
			pos := b.First + i
			if l == "uncertain" {
				return Cuts{}, fmt.Errorf("uncertain unit %s needs reclassification", t.Units[pos].ID)
			}
			if labels[pos] != "" && labels[pos] != l {
				return Cuts{}, fmt.Errorf("overlap conflict at %s", t.Units[pos].ID)
			}
			labels[pos] = l
		}
	}
	cuts := Cuts{Version: Version, SourceHash: t.SourceHash, TranscriptDigest: Digest(t), PolicyDigest: Digest(p), ClassificationDigest: Digest(ordered), DurationMS: t.DurationMS, Ranges: []Cut{}}
	remove := map[string]bool{}
	for _, l := range p.Remove {
		remove[l] = true
	}
	for i := 0; i < len(labels); {
		if !remove[labels[i]] {
			i++
			continue
		}
		first := i
		last := i
		for last+1 < len(labels) && remove[labels[last+1]] {
			if t.Units[last+1].StartMS-t.Units[last].EndMS > 2000 {
				return Cuts{}, fmt.Errorf("untimed gap inside cut at %s needs review", t.Units[last].ID)
			}
			last++
		}
		a, z := t.Units[first], t.Units[last]
		end := z.EndMS
		if end > t.DurationMS {
			end = t.DurationMS
		}
		cuts.Ranges = append(cuts.Ranges, Cut{a.ID, z.ID, a.StartMS, end})
		cuts.RemovedMS += end - a.StartMS
		i = last + 1
	}
	if float64(cuts.RemovedMS)/float64(t.DurationMS) > p.MaxRemovedFraction {
		return Cuts{}, fmt.Errorf("removed fraction exceeds podcast policy; reclassify before rendering")
	}
	return cuts, cuts.Validate()
}
func VerifyCuts(t Transcript, c Cuts) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if c.SourceHash != t.SourceHash || c.TranscriptDigest != Digest(t) || c.DurationMS != t.DurationMS {
		return fmt.Errorf("cuts refer to a different transcript/audio")
	}
	idx := map[string]int{}
	for i, u := range t.Units {
		idx[u.ID] = i
	}
	for _, r := range c.Ranges {
		a, ok := idx[r.FirstID]
		z, ok2 := idx[r.LastID]
		if !ok || !ok2 || z < a || t.Units[a].StartMS != r.StartMS || min(t.Units[z].EndMS, t.DurationMS) != r.EndMS {
			return fmt.Errorf("cut times do not match native IDs")
		}
		for i := a; i < z; i++ {
			if t.Units[i+1].StartMS-t.Units[i].EndMS > 2000 {
				return fmt.Errorf("untimed gap inside cut")
			}
		}
	}
	return nil
}
