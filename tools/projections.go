package tools

import (
	"strings"
)

// ProjectFields extracts the requested fields from an object or array.
// Supports arbitrary nesting with dot notation (e.g. "movieFile.mediaInfo.audioLanguages")
// and array drilling (e.g. "records.id,records.title").
func ProjectFields(data any, fields []string) any {
	if len(fields) == 0 {
		return data
	}

	cleanedFields := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f != "" {
			cleanedFields = append(cleanedFields, f)
		}
	}
	if len(cleanedFields) == 0 {
		return data
	}

	return projectRecursive(data, cleanedFields)
}

func projectRecursive(data any, fields []string) any {
	switch v := data.(type) {
	case []any:
		res := make([]any, len(v))
		for i, item := range v {
			res[i] = projectRecursive(item, fields)
		}
		return res

	case map[string]any:
		return projectObject(v, fields)

	default:
		return data
	}
}

func projectObject(obj map[string]any, fields []string) map[string]any {
	result := make(map[string]any)

	// Group fields by their first path segment
	grouped := make(map[string][]string)
	for _, f := range fields {
		parts := strings.SplitN(f, ".", 2)
		head := parts[0]
		if len(parts) == 1 {
			grouped[head] = append(grouped[head], "")
		} else {
			grouped[head] = append(grouped[head], parts[1])
		}
	}

	for head, subs := range grouped {
		val, exists := obj[head]
		if !exists {
			continue
		}

		// Check if any subfield is empty (meaning the entire property was requested)
		allRequested := false
		var nestedSubs []string
		for _, s := range subs {
			if s == "" {
				allRequested = true
			} else {
				nestedSubs = append(nestedSubs, s)
			}
		}

		if allRequested || len(nestedSubs) == 0 {
			result[head] = val
			continue
		}

		// Otherwise recursively project the sub-property
		result[head] = projectRecursive(val, nestedSubs)
	}

	return result
}
