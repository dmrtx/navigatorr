package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/resilience"
	"github.com/jakenesler/navigatorr/snapshot"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerAPICallTool(s *server.MCPServer, registry *arrservice.Registry, maxResponseSizeKB int, allowDestructive bool) {
	s.AddTool(
		mcp.NewTool("call_api",
			mcp.WithDescription("Make an authenticated API call to any configured *arr service. Returns the JSON response. Supports real field projections, snapshot caching, and cursor-based pagination for large collections."),
			mcp.WithString("service", mcp.Description("Service name (e.g. sonarr, radarr). Required unless cursor is provided.")),
			mcp.WithString("method", mcp.Description("HTTP method (default: GET)")),
			mcp.WithString("path", mcp.Description("API path (e.g. /series, /movie). The API version prefix is added automatically. Required unless cursor is provided.")),
			mcp.WithString("query", mcp.Description("Query parameters as JSON object (e.g. {\"term\": \"breaking bad\"})")),
			mcp.WithString("body", mcp.Description("Request body as JSON string")),
			mcp.WithString("fields", mcp.Description("Comma-separated fields to project in response. Supports nested dot notation (e.g. \"id,title,movieFile.id,movieFile.size,movieFile.mediaInfo.audioLanguages,movieFile.mediaInfo.subtitles\").")),
			mcp.WithString("filter", mcp.Description("Filter array results. Format: \"field:op:value\". Ops: contains, eq, ne, gt, lt (e.g. \"title:contains:Pirates\", \"year:gt:2000\", \"hasFile:eq:true\")")),
			mcp.WithString("limit", mcp.Description("Max number of items to return from array responses (default: 50 for large collections)")),
			mcp.WithString("cursor", mcp.Description("Opaque cursor token from a previous call_api response to retrieve the next page from local snapshot")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleCallAPI(ctx, req, registry, maxResponseSizeKB, allowDestructive)
		},
	)
}

func handleCallAPI(ctx context.Context, req mcp.CallToolRequest, registry *arrservice.Registry, maxResponseSizeKB int, allowDestructive bool) (*mcp.CallToolResult, error) {
	cursorStr := strings.TrimSpace(mcp.ParseString(req, "cursor", ""))
	limitStr := strings.TrimSpace(mcp.ParseString(req, "limit", ""))
	fieldsStr := strings.TrimSpace(mcp.ParseString(req, "fields", ""))
	filterStr := strings.TrimSpace(mcp.ParseString(req, "filter", ""))

	// If cursor is provided, retrieve from local snapshot without hitting upstream
	if cursorStr != "" {
		if registry == nil || registry.Snapshots == nil {
			return mcp.NewToolResultError("snapshot store is not available"), nil
		}
		limit := 50
		if limitStr != "" {
			if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
				limit = l
			}
		}
		items, nextCursor, complete, total, offset, err := registry.Snapshots.GetPage(cursorStr, limit)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("cursor error: %v", err)), nil
		}
		if filterStr != "" {
			items = applyFilter(items, filterStr)
		}
		if fieldsStr != "" {
			fields := parseFields(fieldsStr)
			items = ProjectFields(items, fields).([]any)
		}
		respMap := map[string]any{
			"items":    items,
			"complete": complete,
			"total":    total,
			"offset":   offset,
		}
		if nextCursor != "" {
			respMap["next_cursor"] = nextCursor
		}
		data, _ := json.MarshalIndent(respMap, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	}

	svcName := mcp.ParseString(req, "service", "")
	method := strings.ToUpper(strings.TrimSpace(mcp.ParseString(req, "method", "GET")))
	path := mcp.ParseString(req, "path", "")
	queryStr := mcp.ParseString(req, "query", "")
	bodyStr := mcp.ParseString(req, "body", "")

	if svcName == "" || path == "" {
		return mcp.NewToolResultError("service and path are required"), nil
	}

	if isDestr, reason := isDestructiveAPICall(method, path, bodyStr); isDestr && !allowDestructive {
		return mcp.NewToolResultError(fmt.Sprintf("destructive operations are disabled (%s). Set allow_destructive: true in config.yaml to enable.", reason)), nil
	}

	svc, err := registry.Get(svcName)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Check for cached GET snapshot to avoid hammering upstream and SQLite database locks
	if method == "GET" && registry.Snapshots != nil {
		if snap, ok := registry.Snapshots.Find(svcName, path, queryStr); ok {
			items := snap.Items
			if filterStr != "" {
				items = applyFilter(items, filterStr)
			}
			if fieldsStr != "" {
				items = ProjectFields(items, parseFields(fieldsStr)).([]any)
			}
			limit := 50
			if limitStr != "" {
				if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
					limit = l
				}
			}
			if limitStr != "" || len(items) > 50 {
				firstPage := items
				complete := true
				nextCursor := ""
				if len(items) > limit {
					firstPage = items[:limit]
					complete = false
					nextCursor = snapshot.EncodeCursor(snap.ID, limit)
				}
				respMap := map[string]any{
					"items":    firstPage,
					"complete": complete,
					"total":    snap.Total,
					"offset":   0,
				}
				if nextCursor != "" {
					respMap["next_cursor"] = nextCursor
				}
				data, _ := json.MarshalIndent(respMap, "", "  ")
				return mcp.NewToolResultText(string(data)), nil
			}
		}
	}

	// Parse query params
	var query map[string]string
	if queryStr != "" {
		var raw map[string]any
		if err := json.Unmarshal([]byte(queryStr), &raw); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid query JSON: %v", err)), nil
		}
		query = make(map[string]string)
		for k, v := range raw {
			query[k] = fmt.Sprintf("%v", v)
		}
	}

	// Parse body
	var body []byte
	if bodyStr != "" {
		if !json.Valid([]byte(bodyStr)) {
			return mcp.NewToolResultError("invalid body JSON"), nil
		}
		body = []byte(bodyStr)
	} else if raw, ok := req.GetArguments()["body"]; ok && raw != nil {
		b, err := json.Marshal(raw)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid body: %v", err)), nil
		}
		body = b
	}

	respBody, statusCode, err := svc.DoRequest(ctx, method, path, query, body)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("request failed: %v", err)), nil
	}

	if statusCode < 200 || statusCode > 299 {
		structErr := resilience.ClassifyError(svcName, statusCode, respBody)
		return mcp.NewToolResultError(fmt.Sprintf(
			"%s %s failed: HTTP %d (category: %s, retryable: %v)\n%s",
			method, path, statusCode, structErr.Category, structErr.Retryable, truncate(string(respBody), 2000))), nil
	}

	// Parse response JSON
	var jsonResp any
	if err := json.Unmarshal(respBody, &jsonResp); err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("status: %d\n%s", statusCode, string(respBody))), nil
	}

	// If response is an array from a GET request, create a snapshot
	if arr, ok := jsonResp.([]any); ok && method == "GET" && registry != nil && registry.Snapshots != nil {
		snap := registry.Snapshots.Create(svcName, path, queryStr, arr)
		if filterStr != "" {
			arr = applyFilter(arr, filterStr)
		}
		if fieldsStr != "" {
			arr = ProjectFields(arr, parseFields(fieldsStr)).([]any)
		}
		limit := 50
		if limitStr != "" {
			if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
				limit = l
			}
		}
		// Return cursor envelope if requested or if collection is large
		if limitStr != "" || len(arr) > 50 {
			firstPage := arr
			complete := true
			nextCursor := ""
			if len(arr) > limit {
				firstPage = arr[:limit]
				complete = false
				nextCursor = snapshot.EncodeCursor(snap.ID, limit)
			}
			respMap := map[string]any{
				"items":    firstPage,
				"complete": complete,
				"total":    snap.Total,
				"offset":   0,
			}
			if nextCursor != "" {
				respMap["next_cursor"] = nextCursor
			}
			data, _ := json.MarshalIndent(respMap, "", "  ")
			return mcp.NewToolResultText(string(data)), nil
		}
		jsonResp = arr
	} else {
		// Object response or non-GET
		if fieldsStr != "" {
			jsonResp = ProjectFields(jsonResp, parseFields(fieldsStr))
		}
		if filterStr != "" || limitStr != "" {
			jsonResp = processResponse(jsonResp, "", filterStr, limitStr)
		}
	}

	maxResponseBytes := maxResponseSizeKB * 1024
	data, _ := json.MarshalIndent(jsonResp, "", "  ")

	if len(data) > maxResponseBytes {
		arr, fieldPath := findLargestArray(jsonResp)
		if len(arr) > 0 {
			var availableFields []string
			if obj, ok := arr[0].(map[string]any); ok {
				for k := range obj {
					availableFields = append(availableFields, k)
				}
			}

			sizeKB := len(data) / 1024
			hint := fmt.Sprintf("⚠️ Response too large (%dKB, %d items", sizeKB, len(arr))
			if fieldPath != "" {
				hint += fmt.Sprintf(" in \"%s\"", fieldPath)
			}
			hint += "). This would consume excessive tokens.\n\n"
			hint += "Retry this call with the fields param to select only the fields you need.\n"
			if len(availableFields) > 0 {
				prefix := ""
				if fieldPath != "" {
					prefix = fieldPath + "."
				}
				hint += fmt.Sprintf("Available fields: %s\n", joinWithPrefix(availableFields, prefix))
				hint += fmt.Sprintf("\nExample: fields: \"%sid,%stitle,%sstatus\"\n", prefix, prefix, prefix)
			}
			hint += "\nYou can also use filter, limit, and cursor params."
			hint += "\nDo NOT retry this call without fields, filter, or limit."

			return mcp.NewToolResultText(hint), nil
		}

		return mcp.NewToolResultText(fmt.Sprintf(
			"⚠️ Response too large (%dKB). Use fields param to reduce response size.",
			len(data)/1024)), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}

