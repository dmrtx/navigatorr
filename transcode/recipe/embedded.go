package recipe

import _ "embed"

//go:embed default.yaml
var embeddedBundle []byte

func EmbeddedBytes() []byte { return append([]byte(nil), embeddedBundle...) }
