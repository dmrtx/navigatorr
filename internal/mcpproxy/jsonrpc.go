package mcpproxy

import (
	"bytes"
	"encoding/json"
	"math/big"
)

// envelope is the part of a JSON-RPC message the proxy looks at. Everything
// else is passed on as raw bytes.
type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// maxNumericID bounds a numeric id's text. sameID compares numbers exactly
// with big.Rat, and an id like 1e10000000 would make that allocate without
// bound; real clients use small integers.
const maxNumericID = 32

// validID reports whether raw is a JSON-RPC request id: a string, or a
// number written without an exponent and at most maxNumericID characters.
func validID(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	c := raw[0]
	if c == '"' {
		return true
	}
	if c != '-' && (c < '0' || c > '9') {
		return false
	}
	return len(raw) <= maxNumericID && !bytes.ContainsAny(raw, "eE")
}

// sameID compares two JSON-RPC ids by value, so 1 and 1.0 match and "1" does not.
func sameID(a, b json.RawMessage) bool {
	var x, y any
	da := json.NewDecoder(bytes.NewReader(a))
	da.UseNumber()
	db := json.NewDecoder(bytes.NewReader(b))
	db.UseNumber()
	if da.Decode(&x) != nil || db.Decode(&y) != nil {
		return false
	}
	switch xv := x.(type) {
	case string:
		yv, ok := y.(string)
		return ok && xv == yv
	case json.Number:
		yv, ok := y.(json.Number)
		if !ok {
			return false
		}
		rx, ok1 := new(big.Rat).SetString(string(xv))
		ry, ok2 := new(big.Rat).SetString(string(yv))
		return ok1 && ok2 && rx.Cmp(ry) == 0
	}
	return false
}

// rpcError builds a JSON-RPC error response. id is the client's raw id, or
// nil for "null".
func rpcError(id json.RawMessage, code int, msg string) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	m, _ := json.Marshal(msg)
	var b bytes.Buffer
	b.WriteString(`{"jsonrpc":"2.0","id":`)
	b.Write(id)
	b.WriteString(`,"error":{"code":`)
	b.WriteString(itoa(code))
	b.WriteString(`,"message":`)
	b.Write(m)
	b.WriteString(`}}`)
	return b.Bytes()
}

// toolError builds a successful JSON-RPC response whose result is a tool
// error. The connector hands a result to the model as is, so this is how the
// model reads why a call failed.
func toolError(id json.RawMessage, text string) []byte {
	m, _ := json.Marshal(text)
	var b bytes.Buffer
	b.WriteString(`{"jsonrpc":"2.0","id":`)
	b.Write(id)
	b.WriteString(`,"result":{"content":[{"type":"text","text":`)
	b.Write(m)
	b.WriteString(`}],"isError":true}}`)
	return b.Bytes()
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
