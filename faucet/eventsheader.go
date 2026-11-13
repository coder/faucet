package faucet

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	nameHeader   = "Mcp-Name"
	base64Prefix = "=?base64?"
	base64Suffix = "?="
)

var nameMethods = []string{"events/poll", "events/stream", "events/subscribe", "events/unsubscribe"}

// RequireEventName wraps an MCP Streamable HTTP handler. It answers 400 with
// the HeaderMismatch error when the Mcp-Name header of an events/* request is
// missing or differs from params.name. The SDK checks the header for tools,
// resources and prompts only. A host that serves Service.Register over HTTP
// puts this in front of the SDK handler. The wrapper reads each body whole, up
// to mcp.DefaultMaxRequestBodyBytes, and answers 413 beyond that.
func RequireEventName(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mcp.DefaultMaxRequestBodyBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "could not read the request body", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if f := checkEventName(body, r.Header.Get(nameHeader)); f != nil {
			writeRPCFailure(w, f)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type rpcFailure struct {
	id      json.RawMessage
	code    int64
	message string
}

// The fields are read by exact name, because the SDK
// matches them that way and encoding/json would not. A repeated field fails as
// well: the SDK keeps an earlier string when a later duplicate is null, and a
// map here would keep the null.
func checkEventName(body []byte, header string) *rpcFailure {
	if !json.Valid(body) {
		return &rpcFailure{code: jsonrpc.CodeParseError, message: "request body is not one JSON value"}
	}
	var requests []json.RawMessage
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("[")) {
		if json.Unmarshal(body, &requests) != nil {
			return &rpcFailure{code: jsonrpc.CodeParseError, message: "request body is not valid JSON-RPC"}
		}
	} else {
		requests = []json.RawMessage{body}
	}
	for _, raw := range requests {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return &rpcFailure{code: jsonrpc.CodeInvalidRequest, message: "request is not a JSON object"}
		}
		if hasRepeatedKey(raw) {
			return &rpcFailure{fields["id"], jsonrpc.CodeInvalidRequest, "request repeats a field"}
		}
		var method string
		if json.Unmarshal(fields["method"], &method) != nil || !slices.Contains(nameMethods, method) {
			continue
		}
		if hasRepeatedKey(fields["params"]) {
			return &rpcFailure{fields["id"], jsonrpc.CodeInvalidRequest, "params repeats a field"}
		}
		var params map[string]json.RawMessage
		var name string
		if json.Unmarshal(fields["params"], &params) != nil || json.Unmarshal(params["name"], &name) != nil || name == "" {
			continue
		}
		switch got, ok := decodeNameHeader(header); {
		case header == "":
			return &rpcFailure{fields["id"], mcp.CodeHeaderMismatch, fmt.Sprintf("missing required Mcp-Name header for method %q", method)}
		case !ok || got != name:
			return &rpcFailure{fields["id"], mcp.CodeHeaderMismatch, "header mismatch: Mcp-Name header does not match params.name"}
		}
	}
	return nil
}

func hasRepeatedKey(raw json.RawMessage) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		key, isKey := tok.(string)
		if err != nil || !isKey {
			return false
		}
		if seen[key] {
			return true
		}
		seen[key] = true
		var value json.RawMessage
		if dec.Decode(&value) != nil {
			return false
		}
	}
	return false
}

// decodeNameHeader reads a header value that is plain or in the Base64
// sentinel form that SEP-2243 gives to names that are not plain ASCII.
func decodeNameHeader(value string) (string, bool) {
	encoded, ok := strings.CutPrefix(value, base64Prefix)
	if !ok {
		return value, true
	}
	encoded, ok = strings.CutSuffix(encoded, base64Suffix)
	if !ok {
		return value, true
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}
	return string(decoded), true
}

func writeRPCFailure(w http.ResponseWriter, f *rpcFailure) {
	id := f.id
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	body, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   *jsonrpc.Error  `json:"error"`
	}{"2.0", id, &jsonrpc.Error{Code: f.code, Message: f.message}})
	if err != nil {
		panic(fmt.Sprintf("marshal JSON-RPC error: %v", err))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write(body)
}
