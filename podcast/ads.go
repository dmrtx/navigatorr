package podcast

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// Changing preprocessing, candidate selection or verification changes the
// immutable catalog/request identity, never just a runtime threshold.
const AdAlgorithm = "pcm8k-spectral-text-v1"
const MaxAdReferences = 128
const MaxAdLearningBytes = 8 << 20

type AdReference struct {
	TextDigest string `json:"text_digest"`
	ID         string `json:"id"`
	Label      string `json:"label"`
	Digest     string `json:"digest"`
	DurationMS int64  `json:"duration_ms"`
	SourceHash string `json:"source_hash"`
	FirstID    string `json:"first_id"`
	LastID     string `json:"last_id"`
	CutsDigest string `json:"cuts_digest"`
	Revoked    bool   `json:"revoked,omitempty"`
}
type AdCatalog struct {
	Algorithm  string        `json:"algorithm"`
	Scope      string        `json:"scope"`
	References []AdReference `json:"references"`
}

func (c AdCatalog) Validate() error {
	if c.Algorithm != AdAlgorithm || c.Scope == "" || len(c.Scope) > 128 || len(c.References) > MaxAdReferences {
		return fmt.Errorf("invalid ad catalog identity/size")
	}
	seen := map[string]bool{}
	for _, r := range c.References {
		if !ValidHash(r.ID) || !ValidHash(r.Digest) || !ValidHash(r.TextDigest) || !ValidHash(r.SourceHash) || !ValidHash(r.CutsDigest) || r.DurationMS < 8000 || r.DurationMS > 180000 || !Label(r.Label) || r.Label == "content" || r.Label == "uncertain" || seen[r.ID] {
			return fmt.Errorf("invalid ad reference")
		}
		seen[r.ID] = true
	}
	return nil
}

type AdMatch struct {
	ReferenceID    string  `json:"reference_id"`
	Label          string  `json:"label"`
	StartMS        int64   `json:"start_ms"`
	EndMS          int64   `json:"end_ms"`
	MinCorrelation float64 `json:"min_correlation"`
}
type AdMatchReport struct {
	Algorithm     string    `json:"algorithm"`
	SourceHash    string    `json:"source_hash"`
	Catalog       AdCatalog `json:"catalog"`
	DurationMS    int64     `json:"duration_ms"`
	WallSeconds   float64   `json:"wall_seconds"`
	DecodeSeconds float64   `json:"decode_seconds"`
	SearchSeconds float64   `json:"search_seconds"`
	Matches       []AdMatch `json:"matches"`
}

func (r AdMatchReport) Validate() error {
	if r.Algorithm != AdAlgorithm || !ValidHash(r.SourceHash) || r.DurationMS <= 0 || r.DurationMS > 43200000 || len(r.Matches) > 1000 {
		return fmt.Errorf("invalid ad match report")
	}
	if err := r.Catalog.Validate(); err != nil {
		return err
	}
	refs := map[string]AdReference{}
	for _, ref := range r.Catalog.References {
		refs[ref.ID] = ref
	}
	for _, m := range r.Matches {
		ref, ok := refs[m.ReferenceID]
		if !ok || ref.Revoked || m.Label != ref.Label || m.StartMS < 0 || m.EndMS <= m.StartMS || m.EndMS > r.DurationMS || m.EndMS-m.StartMS != ref.DurationMS || !(m.MinCorrelation >= .94 && m.MinCorrelation <= 1.00001) {
			return fmt.Errorf("invalid verified ad match")
		}
	}
	return nil
}

type AdEvidence struct {
	ReferenceID string `json:"reference_id"`
	MatchDigest string `json:"match_digest"`
}
type AdKnownUnit struct {
	Label    string     `json:"label"`
	Evidence AdEvidence `json:"evidence"`
}

