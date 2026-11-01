package config

import (
	"fmt"
	"sort"
	"strings"
)

// PolicyPlex is the built-in request policy for Plex Media Server.
const PolicyPlex = "plex"

// Plex write groups a config may switch on with allow_writes. Nothing outside
// these exists: deleting libraries or media, emptying the trash, account and
// server-preference routes are never reachable through a policy.
const (
	PlexWriteRefresh     = "refresh"       // scan a library, refresh one item's metadata
	PlexWriteLibraryNew  = "library_new"   // create a library
	PlexWriteLibraryEdit = "library_edit"  // change a library's agent, scanner, language or name
	PlexWriteMatch       = "match"         // match or unmatch one item
	PlexWriteMetaEdit    = "metadata_edit" // edit one item's fields
)

// PlexWriteGroups lists the valid allow_writes values for the plex policy.
var PlexWriteGroups = []string{
	PlexWriteRefresh, PlexWriteLibraryNew, PlexWriteLibraryEdit, PlexWriteMatch, PlexWriteMetaEdit,
}

// ValidateServicePolicy rejects an unknown policy or write group so a typo
// fails at load time instead of silently leaving writes off or on.
func ValidateServicePolicy(name string, svc ServiceConfig) error {
	switch svc.Policy {
	case "":
		if len(svc.AllowWrites) > 0 {
			return fmt.Errorf("service %q: allow_writes needs a policy (policy: %s)", name, PolicyPlex)
		}
		return nil
	case PolicyPlex:
		valid := map[string]bool{}
		for _, g := range PlexWriteGroups {
			valid[g] = true
		}
		for _, g := range svc.AllowWrites {
			if !valid[g] {
				sorted := append([]string(nil), PlexWriteGroups...)
				sort.Strings(sorted)
				return fmt.Errorf("service %q: unknown allow_writes group %q (valid: %s)", name, g, strings.Join(sorted, ", "))
			}
		}
		return nil
	default:
		return fmt.Errorf("service %q: unknown policy %q (valid: %s)", name, svc.Policy, PolicyPlex)
	}
}