// processResponse applies filter, fields, and limit to the API response.
// Handles both top-level arrays and object responses with nested arrays
// (e.g. {records: [...], page: 1, totalRecords: 50}).
// Fields like "records.title" will drill into the "records" array and
// pick "title" from each item.
func processResponse(resp any, fieldsStr, filterStr, limitStr string) any {
	// Top-level array — apply directly
	if arr, ok := resp.([]any); ok {
		return processArray(arr, fieldsStr, filterStr, limitStr)
	}

	// Object response — check for nested array field selection
	obj, isObj := resp.(map[string]any)
	if !isObj {
		return resp
	}

	if fieldsStr != "" {
		fields := parseFields(fieldsStr)

		// Group fields by their top-level key to detect array drilling
		// e.g. "records.title,records.year,page" → {records: [title, year], page: []}
		grouped := make(map[string][]string)
		nested := make(map[string][]string)
		var topFields []string
		for _, f := range fields {
			parts := strings.SplitN(f, ".", 2)
			key := parts[0]
			if len(parts) == 2 {
				// Check if this key holds an array — if so, treat as array field selection
				if _, ok := obj[key].([]any); ok {
					grouped[key] = append(grouped[key], parts[1])
					continue
				}
				// The array can sit deeper, as in {history: {slots: [...]}}. Recurse
				// so the rest of the path is resolved against the sub-object.
				if _, ok := obj[key].(map[string]any); ok && strings.Contains(parts[1], ".") {
					nested[key] = append(nested[key], parts[1])
					continue
				}
			}
			topFields = append(topFields, f)
		}

		// Process array fields with sub-selection
		if len(grouped) > 0 || len(nested) > 0 {
			result := make(map[string]any)
			// Keep any requested top-level scalar fields
			if len(topFields) > 0 {
				picked := pickFields(obj, topFields)
				for k, v := range picked {
					result[k] = v
				}
			}
			// Drill into arrays
			for key, subFields := range grouped {
				arr, ok := obj[key].([]any)
				if !ok {
					continue
				}
				subFieldsStr := strings.Join(subFields, ",")
				result[key] = processArray(arr, subFieldsStr, filterStr, limitStr)
			}
			// Drill into sub-objects
			for key, subPaths := range nested {
				sub, ok := obj[key].(map[string]any)
				if !ok {
					continue
				}
				result[key] = processResponse(sub, strings.Join(subPaths, ","), filterStr, limitStr)
			}
			return result
		}

		// No array drilling — just pick top-level fields
		return pickFields(obj, fields)
	}

	// No fields but filter/limit — find and process nested arrays
	if filterStr != "" || limitStr != "" {
		for k, v := range obj {
			if arr, ok := v.([]any); ok {
				obj[k] = processArray(arr, "", filterStr, limitStr)
			}
		}
	}

	return obj
}

