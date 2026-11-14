package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

type eventsParams struct {
	mcp.ParamsBase
	Name      string         `json:"name,omitempty"`
	Arguments map[string]any `json:"arguments,omitempty"`
	Cursor    *string        `json:"cursor,omitempty"`
}

type eventsResult struct {
	mcp.ResultBase
	fields map[string]json.RawMessage
}

func (r *eventsResult) UnmarshalJSON(b []byte) error { return json.Unmarshal(b, &r.fields) }

// eventNameTransport sets Mcp-Name on events/* requests as SEP-3415 asks every
// client to. The SDK client sets the header for tools, resources and prompts only.
type eventNameTransport struct{ next http.RoundTripper }

func (e eventNameTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost && r.Body != nil {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		_ = r.Body.Close()
		var req struct {
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		r = r.Clone(r.Context())
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		if strings.HasPrefix(req.Method, "events/") && req.Params.Name != "" {
			r.Header.Set("Mcp-Name", req.Params.Name)
		}
	}
	return e.next.RoundTrip(r)
}

func (s *testServer) connectEvents(ctx context.Context, t *testing.T, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	for _, method := range []string{"events/list", "events/poll"} {
		require.NoError(t, mcp.AddSendingCustomMethod[*eventsParams, *eventsResult](client, method))
	}
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             s.URL + "/mcp",
		HTTPClient:           &http.Client{Transport: eventNameTransport{next: bearerTransport{token, s.Client().Transport}}},
		DisableStandaloneSSE: true,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func callEvents(ctx context.Context, session *mcp.ClientSession, method string, params *eventsParams) (map[string]json.RawMessage, error) {
	res, err := mcp.CallCustomMethod[*eventsParams, *eventsResult](ctx, session, method, params)
	if err != nil {
		return nil, err
	}
	return res.fields, nil
}

func pollParams(repo string, cursor *string) *eventsParams {
	return &eventsParams{Name: "ci.run_completed", Arguments: map[string]any{"repo": repo}, Cursor: cursor}
}

func TestMCPDiscoveryAdvertisesEvents(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	srv := newTestServer(t)
	session := srv.connectEvents(ctx, t, aliceToken)

	init := session.InitializeResult()
	require.GreaterOrEqual(t, init.ProtocolVersion, "2026-07-28", "the client reached the server through server/discover")
	require.Equal(t, map[string]any{"listChanged": false}, init.Capabilities.Extensions[faucet.EventsExtension])
	require.NotNil(t, init.Capabilities.Tools, "the wait tool is still advertised")
}

func TestMCPEventsListAndPollOverHTTP(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	srv := newTestServer(t)
	session := srv.connectEvents(ctx, t, aliceToken)

	list, err := callEvents(ctx, session, "events/list", nil)
	require.NoError(t, err)
	var events []struct {
		Name     string   `json:"name"`
		Delivery []string `json:"delivery"`
	}
	require.NoError(t, json.Unmarshal(list["events"], &events))
	require.Len(t, events, 5)
	require.Equal(t, []string{"poll"}, events[0].Delivery)

	head, err := callEvents(ctx, session, "events/poll", pollParams("o/r", nil))
	require.NoError(t, err)
	require.JSONEq(t, `[]`, string(head["events"]))
	var cursor string
	require.NoError(t, json.Unmarshal(head["cursor"], &cursor))
	require.NotEmpty(t, cursor)

	res, err := callEvents(ctx, session, "events/poll", pollParams("o/r", &cursor))
	require.NoError(t, err)
	var polled []struct {
		EventID string         `json:"eventId"`
		Name    string         `json:"name"`
		Data    map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(res["events"], &polled))
	require.Len(t, polled, 1)
	require.Equal(t, "d", polled[0].EventID)
	require.Equal(t, "ci.run_completed", polled[0].Name)
	require.Equal(t, "success", polled[0].Data["conclusion"])
	require.Equal(t, []any{float64(3)}, polled[0].Data["prs"])

	paths, auth := srv.github.seen()
	require.Contains(t, paths, "/repos/o/r")
	for _, a := range auth {
		require.Equal(t, "Bearer "+aliceToken, a, "GitHub got the caller's own token")
	}
}

func TestMCPEventsPollDeniesARepoTheCallerCannotSee(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	srv := newTestServer(t)

	bob := srv.connectEvents(ctx, t, bobToken)
	_, err := callEvents(ctx, bob, "events/poll", pollParams("o/r", nil))
	var denied *jsonrpc.Error
	require.ErrorAs(t, err, &denied)
	require.EqualValues(t, -32024, denied.Code)

	alice := srv.connectEvents(ctx, t, aliceToken)
	_, err = callEvents(ctx, alice, "events/poll", pollParams("o/hidden", nil))
	var hidden *jsonrpc.Error
	require.ErrorAs(t, err, &hidden)
	require.Equal(t, denied.Code, hidden.Code)
	require.Equal(t, denied.Message, hidden.Message, "a repository alice cannot see reads like one that does not exist")

	require.Zero(t, srv.store.pollCount(), "no events were read for a denied poll")
	srv.requireNoToken(t, "a denial", denied.Message)
}

func TestMCPEventsListIsSafeForConcurrentRequests(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	srv := newTestServer(t)
	session := srv.connectEvents(ctx, t, aliceToken)

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			_, err := callEvents(ctx, session, "events/list", nil)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

const metaTriple = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
	`"io.modelcontextprotocol/clientInfo":{"name":"raw","version":"0"},` +
	`"io.modelcontextprotocol/clientCapabilities":{}}`

func eventsPollRequest(t *testing.T, srv *testServer, token, name string) (status int, body string) {
	t.Helper()
	params := `{"name":"ci.run_completed","arguments":{"repo":"o/r"},` + metaTriple + `}`
	return postEventsPoll(t, srv, token, name, params)
}

func postEventsPoll(t *testing.T, srv *testServer, token, name, params string) (status int, body string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	payload := `{"jsonrpc":"2.0","id":7,"method":"events/poll","params":` + params + `}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/mcp", strings.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "events/poll")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if name != "" {
		req.Header.Set("Mcp-Name", name)
	}
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(raw)
}

func TestMCPEventsPollNeedsTheNameHeader(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)

	status, body := eventsPollRequest(t, srv, aliceToken, "ci.run_completed")
	require.Equal(t, http.StatusOK, status, body)
	require.Contains(t, body, `"cursor"`)

	for _, name := range []string{"", "pr.closed"} {
		status, body = eventsPollRequest(t, srv, aliceToken, name)
		require.Equal(t, http.StatusBadRequest, status, body)
		require.Contains(t, body, `"code":-32020`)
		require.Contains(t, body, `"id":7`)
	}

	status, _ = eventsPollRequest(t, srv, "", "")
	require.Equal(t, http.StatusUnauthorized, status, "authentication comes before the header check")
}

func TestMCPEventsPollNameHeaderAgreesWithTheEventTheSDKServes(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	tests := []struct {
		name       string
		params     string
		header     string
		wantStatus int
		wantServed string
	}{
		{"a repeated name is refused", `{"name":"ci.run_completed","name":null,"arguments":{"repo":"o/r"},` + metaTriple + `}`, "", http.StatusBadRequest, ""},
		{"a repeated name with a header for the last", `{"name":"ci.run_completed","name":"zzz.first","arguments":{"repo":"o/r"},` + metaTriple + `}`, "zzz.first", http.StatusBadRequest, ""},
		{"one unknown name is served as sent", `{"name":"zzz.only","arguments":{"repo":"o/r"},` + metaTriple + `}`, "zzz.only", http.StatusOK, "zzz.only"},
		{"a name in another case is a different field", `{"name":"zzz.sdk","Name":"ci.run_completed","arguments":{"repo":"o/r"},` + metaTriple + `}`, "ci.run_completed", http.StatusBadRequest, ""},
	}
	for _, tt := range tests {
		status, body := postEventsPoll(t, srv, aliceToken, tt.header, tt.params)
		require.Equal(t, tt.wantStatus, status, "%s: %s", tt.name, body)
		if tt.wantServed != "" {
			require.Contains(t, body, `unknown event type \"`+tt.wantServed+`\"`, tt.name)
		}
	}
}
