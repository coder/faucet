package faucet_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

func rpcBody(id, method, params string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"` + method + `","params":` + params + `}`
}

func TestRequireEventName(t *testing.T) {
	t.Parallel()

	const (
		pollA     = `{"name":"ci.run_completed","arguments":{"repo":"octo/repo"}}`
		sentinel  = "=?base64?"
		unicode   = "événement.é"
		wantMatch = http.StatusOK
	)
	encoded := sentinel + base64.StdEncoding.EncodeToString([]byte(unicode)) + "?="
	tests := []struct {
		name        string
		method      string
		body        string
		header      string
		wantStatus  int
		wantID      string
		wantMessage string
	}{
		{name: "matching name", method: http.MethodPost, body: rpcBody("1", "events/poll", pollA), header: "ci.run_completed", wantStatus: wantMatch},
		{name: "missing header", method: http.MethodPost, body: rpcBody("7", "events/poll", pollA), wantStatus: http.StatusBadRequest, wantID: "7", wantMessage: "missing required Mcp-Name header"},
		{name: "another name", method: http.MethodPost, body: rpcBody("8", "events/poll", pollA), header: "pr.closed", wantStatus: http.StatusBadRequest, wantID: "8", wantMessage: "does not match"},
		{name: "name in another case", method: http.MethodPost, body: rpcBody("9", "events/poll", pollA), header: "CI.RUN_COMPLETED", wantStatus: http.StatusBadRequest, wantID: "9", wantMessage: "does not match"},
		{name: "string id", method: http.MethodPost, body: rpcBody(`"abc"`, "events/poll", pollA), wantStatus: http.StatusBadRequest, wantID: `"abc"`, wantMessage: "missing"},
		{name: "base64 name", method: http.MethodPost, body: rpcBody("1", "events/poll", `{"name":"`+unicode+`"}`), header: encoded, wantStatus: wantMatch},
		{name: "base64 name of another event", method: http.MethodPost, body: rpcBody("1", "events/poll", `{"name":"pr.closed"}`), header: encoded, wantStatus: http.StatusBadRequest, wantID: "1", wantMessage: "does not match"},
		{name: "broken base64", method: http.MethodPost, body: rpcBody("2", "events/poll", pollA), header: sentinel + "!!!?=", wantStatus: http.StatusBadRequest, wantID: "2", wantMessage: "does not match"},
		{name: "subscribe", method: http.MethodPost, body: rpcBody("3", "events/subscribe", pollA), wantStatus: http.StatusBadRequest, wantID: "3", wantMessage: "missing"},
		{name: "stream", method: http.MethodPost, body: rpcBody("3", "events/stream", pollA), header: "x", wantStatus: http.StatusBadRequest, wantID: "3", wantMessage: "does not match"},
		{name: "unsubscribe", method: http.MethodPost, body: rpcBody("3", "events/unsubscribe", pollA), wantStatus: http.StatusBadRequest, wantID: "3", wantMessage: "missing"},
		{name: "events list has no name", method: http.MethodPost, body: rpcBody("4", "events/list", `{}`), wantStatus: wantMatch},
		{name: "tool call is the SDK's to check", method: http.MethodPost, body: rpcBody("4", "tools/call", `{"name":"wait_for_github_events"}`), wantStatus: wantMatch},
		{name: "poll without a name is the handler's to reject", method: http.MethodPost, body: rpcBody("5", "events/poll", `{"arguments":{}}`), wantStatus: wantMatch},
		{name: "poll with a name that is not text", method: http.MethodPost, body: rpcBody("5", "events/poll", `{"name":7}`), wantStatus: wantMatch},
		{name: "poll without params", method: http.MethodPost, body: `{"jsonrpc":"2.0","id":5,"method":"events/poll"}`, wantStatus: wantMatch},
		{name: "not JSON", method: http.MethodPost, body: `not json`, wantStatus: wantMatch},
		{name: "notification", method: http.MethodPost, body: `{"jsonrpc":"2.0","method":"events/poll","params":` + pollA + `}`, wantStatus: http.StatusBadRequest, wantID: "null", wantMessage: "missing"},
		{name: "batch that matches", method: http.MethodPost, body: `[` + rpcBody("1", "events/poll", pollA) + `,` + rpcBody("2", "events/list", `{}`) + `]`, header: "ci.run_completed", wantStatus: wantMatch},
		{name: "batch with another name", method: http.MethodPost, body: `[` + rpcBody("1", "events/list", `{}`) + `,` + rpcBody("2", "events/poll", pollA) + `]`, header: "pr.closed", wantStatus: http.StatusBadRequest, wantID: "2", wantMessage: "does not match"},
		{name: "broken batch", method: http.MethodPost, body: `[{`, wantStatus: wantMatch},
		{name: "GET", method: http.MethodGet, wantStatus: wantMatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var seen []byte
			var reached bool
			handler := faucet.RequireEventName(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				seen, _ = io.ReadAll(r.Body)
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest(tt.method, "/mcp", strings.NewReader(tt.body))
			if tt.header != "" {
				req.Header.Set("Mcp-Name", tt.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			require.Equal(t, tt.wantStatus, rec.Code)
			if tt.wantStatus == wantMatch {
				require.True(t, reached)
				require.Equal(t, tt.body, string(seen), "the wrapped handler reads the whole body")
				return
			}
			require.False(t, reached, "a rejected request never reaches the SDK")
			require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			var resp struct {
				ID    json.RawMessage `json:"id"`
				Error struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.Equal(t, tt.wantID, string(resp.ID))
			require.Equal(t, mcp.CodeHeaderMismatch, resp.Error.Code)
			require.Contains(t, resp.Error.Message, tt.wantMessage)
		})
	}
}

func TestRequireEventNameRefusesABodyBeyondTheSDKLimit(t *testing.T) {
	t.Parallel()

	reached := false
	handler := faucet.RequireEventName(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	body := strings.Repeat(" ", mcp.DefaultMaxRequestBodyBytes+1)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body)))

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	require.False(t, reached)
}
