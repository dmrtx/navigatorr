package tools

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestProjectFieldsExactPromptExample(t *testing.T) {
	// Exact example from user prompt:
	// fields: id, title, movieFile.id, movieFile.size, movieFile.mediaInfo.audioLanguages, movieFile.mediaInfo.subtitles
	rawJSON := `{
		"id": 1,
		"title": "Akira",
		"year": 1988,
		"overview": "A secret military project...",
		"movieFile": {
			"id": 123,
			"size": 4053950652,
			"relativePath": "Akira.1988.mkv",
			"mediaInfo": {
				"audioLanguages": ["jpn", "eng"],
				"subtitles": ["eng", "spa"],
				"videoCodec": "x265",
				"resolution": "1080p"
			}
		}
	}`

	var input map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &input); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}

	fields := []string{
		"id",
		"title",
		"movieFile.id",
		"movieFile.size",
		"movieFile.mediaInfo.audioLanguages",
		"movieFile.mediaInfo.subtitles",
	}

	got := ProjectFields(input, fields)

	expectedJSON := `{
		"id": 1,
		"title": "Akira",
		"movieFile": {
			"id": 123,
			"size": 4053950652,
			"mediaInfo": {
				"audioLanguages": ["jpn", "eng"],
				"subtitles": ["eng", "spa"]
			}
		}
	}`
	var expected map[string]any
	if err := json.Unmarshal([]byte(expectedJSON), &expected); err != nil {
		t.Fatalf("unmarshal expected: %v", err)
	}

	// Compare JSON strings for simplicity and accuracy
	gotBytes, _ := json.Marshal(got)
	expectedBytes, _ := json.Marshal(expected)

	var gotMap, expMap map[string]any
	json.Unmarshal(gotBytes, &gotMap)
	json.Unmarshal(expectedBytes, &expMap)

	if !reflect.DeepEqual(gotMap, expMap) {
		t.Errorf("ProjectFields mismatch.\nGot:      %s\nExpected: %s", string(gotBytes), string(expectedBytes))
	}
}

func TestProjectFieldsArray(t *testing.T) {
	rawJSON := `[
		{"id": 1, "title": "A", "extra": 100},
		{"id": 2, "title": "B", "extra": 200}
	]`

	var input []any
	json.Unmarshal([]byte(rawJSON), &input)

	got := ProjectFields(input, []string{"title"})
	gotBytes, _ := json.Marshal(got)
	expected := `[{"title":"A"},{"title":"B"}]`

	if string(gotBytes) != expected {
		t.Errorf("expected %s, got %s", expected, string(gotBytes))
	}
}

func TestProjectFieldsNestedArrayDrilling(t *testing.T) {
	rawJSON := `{
		"records": [
			{"id": 10, "name": "Item 1", "secret": "x"},
			{"id": 20, "name": "Item 2", "secret": "y"}
		],
		"total": 2,
		"unwanted": "discard"
	}`

	var input map[string]any
	json.Unmarshal([]byte(rawJSON), &input)

	got := ProjectFields(input, []string{"records.id", "records.name", "total"})
	gotBytes, _ := json.Marshal(got)
	expected := `{"records":[{"id":10,"name":"Item 1"},{"id":20,"name":"Item 2"}],"total":2}`

	var gotMap, expMap map[string]any
	json.Unmarshal(gotBytes, &gotMap)
	json.Unmarshal([]byte(expected), &expMap)

	if !reflect.DeepEqual(gotMap, expMap) {
		t.Errorf("expected %s, got %s", expected, string(gotBytes))
	}
}
