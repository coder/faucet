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

// nameMethods are the events/* methods that carry the event name in
// params.name, so SEP-3415 requires their Mcp-Name header.
var nameMethods = []string{"events/poll", "events/stream", "events/subscribe", "events/unsubscribe"}

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// RequireEventName wraps an MCP Streamable HTTP handler. It answers 400 with
// the HeaderMismatch error when the Mcp-Name header of an events/* request is
// missing or differs from params.name. The SDK checks the header for tools,
// resources and prompts only. A host that serves Service.Register over HTTP
// puts this in front of the SDK handler.
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
		if id, problem := checkEventName(body, r.Header.Get(nameHeader)); problem != "" {
			writeHeaderMismatch(w, id, problem)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// checkEventName returns the request ID and the problem of the first events/*
// request in body that the header does not match. A body that is not JSON-RPC
// is left to the SDK to reject.
func checkEventName(body []byte, header string) (id json.RawMessage, problem string) {
	var requests []rpcRequest
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("[")) {
		if json.Unmarshal(body, &requests) != nil {
			return nil, ""
		}
	} else {
		var single rpcRequest
		if json.Unmarshal(body, &single) != nil {
			return nil, ""
		}
		requests = []rpcRequest{single}
	}
	for _, req := range requests {
		if !slices.Contains(nameMethods, req.Method) {
			continue
		}
		var params struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(req.Params, &params) != nil || params.Name == "" {
			continue
		}
		if header == "" {
			return req.ID, fmt.Sprintf("missing required Mcp-Name header for method %q", req.Method)
		}
		if got, ok := decodeNameHeader(header); !ok || got != params.Name {
			return req.ID, "header mismatch: Mcp-Name header does not match params.name"
		}
	}
	return nil, ""
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

func writeHeaderMismatch(w http.ResponseWriter, id json.RawMessage, message string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	body, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   *jsonrpc.Error  `json:"error"`
	}{"2.0", id, &jsonrpc.Error{Code: mcp.CodeHeaderMismatch, Message: message}})
	if err != nil {
		panic(fmt.Sprintf("marshal header mismatch error: %v", err))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write(body)
}
