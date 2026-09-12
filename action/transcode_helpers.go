package action

import (
	"encoding/json"
	"strings"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

func getPlan(v any) *transcode.Plan {
	if p, ok := v.(*transcode.Plan); ok {
		return p
	}
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var p transcode.Plan
	if json.Unmarshal(b, &p) != nil {
		return nil
	}
	return &p
}

func getFloat(m map[string]any, key string) float64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}

func getStreamsList(m map[string]any, key string) []mediainspect.DetailedStream {
	if m == nil {
		return nil
	}
	raw, ok := m[key]
	if !ok || raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case []mediainspect.DetailedStream:
		return v
	case []any:
		res := make([]mediainspect.DetailedStream, 0, len(v))
		for _, item := range v {
			if ds, ok := item.(mediainspect.DetailedStream); ok {
				res = append(res, ds)
				continue
			}
			if im, ok := item.(map[string]any); ok {
				ds := mediainspect.DetailedStream{Index: getInt(im, "index"), Kind: getString(im, "kind"), Codec: getString(im, "codec"), Language: getString(im, "language"), Title: getString(im, "title"), Channels: getInt(im, "channels")}
				if d, ok := im["disposition"].(map[string]int); ok {
					ds.Disposition = d
				} else if da, ok := im["disposition"].(map[string]any); ok {
					ds.Disposition = map[string]int{}
					for k := range da {
						ds.Disposition[k] = getInt(da, k)
					}
				}
				res = append(res, ds)
			}
		}
		return res
	}
	return nil
}

func getInt(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

func getBool(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	if b, ok := m[key].(bool); ok {
		return b
	}
	if s, ok := m[key].(string); ok {
		s = strings.ToLower(strings.TrimSpace(s))
		return s == "true" || s == "1" || s == "yes"
	}
	return false
}
