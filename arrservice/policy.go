package arrservice

import (
	"fmt"
	"net/url"
	"strings"
)

// Plex's token is an admin token that cannot be scoped. A service called plex
// may read and write like any other, but never delete, and never reach the
// routes that hand out the account token. Everything else passes.
var plexBlockedRoots = map[string]bool{"myplex": true, "security": true, "accounts": true}

// PolicyError is returned when a request is refused by a service's policy.
type PolicyError struct {
	Service string
	Reason  string
}

func (e *PolicyError) Error() string {
	return fmt.Sprintf("%s: request blocked: %s", e.Service, e.Reason)
}

type plexPolicy struct{ service string }

func (p plexPolicy) Check(method, path string, query map[string]string) error {
	deny := func(reason string) error { return &PolicyError{Service: p.service, Reason: reason} }
	if strings.ToUpper(strings.TrimSpace(method)) == "DELETE" {
		return deny("DELETE is not allowed on Plex")
	}
	// A deny-list only holds if the path cannot be spelled another way, so
	// refuse anything Plex might read differently from this check.
	route := path
	keys := []string{}
	if i := strings.IndexByte(route, '?'); i >= 0 {
		q, err := url.ParseQuery(route[i+1:])
		if err != nil {
			return deny("unreadable query string in path")
		}
		for k := range q {
			keys = append(keys, k)
		}
		route = route[:i]
	}
	for k := range query {
		keys = append(keys, k)
	}
	for _, k := range keys {
		if k = strings.ToLower(k); k == "x-plex-token" || k == "_method" {
			return deny(fmt.Sprintf("query parameter %q is not allowed", k))
		}
	}
	if strings.ContainsAny(route, "%\\#") {
		return deny("encoded or special characters are not allowed in the path")
	}
	for _, r := range route {
		if r <= 0x20 || r == 0x7f {
			return deny("spaces and control characters are not allowed in the path")
		}
	}
	segs := strings.Split(strings.Trim(route, "/"), "/")
	for _, seg := range segs {
		if seg == "." || seg == ".." || (seg == "" && len(segs) > 1) {
			return deny("empty or dot segments are not allowed in the path")
		}
	}
	if plexBlockedRoots[strings.ToLower(segs[0])] {
		return deny(fmt.Sprintf("/%s hands out the account token", strings.ToLower(segs[0])))
	}
	return nil
}
