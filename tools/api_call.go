package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"mime"
	"mime/multipart"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/openapi"
	"github.com/jakenesler/navigatorr/resilience"
	"github.com/jakenesler/navigatorr/snapshot"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Supported Content Types
const (
	ContentTypeJSON           = "application/json"
	ContentTypeFormURLEncoded = "application/x-www-form-urlencoded"
	ContentTypeMultipartForm  = "multipart/form-data"
)

func registerAPICallTool(s *server.MCPServer, registry *arrservice.Registry, specStore *openapi.Store, maxResponseSizeKB int, allowDestructive bool) {
	s.AddTool(
		mcp.NewTool("call_api",
			mcp.WithDescription("Make an authenticated API call to any configured *arr service. Returns the JSON response. Supports real field projections, snapshot caching, and cursor-based pagination for large collections."),
			mcp.WithString("service", mcp.Description("Service name (e.g. sonarr, radarr, bazarr). Required unless cursor is provided.")),
			mcp.WithString("method", mcp.Description("HTTP method (default: GET)")),
			mcp.WithString("path", mcp.Description("API path (e.g. /series, /movie, /system/settings). The API version prefix is added automatically. Required unless cursor is provided.")),
			mcp.WithString("query", mcp.Description("Query parameters as JSON object (e.g. {\"term\": \"breaking bad\"})")),
			mcp.WithString("body", mcp.Description("JSON request body encoded as a JSON string. Do not use together with form.")),
			mcp.WithString("content_type", mcp.Description("Request content type. Defaults to application/json when body is supplied. Supported: application/json, application/x-www-form-urlencoded, multipart/form-data.")),
			mcp.WithString("form", mcp.Description("Form fields for application/x-www-form-urlencoded (or multipart/form-data). Pass as a JSON object string or key-value map. Arrays of primitives are encoded as repeated keys. Do not use together with body.")),
			mcp.WithBoolean("include_metadata", mcp.Description("When true, returns a structured JSON envelope with status_code, request metadata, mutating flag, and response body.")),
			mcp.WithString("fields", mcp.Description("Comma-separated fields to project in response. Supports nested dot notation (e.g. \"id,title,movieFile.id,movieFile.size,movieFile.mediaInfo.audioLanguages,movieFile.mediaInfo.subtitles\").")),
			mcp.WithString("filter", mcp.Description("Filter array results. Format: \"field:op:value\". Ops: contains, eq, ne, gt, lt (e.g. \"title:contains:Pirates\", \"year:gt:2000\", \"hasFile:eq:true\")")),
			mcp.WithString("limit", mcp.Description("Max number of items to return from array responses (default: 50 for large collections)")),
			mcp.WithString("cursor", mcp.Description("Opaque cursor token from a previous call_api response to retrieve the next page from local snapshot")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleCallAPI(ctx, req, registry, specStore, maxResponseSizeKB, allowDestructive)
		},
	)
}

func handleCallAPI(ctx context.Context, req mcp.CallToolRequest, registry *arrservice.Registry, specStore *openapi.Store, maxResponseSizeKB int, allowDestructive bool) (*mcp.CallToolResult, error) {
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
	contentTypeStr := strings.TrimSpace(mcp.ParseString(req, "content_type", ""))
	includeMetadata := false
	if im, ok := req.GetArguments()["include_metadata"].(bool); ok {
		includeMetadata = im
	}

	if svcName == "" || path == "" {
		return mcp.NewToolResultError("service and path are required"), nil
	}

	svc, err := registry.Get(svcName)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Determine if body or form arguments are provided
	hasBody := bodyStr != ""
	if !hasBody {
		if raw, ok := req.GetArguments()["body"]; ok && raw != nil {
			if s, isStr := raw.(string); isStr {
				hasBody = strings.TrimSpace(s) != ""
			} else {
				hasBody = true
			}
		}
	}

	formRaw := req.GetArguments()["form"]
	hasForm := false
	if formRaw != nil {
		if s, isStr := formRaw.(string); isStr {
			hasForm = strings.TrimSpace(s) != ""
		} else if m, isMap := formRaw.(map[string]any); isMap {
			hasForm = len(m) > 0
		} else {
			hasForm = true
		}
	}

	// Validation 1: Simultaneous body and form
	if hasBody && hasForm {
		return mcp.NewToolResultError("cannot provide both body and form simultaneously; use body for JSON or form for form-urlencoded"), nil
	}

	// Validation 2: Form with GET or HEAD
	if hasForm && (method == "GET" || method == "HEAD") {
		return mcp.NewToolResultError(fmt.Sprintf("cannot use form with HTTP method %s; form fields are only supported for mutation methods (POST, PUT, PATCH)", method)), nil
	}

	// Validation 3: Normalize content_type if supplied
	normalizedCT, err := normalizeContentType(contentTypeStr)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Validation 4: Content-Type incompatibilities
	if normalizedCT == ContentTypeJSON && hasForm {
		return mcp.NewToolResultError("cannot use form with content_type application/json; use body for JSON or set content_type to application/x-www-form-urlencoded"), nil
	}
	if (normalizedCT == ContentTypeFormURLEncoded || normalizedCT == ContentTypeMultipartForm) && hasBody {
		return mcp.NewToolResultError(fmt.Sprintf("cannot use body with content_type %s; use form parameter", normalizedCT)), nil
	}

	// Determine effective content type
	effectiveCT := normalizedCT
	if effectiveCT == "" {
		if hasForm {
			effectiveCT = ContentTypeFormURLEncoded
		} else if hasBody {
			effectiveCT = ContentTypeJSON
		} else if specStore != nil {
			// Check OpenAPI spec for default content type
			if idx := specStore.GetIndex(svcName); idx != nil {
				if detail, err := idx.GetDetail(path, method); err == nil && detail != nil && detail.RequestBody != nil && detail.RequestBody.ContentType != "" {
					if specCT, err := normalizeContentType(detail.RequestBody.ContentType); err == nil && specCT != "" {
						effectiveCT = specCT
					}
				}
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
	} else if raw, ok := req.GetArguments()["query"].(map[string]any); ok && raw != nil {
		query = make(map[string]string)
		for k, v := range raw {
			query[k] = fmt.Sprintf("%v", v)
		}
	}

	// Encode payload
	var reqBodyBytes []byte
	if hasForm {
		if effectiveCT == ContentTypeMultipartForm {
			var headerCT string
			reqBodyBytes, headerCT, err = encodeMultipartFormData(formRaw)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("invalid form data: %v", err)), nil
			}
			effectiveCT = headerCT
		} else {
			effectiveCT = ContentTypeFormURLEncoded
			reqBodyBytes, err = encodeFormURLEncoded(formRaw)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("invalid form data: %v", err)), nil
			}
		}
	} else if hasBody {
		if bodyStr != "" {
			if (effectiveCT == ContentTypeJSON || effectiveCT == "") && !json.Valid([]byte(bodyStr)) {
				return mcp.NewToolResultError("invalid body JSON"), nil
			}
			reqBodyBytes = []byte(bodyStr)
		} else if raw, ok := req.GetArguments()["body"]; ok && raw != nil {
			b, err := json.Marshal(raw)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("invalid body: %v", err)), nil
			}
			reqBodyBytes = b
		}
	}

	if isDestr, reason := isDestructiveAPICall(method, path, string(reqBodyBytes)); isDestr && !allowDestructive {
		return mcp.NewToolResultError(fmt.Sprintf("destructive operations are disabled (%s). Set allow_destructive: true in config.yaml to enable.", reason)), nil
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

	respBody, statusCode, err := svc.DoRequestWithContentType(ctx, method, path, query, reqBodyBytes, effectiveCT)
	if err != nil {
		cleanErrMsg := sanitizeErrorMessage(err.Error(), svc.Config.APIKey)
		return mcp.NewToolResultError(fmt.Sprintf("request failed: %s", cleanErrMsg)), nil
	}

	if statusCode < 200 || statusCode > 299 {
		structErr := resilience.ClassifyError(svcName, statusCode, respBody)
		cleanResp := sanitizeErrorMessage(truncate(string(respBody), 2000), svc.Config.APIKey)
		return mcp.NewToolResultError(fmt.Sprintf(
			"%s %s failed: HTTP %d (category: %s, retryable: %v)\n%s",
			method, path, statusCode, structErr.Category, structErr.Retryable, cleanResp)), nil
	}

	// Parse response JSON
	var jsonResp any
	if err := json.Unmarshal(respBody, &jsonResp); err != nil {
		if includeMetadata {
			meta := map[string]any{
				"status_code": statusCode,
				"mutating":    method != "GET" && method != "HEAD",
				"request": map[string]any{
					"service":      svcName,
					"method":       method,
					"path":         path,
					"content_type": effectiveCT,
				},
				"response": strings.TrimSpace(string(respBody)),
			}
			data, _ := json.MarshalIndent(meta, "", "  ")
			return mcp.NewToolResultText(string(data)), nil
		}
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

	if includeMetadata {
		meta := map[string]any{
			"status_code": statusCode,
			"mutating":    method != "GET" && method != "HEAD",
			"request": map[string]any{
				"service":      svcName,
				"method":       method,
				"path":         path,
				"content_type": effectiveCT,
			},
			"response": jsonResp,
		}
		data, _ := json.MarshalIndent(meta, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
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
		} else if vals, err := url.ParseQuery(bodyStr); err == nil {
			name := vals.Get("name")
			normName := strings.ToLower(strings.TrimSpace(name))
			if normName == "cleanuprecyclebin" || strings.HasPrefix(normName, "delete") || strings.HasPrefix(normName, "purge") {
				return true, fmt.Sprintf("destructive command %q", name)
			}
		}
	}

	return false, ""
}

// normalizeContentType maps aliases and handles media type parameters.
func normalizeContentType(ct string) (string, error) {
	ct = strings.TrimSpace(ct)
	if ct == "" {
		return "", nil
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		mediaType = strings.ToLower(ct)
	}
	switch mediaType {
	case "application/json", "json":
		return ContentTypeJSON, nil
	case "application/x-www-form-urlencoded", "form", "form-urlencoded", "application/x-form-urlencoded":
		return ContentTypeFormURLEncoded, nil
	case "multipart/form-data", "multipart":
		return ContentTypeMultipartForm, nil
	default:
		return "", fmt.Errorf("unsupported content_type %q (supported: %s, %s, %s)", ct, ContentTypeJSON, ContentTypeFormURLEncoded, ContentTypeMultipartForm)
	}
}

// encodeFormURLEncoded encodes form data into application/x-www-form-urlencoded format.
// Supports primitives, lists/arrays as repeated keys, Unicode, special characters,
// and complex nested structures serialized to JSON.
func encodeFormURLEncoded(formRaw any) ([]byte, error) {
	if formRaw == nil {
		return nil, nil
	}

	var m map[string]any

	switch v := formRaw.(type) {
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return nil, nil
		}
		// Try parsing as JSON object first
		if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
			// Fallback: check if already a query string like foo=bar&baz=1
			parsed, parseErr := url.ParseQuery(trimmed)
			if parseErr == nil && len(parsed) > 0 {
				return []byte(parsed.Encode()), nil
			}
			return nil, fmt.Errorf("invalid form data string: %v", err)
		}
	default:
		// Always JSON round-trip to normalize nested maps/slices into standard JSON types ([]any, map[string]any)
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("invalid form object: %v", err)
		}
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("form must be an object/map: %v", err)
		}
	}

	vals := make(url.Values)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		val := m[k]
		switch typed := val.(type) {
		case nil:
			vals.Add(k, "")
		case []any:
			// Check if slice contains only primitives (string, number, boolean)
			allPrimitives := true
			for _, item := range typed {
				if item == nil {
					continue
				}
				switch item.(type) {
				case string, bool, int, int64, float64, json.Number:
					// primitive
				default:
					allPrimitives = false
				}
			}
			if allPrimitives {
				for _, item := range typed {
					vals.Add(k, formatFormPrimitive(item))
				}
			} else {
				// Complex structures (e.g. array of objects like Bazarr's languages-profiles)
				// serialize to a JSON string for the single form field.
				jsonBytes, err := json.Marshal(typed)
				if err != nil {
					return nil, fmt.Errorf("serializing form field %q: %w", k, err)
				}
				vals.Add(k, string(jsonBytes))
			}
		case map[string]any:
			jsonBytes, err := json.Marshal(typed)
			if err != nil {
				return nil, fmt.Errorf("serializing form field %q: %w", k, err)
			}
			vals.Add(k, string(jsonBytes))
		default:
			vals.Add(k, formatFormPrimitive(typed))
		}
	}

	return []byte(vals.Encode()), nil
}