// processArray applies filter, limit, and field selection to an array.
func processArray(arr []any, fieldsStr, filterStr, limitStr string) any {
	if filterStr != "" {
		arr = applyFilter(arr, filterStr)
	}

	if limitStr != "" {
		if limit, err := strconv.Atoi(limitStr); err == nil && limit > 0 && limit < len(arr) {
			arr = arr[:limit]
		}
	}

	if fieldsStr != "" {
		fields := parseFields(fieldsStr)
		result := make([]any, len(arr))
		for i, item := range arr {
			if obj, ok := item.(map[string]any); ok {
				result[i] = pickFields(obj, fields)
			} else {
				result[i] = item
			}
		}
		return result
	}

	return arr
}

// findLargestArray finds the largest []any in a response, walking nested objects
// so envelopes like {history: {slots: [...]}} are found as well as {records: [...]}.
// Returns the array and its dotted field path (empty for a top-level array).
func findLargestArray(resp any) ([]any, string) {
	if arr, ok := resp.([]any); ok {
		return arr, ""
	}
	obj, ok := resp.(map[string]any)
	if !ok {
		return nil, ""
	}
	var largest []any
	var largestPath string
	for k, v := range obj {
		arr, path := findLargestArray(v)
		if len(arr) <= len(largest) {
			continue
		}
		largest = arr
		largestPath = k
		if path != "" {
			largestPath = k + "." + path
		}
	}
	return largest, largestPath
}

// joinWithPrefix joins field names with a prefix for display.
func joinWithPrefix(fields []string, prefix string) string {
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = prefix + f
	}
	return strings.Join(parts, ", ")
}