// Boundary units remain for the orchestrator. A fingerprint does not extend
// native ASR timings or provide synthetic transcript units.
func KnownAdUnits(t Transcript, r AdMatchReport, p Policy) (map[string]AdKnownUnit, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if r.SourceHash != t.SourceHash || abs64(r.DurationMS-t.DurationMS) > 250 {
		return nil, fmt.Errorf("match/transcript identity differs")
	}
	remove := map[string]bool{}
	for _, l := range p.Remove {
		remove[l] = true
	}
	out := map[string]AdKnownUnit{}
	conflicts := map[string]bool{}
	matchDigest := Digest(r)
	refs := map[string]AdReference{}
	for _, ref := range r.Catalog.References {
		refs[ref.ID] = ref
	}
	for _, m := range r.Matches {
		var candidate []Unit
		for _, u := range t.Units {
			midpoint := (u.StartMS + u.EndMS) / 2
			if midpoint >= m.StartMS && midpoint < m.EndMS {
				candidate = append(candidate, u)
			}
		}
		if NativeAdTextDigest(candidate) != refs[m.ReferenceID].TextDigest {
			continue
		}
		for _, u := range t.Units {
			if u.StartMS < m.StartMS+30 || u.EndMS > m.EndMS-30 {
				continue
			}
			k := AdKnownUnit{m.Label, AdEvidence{m.ReferenceID, matchDigest}}
			if old, ok := out[u.ID]; ok && old.Label != k.Label {
				conflicts[u.ID] = true
			}
			if !conflicts[u.ID] {
				out[u.ID] = k
			}
		}
	}
	for id := range conflicts {
		delete(out, id)
	}
	for id, k := range out {
		if !remove[k.Label] {
			delete(out, id)
		}
	}
	return out, nil
}

