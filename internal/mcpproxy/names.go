package mcpproxy

import (
	"fmt"
	"sort"
)

// proxyTools are the stateful connector tools the gateway forwards to
// Transcode. The names are exact (no prefixes): a tool the gateway also
// registers itself must never be listed here, and New refuses to start when
// the two sets overlap. recipe_* is left out on purpose, it is not part of
// the connector's reviewed catalog.
var proxyTools = []string{
	"action_cancel", "action_catalog", "action_detail", "action_history",
	"action_list", "action_resume", "action_retry", "action_run", "action_status",
	"block_list", "block_release",
	"cleanup_download_leftovers", "cleanup_imported_downloads",
	"decision_list", "decision_record", "diagnostics",
	"fs_copy", "fs_hash", "fs_list", "fs_mkdir", "fs_remove",
	"fs_safe_delete", "fs_safe_move", "fs_stat",
	"get_context", "inspect_media",
	"maintenance_add", "maintenance_claim", "maintenance_get", "maintenance_list",
	"maintenance_next", "maintenance_release", "maintenance_reopen",
	"maintenance_resolve", "maintenance_update",
	"memory_delete", "memory_get", "memory_list", "memory_search", "memory_set",
	"qbit_list_files",
	"queue_claim", "queue_list", "queue_release", "queue_resolve",
	"rank_releases", "safe_replace", "scan_dangerous_files", "scan_library_issues",
}

// ProxyTools returns a copy of the fixed list of proxied tool names, sorted.
func ProxyTools() []string {
	out := append([]string(nil), proxyTools...)
	sort.Strings(out)
	return out
}

// CheckDisjoint fails when one of the gateway's own tool names is also in the
// proxy list, naming every collision.
func CheckDisjoint(own []string) error {
	proxied := make(map[string]struct{}, len(proxyTools))
	for _, n := range proxyTools {
		proxied[n] = struct{}{}
	}
	var clash []string
	for _, n := range own {
		if _, ok := proxied[n]; ok {
			clash = append(clash, n)
		}
	}
	if len(clash) > 0 {
		sort.Strings(clash)
		return fmt.Errorf("tools registered by the gateway are also in the proxy list: %v", clash)
	}
	return nil
}