// parseFields splits a comma-separated fields string.
func parseFields(s string) []string {
	parts := strings.Split(s, ",")
	fields := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			fields = append(fields, p)
		}
	}
	return fields
}

// pickFields extracts only the specified fields from an object.
// Supports dot notation for nested fields (e.g. "statistics.sizeOnDisk").
func pickFields(obj map[string]any, fields []string) map[string]any {
	result := make(map[string]any)
	for _, f := range fields {
		parts := strings.SplitN(f, ".", 2)
		key := parts[0]
		val, ok := obj[key]
		if !ok {
			continue
		}
		if len(parts) == 1 {
			result[key] = val
		} else {
			// Nested field
			if nested, ok := val.(map[string]any); ok {
				if existing, ok := result[key].(map[string]any); ok {
					// Merge into existing picked nested object
					for k, v := range pickFields(nested, []string{parts[1]}) {
						existing[k] = v
					}
				} else {
					result[key] = pickFields(nested, []string{parts[1]})
				}
			}
		}
	}
	return result
}

// applyFilter filters array items. Format: "field:op:value"
// Ops: contains, eq, ne, gt, lt
func applyFilter(arr []any, filterStr string) []any {
	parts := strings.SplitN(filterStr, ":", 3)
	if len(parts) != 3 {
		return arr
	}
	field, op, value := parts[0], parts[1], parts[2]

	// Non-nil so a filter that matches nothing marshals as [] rather than null.
	result := make([]any, 0, len(arr))
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fieldVal := getNestedField(obj, field)
		if fieldVal == nil {
			// An absent field is "not equal" to any value; every other op
			// needs something to compare against.
			if op == "ne" {
				result = append(result, item)
			}
			continue
		}
		if matchFilter(fieldVal, op, value) {
			result = append(result, item)
		}
	}
	return result
}

// truncate caps a string for inclusion in a tool result, so a large error
// body cannot eat the LLM's context window.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n... (truncated, %d bytes total)", len(s))
}

// getNestedField retrieves a value using dot notation.
func getNestedField(obj map[string]any, field string) any {
	parts := strings.SplitN(field, ".", 2)
	val, ok := obj[parts[0]]
	if !ok {
		return nil
	}
	if len(parts) == 1 {
		return val
	}
	if nested, ok := val.(map[string]any); ok {
		return getNestedField(nested, parts[1])
	}
	return nil
}

// matchFilter checks if a value matches the filter operation.
func matchFilter(fieldVal any, op, value string) bool {
	fieldStr := fmt.Sprintf("%v", fieldVal)

	switch op {
	case "contains":
		return strings.Contains(strings.ToLower(fieldStr), strings.ToLower(value))
	case "eq":
		return strings.EqualFold(fieldStr, value)
	case "ne":
		return !strings.EqualFold(fieldStr, value)
	case "gt":
		fv, err1 := strconv.ParseFloat(fieldStr, 64)
		cv, err2 := strconv.ParseFloat(value, 64)
		return err1 == nil && err2 == nil && fv > cv
	case "lt":
		fv, err1 := strconv.ParseFloat(fieldStr, 64)
		cv, err2 := strconv.ParseFloat(value, 64)
		return err1 == nil && err2 == nil && fv < cv
	}
	return false
}

// isDestructiveAPICall checks whether an API request is destructive regardless of the HTTP method.
// It detects:
// 1. Any HTTP DELETE method.
// 2. Any path explicitly targeting deletion or purging (e.g. /delete, /purge).
// 3. Destructive command executions in RPC endpoints (e.g. POST /command with CleanUpRecycleBin, DeleteLogFiles, or Purge...).
func isDestructiveAPICall(method, path, bodyStr string) (bool, string) {
	if method == "DELETE" {
		return true, "HTTP DELETE method"
	}

	normPath := strings.ToLower(strings.TrimSpace(path))

	// 1. Endpoints with delete or purge in the path
	if strings.HasSuffix(normPath, "/delete") || strings.Contains(normPath, "/delete/") ||
		strings.HasSuffix(normPath, "/purge") || strings.Contains(normPath, "/purge/") {
		return true, fmt.Sprintf("destructive path %q", path)
	}

	// 2. Commands that execute destructive operations (e.g. POST /command in *arr services)
	if (strings.HasSuffix(normPath, "/command") || strings.Contains(normPath, "/command/")) && bodyStr != "" {
		var cmdMap map[string]any
		if err := json.Unmarshal([]byte(bodyStr), &cmdMap); err == nil {
			if name, ok := cmdMap["name"].(string); ok {
				normName := strings.ToLower(strings.TrimSpace(name))
				if normName == "cleanuprecyclebin" || strings.HasPrefix(normName, "delete") || strings.HasPrefix(normName, "purge") {
					return true, fmt.Sprintf("destructive command %q", name)
				}
			}
		}
	}

	return false, ""
}
