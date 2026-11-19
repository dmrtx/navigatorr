package nodes

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.json
var specJSON []byte

// Spec is the OpenAPI 3 document of /nodes/v1.
func Spec() []byte { return specJSON }

// SpecHandler serves the document at SpecPath. It is public by design.
func SpecHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(specJSON)
	})
}
