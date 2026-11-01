package arrservice

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/jakenesler/navigatorr/config"
)

// Policy decides whether a request may be sent to a service at all. It runs
// inside the service client, so every tool that reaches the service passes it,
// not only call_api.
type Policy interface {
	Check(method, path string, query map[string]string) error
}

// PolicyError is returned when a request does not pass a service's policy.
type PolicyError struct {
	Service string
	Reason  string
}

func (e *PolicyError) Error() string {
	return fmt.Sprintf("%s: request blocked by service policy: %s", e.Service, e.Reason)
}

func newPolicy(name string, cfg config.ServiceConfig) Policy {
	switch cfg.Policy {
	case config.PolicyPlex:
		return newPlexPolicy(name, cfg.AllowWrites)
	}
	return nil
}

type plexRule struct {
	methods []string
	re      *regexp.Regexp
	group   string // "" for reads
}

type plexPolicy struct {
	service string
	rules   []plexRule
	enabled map[string]bool
}

const (
	plexID  = `\d+`
	plexIDs = `\d+(?:,\d+)*`
)

func plexRe(pattern string) *regexp.Regexp {
	return regexp.MustCompile("^" + pattern + "$")
}

// Plex answers some GET requests by changing something (a library refresh is a
// GET), so reads are an allowlist of routes, not "every GET". Routes that echo
// the account token (/myplex, /security, /accounts) and server preferences are
// left out on purpose.
var plexRules = []plexRule{
	{methods: []string{"GET", "HEAD"}, re: plexRe(`/`)},
	{methods: []string{"GET", "HEAD"}, re: plexRe(`/identity`)},
	{methods: []string{"GET", "HEAD"}, re: plexRe(`/library/sections`)},
	{methods: []string{"GET", "HEAD"}, re: plexRe(`/library/sections/` + plexID)},
	{methods: []string{"GET", "HEAD"}, re: plexRe(`/library/sections/` + plexID + `/(?:all|folder|firstcharacter|genre|year|decade|collection|collections|unwatched|newest|recentlyadded|ondeck|contentrating|network|studio|resolution|label|country|sorts|filters|search|autocomplete)`)},
	{methods: []string{"GET", "HEAD"}, re: plexRe(`/library/(?:recentlyadded|ondeck)`)},
	{methods: []string{"GET", "HEAD"}, re: plexRe(`/library/metadata/` + plexIDs)},
	{methods: []string{"GET", "HEAD"}, re: plexRe(`/library/metadata/` + plexIDs + `/(?:children|allleaves|matches)`)},
	{methods: []string{"GET", "HEAD"}, re: plexRe(`/hubs/search`)},
	{methods: []string{"GET", "HEAD"}, re: plexRe(`/hubs/sections/` + plexID)},
	{methods: []string{"GET", "HEAD"}, re: plexRe(`/search`)},
	{methods: []string{"GET", "HEAD"}, re: plexRe(`/status/sessions`)},
	{methods: []string{"GET", "HEAD"}, re: plexRe(`/status/sessions/history/all`)},
	{methods: []string{"GET", "HEAD"}, re: plexRe(`/system/(?:agents|scanners)`)},

	{methods: []string{"GET"}, re: plexRe(`/library/sections/` + plexID + `/refresh`), group: config.PlexWriteRefresh},
	{methods: []string{"PUT"}, re: plexRe(`/library/metadata/` + plexID + `/refresh`), group: config.PlexWriteRefresh},
	{methods: []string{"POST"}, re: plexRe(`/library/sections`), group: config.PlexWriteLibraryNew},
	{methods: []string{"PUT"}, re: plexRe(`/library/sections/` + plexID), group: config.PlexWriteLibraryEdit},
	{methods: []string{"PUT"}, re: plexRe(`/library/sections/` + plexID + `/prefs`), group: config.PlexWriteLibraryEdit},
	{methods: []string{"PUT"}, re: plexRe(`/library/metadata/` + plexID + `/(?:match|unmatch)`), group: config.PlexWriteMatch},
	{methods: []string{"PUT"}, re: plexRe(`/library/metadata/` + plexID), group: config.PlexWriteMetaEdit},
}

func newPlexPolicy(service string, allowWrites []string) *plexPolicy {
	p := &plexPolicy{service: service, rules: plexRules, enabled: map[string]bool{}}
	for _, g := range allowWrites {
		p.enabled[g] = true
	}
	return p
}

// normalizePlexPath returns the lowercase route to match and any query keys
// that were written into the path. It refuses anything that could be read as
// a different route by Plex than by this check: encoded characters, dot
// segments, doubled slashes, backslashes and control characters.
func normalizePlexPath(raw string) (string, []string, error) {
	p := raw
	var keys []string
	if i := strings.IndexByte(p, '?'); i >= 0 {
		q, err := url.ParseQuery(p[i+1:])
		if err != nil {
			return "", nil, fmt.Errorf("unreadable query string in path")
		}
		for k := range q {
			keys = append(keys, k)
		}
		p = p[:i]
	}
	if strings.ContainsAny(p, "%\\#") {
		return "", nil, fmt.Errorf("encoded or special characters are not allowed in the path")
	}
	for _, r := range p {
		if r <= 0x20 || r == 0x7f {
			return "", nil, fmt.Errorf("spaces and control characters are not allowed in the path")
		}
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	if p != "/" {
		for _, seg := range strings.Split(p[1:], "/") {
			if seg == "" || seg == "." || seg == ".." {
				return "", nil, fmt.Errorf("empty or dot segments are not allowed in the path")
			}
		}
	}
	return strings.ToLower(p), keys, nil
}

func (p *plexPolicy) Check(method, path string, query map[string]string) error {
	deny := func(reason string) error { return &PolicyError{Service: p.service, Reason: reason} }
	method = strings.ToUpper(strings.TrimSpace(method))
	norm, pathKeys, err := normalizePlexPath(path)
	if err != nil {
		return deny(err.Error())
	}
	keys := pathKeys
	for k := range query {
		keys = append(keys, k)
	}
	for _, k := range keys {
		switch strings.ToLower(k) {
		case "x-plex-token", "_method":
			return deny(fmt.Sprintf("query parameter %q is not allowed", k))
		}
	}
	var needGroup string
	for _, r := range p.rules {
		if !r.re.MatchString(norm) {
			continue
		}
		for _, m := range r.methods {
			if m != method {
				continue
			}
			if r.group == "" || p.enabled[r.group] {
				return nil
			}
			if needGroup == "" {
				needGroup = r.group
			}
		}
	}
	if needGroup != "" {
		return deny(fmt.Sprintf("%s %s is a write that is switched off (allow_writes: %s)", method, norm, needGroup))
	}
	return deny(fmt.Sprintf("%s %s is not on the read list; deletes, trash, accounts and server settings are never allowed", method, norm))
}
