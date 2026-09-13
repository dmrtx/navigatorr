package action

import (
	"encoding/json"
	"strings"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
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
	if streams, ok := raw.([]mediainspect.DetailedStream); ok {
		return streams
	}
	var b []byte
	switch v := raw.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		var err error
		b, err = json.Marshal(v)
		if err != nil {
			return nil
		}
	}
	var streams []mediainspect.DetailedStream
	if json.Unmarshal(b, &streams) != nil {
		return nil
	}
	return streams
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

func getSourceReport(v any) *mediainspect.DetailedReport {
	if r, ok := v.(*mediainspect.DetailedReport); ok {
		return r
	}
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var r mediainspect.DetailedReport
	if json.Unmarshal(b, &r) != nil {
		return nil
	}
	return &r
}

func getOptimizationPolicy(v any) *recipe.OptimizationPolicy {
	if p, ok := v.(*recipe.OptimizationPolicy); ok {
		return p
	}
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var p recipe.OptimizationPolicy
	if json.Unmarshal(b, &p) != nil {
		return nil
	}
	return &p
}

func isSourceHDRorDV(rep *mediainspect.DetailedReport) bool {
	if rep == nil {
		return false
	}
	if rep.HDR != nil && rep.HDR.Present {
		return true
	}
	for _, vs := range rep.Video {
		codec := strings.ToLower(strings.TrimSpace(vs.Codec))
		prof := strings.ToLower(strings.TrimSpace(vs.Profile))
		if strings.Contains(codec, "dovi") || strings.Contains(codec, "dvh1") ||
			strings.Contains(codec, "dvhe") || strings.Contains(codec, "dva1") ||
			strings.Contains(codec, "dav1") || strings.Contains(prof, "dolby vision") ||
			strings.Contains(prof, "dovi") || strings.HasPrefix(prof, "dv") {
			return true
		}
		ct := strings.ToLower(strings.TrimSpace(vs.ColorTransfer))
		cp := strings.ToLower(strings.TrimSpace(vs.ColorPrimaries))
		cs := strings.ToLower(strings.TrimSpace(vs.ColorSpace))
		if ct == "smpte2084" || ct == "arib-std-b67" || strings.Contains(ct, "2084") || strings.Contains(ct, "hlg") || strings.Contains(ct, "pq") {
			return true
		}
		if cp == "bt2020" || strings.Contains(cp, "2020") || cp == "dci-p3" {
			return true
		}
		if cs == "bt2020nc" || cs == "bt2020c" || strings.Contains(cs, "2020") {
			return true
		}
		if vs.MasteringDisplay != nil || vs.ContentLightLevel != nil {
			return true
		}
	}
	return false
}