func formatFormPrimitive(v any) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case bool:
		return strconv.FormatBool(val)
	case int:
		return strconv.Itoa(val)
	case int64:
		return strconv.FormatInt(val, 10)
	case float64:
		if val == math.Trunc(val) && !math.IsNaN(val) && !math.IsInf(val, 0) {
			return strconv.FormatInt(int64(val), 10)
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	case json.Number:
		return val.String()
	default:
		return fmt.Sprintf("%v", val)
	}
}

// encodeMultipartFormData encodes form fields into multipart/form-data.
func encodeMultipartFormData(formRaw any) ([]byte, string, error) {
	if formRaw == nil {
		return nil, "", nil
	}

	var m map[string]any
	switch v := formRaw.(type) {
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return nil, "", nil
		}
		if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
			return nil, "", fmt.Errorf("invalid form data string: %v", err)
		}
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, "", fmt.Errorf("invalid form object: %v", err)
		}
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, "", fmt.Errorf("form must be an object/map: %v", err)
		}
	}

	var b bytes.Buffer
	w := multipart.NewWriter(&b)

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		val := m[k]
		switch typed := val.(type) {
		case nil:
			_ = w.WriteField(k, "")
		case []any:
			allPrimitives := true
			for _, item := range typed {
				if item == nil {
					continue
				}
				switch item.(type) {
				case string, bool, int, int64, float64, json.Number:
				default:
					allPrimitives = false
				}
			}
			if allPrimitives {
				for _, item := range typed {
					_ = w.WriteField(k, formatFormPrimitive(item))
				}
			} else {
				jsonBytes, err := json.Marshal(typed)
				if err != nil {
					return nil, "", fmt.Errorf("serializing form field %q: %w", k, err)
				}
				_ = w.WriteField(k, string(jsonBytes))
			}
		case map[string]any:
			jsonBytes, err := json.Marshal(typed)
			if err != nil {
				return nil, "", fmt.Errorf("serializing form field %q: %w", k, err)
			}
			_ = w.WriteField(k, string(jsonBytes))
		default:
			_ = w.WriteField(k, formatFormPrimitive(typed))
		}
	}

	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("closing multipart writer: %w", err)
	}

	return b.Bytes(), w.FormDataContentType(), nil
}

// sanitizeErrorMessage redacts sensitive credentials from error messages.
func sanitizeErrorMessage(msg string, secrets ...string) string {
	clean := msg
	for _, sec := range secrets {
		if sec != "" {
			clean = strings.ReplaceAll(clean, sec, "***REDACTED***")
		}
	}
	return redactURLSecretsInText(clean)
}

func redactURLSecretsInText(s string) string {
	sensitiveParams := []string{"apikey", "api_key", "token", "password", "auth", "secret"}
	for _, p := range sensitiveParams {
		re := regexp.MustCompile(`(?i)(` + p + `=)([^&\s\n\r"']+)`)
		s = re.ReplaceAllString(s, `${1}***REDACTED***`)
	}
	return s
}
