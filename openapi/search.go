package openapi

import (
	"sort"
	"strings"
	"unicode"
)

// stopWords are dropped from natural-language queries. They carry no signal
// about which operation is meant.
var stopWords = map[string]bool{
	"a": true, "an": true, "the": true, "is": true, "are": true, "was": true,
	"what": true, "which": true, "who": true, "when": true, "where": true,
	"now": true, "of": true, "to": true, "for": true, "before": true,
	"after": true, "on": true, "in": true, "at": true, "by": true, "my": true,
	"me": true, "how": true, "do": true, "does": true, "i": true, "and": true,
	"or": true, "with": true, "from": true, "it": true, "this": true,
	"that": true, "can": true, "all": true, "any": true, "be": true,
	"get": true, "please": true,
}

const (
	weightPrimary     = 3   // token found in path, summary or tags
	weightDescription = 1   // token found only in the description
	weightPhrase      = 100 // the whole query appears verbatim
)

// queryTokens lowercases the query, splits it on non-alphanumerics, drops
// short and stop words and applies a light stem. A query with a single token
// word is not stemmed, so single-word searches behave as plain substring searches.
func queryTokens(query string) []string {
	fields := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	var raw []string
	seen := map[string]bool{}
	for _, f := range fields {
		if len(f) < 2 || stopWords[f] || seen[f] {
			continue
		}
		seen[f] = true
		raw = append(raw, f)
	}
	if len(fields) <= 1 {
		return raw // a one-word query is matched as typed
	}
	out := make([]string, 0, len(raw))
	seen = map[string]bool{}
	for _, f := range raw {
		s := stem(f)
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// stem strips one trailing "ing", "ed", "es" or "s" when at least three
// characters remain.
func stem(w string) string {
	for _, suf := range []string{"ing", "ed", "es", "s"} {
		if strings.HasSuffix(w, suf) && len(w)-len(suf) >= 3 {
			return strings.TrimSuffix(w, suf)
		}
	}
	return w
}

// scored is one endpoint that matched at least one query token.
type scored struct {
	summary EndpointSummary
	matched int
	score   int
}

// rank scores every endpoint of the index against the query. It returns the
// endpoints matching at least one token and the number of tokens in the query.
func (idx *Index) rank(query string) ([]scored, int) {
	phrase := strings.ToLower(strings.TrimSpace(query))
	tokens := queryTokens(query)
	if len(tokens) == 0 {
		if phrase == "" {
			return nil, 0
		}
		tokens = []string{phrase}
	}

	var out []scored
	for path, methods := range idx.Endpoints {
		for m, detail := range methods {
			primary := strings.ToLower(path + "\n" + detail.Summary + "\n" + strings.Join(detail.Tags, "\n"))
			desc := strings.ToLower(detail.Description)

			matched, score := 0, 0
			for _, tok := range tokens {
				switch {
				case strings.Contains(primary, tok):
					matched++
					score += weightPrimary
				case strings.Contains(desc, tok):
					matched++
					score += weightDescription
				}
			}
			if matched == 0 {
				continue
			}
			if phrase != "" && (strings.Contains(primary, phrase) || strings.Contains(desc, phrase)) {
				score += weightPhrase
			}
			t := ""
			if len(detail.Tags) > 0 {
				t = detail.Tags[0]
			}
			out = append(out, scored{
				summary: EndpointSummary{Service: idx.Service, Method: m, Path: path, Summary: detail.Summary, Tag: t},
				matched: matched,
				score:   score,
			})
		}
	}
	return out, len(tokens)
}

// selectRanked picks the endpoints to return. Those matching every token
// always count; when fewer than three do, near misses are added: with no full
// match at all those matching at least half of the tokens (rounded up), and
// with a query of three or more tokens those matching at least two thirds
// (and never fewer than two). Most tokens matched first, then best score; ties keep the stable
// service/path/method order.
func selectRanked(cands []scored, ntok int) []EndpointSummary {
	if len(cands) == 0 {
		return nil
	}
	full := 0
	for _, c := range cands {
		if c.matched == ntok {
			full++
		}
	}
	need := ntok
	switch {
	case full == 0:
		need = (ntok + 1) / 2
	case full < 3 && ntok >= 3:
		need = (ntok*2 + 2) / 3
		if need < 2 {
			need = 2
		}
	}
	if need < 1 {
		need = 1
	}
	kept := cands[:0:0]
	for _, c := range cands {
		if c.matched >= need {
			kept = append(kept, c)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool {
		a, b := kept[i], kept[j]
		if a.matched != b.matched {
			return a.matched > b.matched
		}
		if a.score != b.score {
			return a.score > b.score
		}
		if a.summary.Service != b.summary.Service {
			return a.summary.Service < b.summary.Service
		}
		if a.summary.Path != b.summary.Path {
			return a.summary.Path < b.summary.Path
		}
		return a.summary.Method < b.summary.Method
	})
	res := make([]EndpointSummary, len(kept))
	for i, k := range kept {
		res[i] = k.summary
	}
	return res
}