// Acoustic energy alone cannot distinguish a common music bed with a changed
// quiet voice. The existing original-audio ASR must independently agree on ALL
// seed words before any automatic labels are admitted. Variations fall to LLM.
func NativeAdTextDigest(units []Unit) string {
	var text strings.Builder
	for _, u := range units {
		text.WriteString(u.Text)
		text.WriteByte(' ')
	}
	words := strings.FieldsFunc(strings.ToLower(text.String()), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	// Native ASR may render the same spoken number as "3" or "three" depending
	// on episode context. Canonicalize only equivalent small English integers;
	// different offers/numbers still produce different digests.
	for i, w := range words {
		if n, ok := smallEnglishNumbers[w]; ok {
			words[i] = n
		}
	}
	return Digest(strings.Join(words, " "))
}

var smallEnglishNumbers = map[string]string{"zero": "0", "one": "1", "two": "2", "three": "3", "four": "4", "five": "5", "six": "6", "seven": "7", "eight": "8", "nine": "9", "ten": "10", "eleven": "11", "twelve": "12", "thirteen": "13", "fourteen": "14", "fifteen": "15", "sixteen": "16", "seventeen": "17", "eighteen": "18", "nineteen": "19", "twenty": "20"}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

type AdLearning struct {
	Scope           string                    `json:"scope"`
	Policy          Policy                    `json:"policy"`
	Classifications map[string]Classification `json:"classifications"`
	ApprovedDigest  string                    `json:"approved_digest"`
}

func (l AdLearning) Validate(c Cuts) error {
	if l.Scope == "" || len(l.Scope) > 128 || !l.Policy.KnownAdsFirstPass || Digest(l.Policy) != c.PolicyDigest || len(l.Classifications) > 1500 || l.Policy.ReviewRequired && l.ApprovedDigest != Digest(c) {
		return fmt.Errorf("invalid learning approval/scope/policy")
	}
	if raw, err := json.Marshal(l); err != nil || len(raw) > MaxAdLearningBytes {
		return fmt.Errorf("podcast learning proof exceeds 8 MiB; compact classification reasons/ranges")
	}
	return l.Policy.Validate()
}

// Seeds come from canonical classifications, not merged cuts. Never learn
// fingerprint decisions again: that would amplify automatic evidence.
func AdSeeds(t Transcript, l AdLearning, c Cuts) ([]Decision, error) {
	if err := l.Validate(c); err != nil {
		return nil, err
	}
	computed, err := Plan(t, l.Policy, l.Classifications)
	if err != nil {
		return nil, err
	}
	if Digest(computed) != Digest(c) {
		return nil, fmt.Errorf("learning classifications differ from reviewed cuts")
	}
	bs, _ := Blocks(t, l.Policy)
	labels := make([]string, len(t.Units))
	machine := make([]bool, len(t.Units))
	for _, b := range bs {
		class := l.Classifications[b.ID]
		ls, _ := Labels(t, b, class)
		for i, label := range ls {
			labels[b.First+i] = label
		}
		idx := b.First
		for _, d := range class.Decisions {
			for idx <= b.Last {
				machine[idx] = machine[idx] || d.Evidence != nil
				last := t.Units[idx].ID == d.LastID
				idx++
				if last {
					break
				}
			}
		}
	}
	remove := map[string]bool{}
	for _, label := range l.Policy.Remove {
		remove[label] = true
	}
	var out []Decision
	for i := 0; i < len(labels); {
		if machine[i] || !remove[labels[i]] {
			i++
			continue
		}
		first := i
		label := labels[i]
		for i+1 < len(labels) && !machine[i+1] && labels[i+1] == label && t.Units[i+1].StartMS-t.Units[i].EndMS <= 2000 {
			i++
		}
		last := i
		i++
		duration := t.Units[last].EndMS - t.Units[first].StartMS
		if duration < 8000 || duration > 180000 || last-first < 12 {
			continue
		}
		out = append(out, Decision{FirstID: t.Units[first].ID, LastID: t.Units[last].ID, Label: label, Reason: "reviewed classification"})
	}
	return out, nil
}

// Merge only server-verified evidence with submitted ID ranges. Unknown IDs
// still require the actual LLM read receipts; arbitrary evidence is rejected.
func MergeKnownAdDecisions(t Transcript, b Block, c Classification, known map[string]AdKnownUnit, reads map[int]bool) (Classification, error) {
	byID := map[string]int{}
	for i := b.First; i <= b.Last; i++ {
		byID[t.Units[i].ID] = i
	}
	ds := make([]*Decision, b.Last-b.First+1)
	prev := b.First - 1
	for _, d := range c.Decisions {
		first, ok := byID[d.FirstID]
		last, ok2 := byID[d.LastID]
		if !ok || !ok2 || first <= prev || last < first || !Label(d.Label) || d.Reason == "" || len(d.Reason) > 1024 || d.Evidence != nil {
			return c, fmt.Errorf("invalid submitted ID ranges or evidence")
		}
		prev = last
		for i := first; i <= last; i++ {
			cp := d
			ds[i-b.First] = &cp
		}
	}
	var merged []Decision
	for i := b.First; i <= b.Last; i++ {
		u := t.Units[i]
		d := ds[i-b.First]
		if k, ok := known[u.ID]; ok {
			if d != nil && d.Label != k.Label {
				return c, fmt.Errorf("known ad conflict at %s; revoke its reference and start a new action", u.ID)
			}
			d = &Decision{Label: k.Label, Reason: "verified complete acoustic match", Evidence: &k.Evidence}
		} else if d == nil || !reads[i-b.First] {
			return c, fmt.Errorf("unknown unit %s requires reading and classification", u.ID)
		}
		cp := *d
		cp.FirstID = u.ID
		cp.LastID = u.ID
		if len(merged) > 0 && merged[len(merged)-1].Label == cp.Label && merged[len(merged)-1].Reason == cp.Reason && Digest(merged[len(merged)-1].Evidence) == Digest(cp.Evidence) {
			merged[len(merged)-1].LastID = u.ID
		} else {
			merged = append(merged, cp)
		}
	}
	c.Decisions = merged
	_, err := Labels(t, b, c)
	return c, err
}
