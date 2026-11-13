package faucet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"cdr.dev/slog/v3/sloggers/sloghuman"
	"github.com/coder/faucet/faucet"
	"github.com/coder/quartz"
)

const (
	methodList = "events/list"
	methodPoll = "events/poll"

	codeInvalidParams = -32602
	codeInternal      = -32603
	codeNotFound      = -32023
	codeForbidden     = -32024

	deniedRepoID = 99
)

var sampleData = map[string]string{
	"ci.check_completed":  `{"check_run_id":9,"name":"lint","conclusion":"success","html_url":"https://example.test/check/9","app_slug":"octoapp"}`,
	"ci.run_completed":    `{"run_id":1,"name":"CI","conclusion":"success","head_branch":"main","html_url":"https://example.test/run/1","run_attempt":1}`,
	"pr.closed":           `{"merged":true,"merge_commit_sha":"cccccccccccccccccccccccccccccccccccccccc","html_url":"https://example.test/pull/7"}`,
	"pr.comment_created":  `{"comment_id":5,"user_login":"octocat","html_url":"https://example.test/pull/7#c5"}`,
	"pr.review_submitted": `{"review_id":3,"state":"approved","user_login":"octocat","html_url":"https://example.test/pull/7#r3"}`,
}

type rawParams struct {
	mcp.ParamsBase
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Cursor    json.RawMessage `json:"cursor,omitempty"`
	MaxAgeMs  json.RawMessage `json:"maxAgeMs,omitempty"`
	MaxEvents json.RawMessage `json:"maxEvents,omitempty"`
}

type rawResult struct {
	mcp.ResultBase
	fields map[string]json.RawMessage
}

func (r *rawResult) UnmarshalJSON(b []byte) error { return json.Unmarshal(b, &r.fields) }

type occurrence struct {
	EventID   string         `json:"eventId"`
	Name      string         `json:"name"`
	Timestamp time.Time      `json:"timestamp"`
	Data      map[string]any `json:"data"`
}

type pollOutcome struct {
	ResultType string       `json:"resultType"`
	Events     []occurrence `json:"events"`
	Cursor     string       `json:"cursor"`
	Truncated  bool         `json:"truncated"`
	HasMore    bool         `json:"hasMore"`
	NextPollMs int          `json:"nextPollMs"`
}

func (o pollOutcome) eventIDs() []string {
	ids := make([]string, len(o.Events))
	for i, e := range o.Events {
		ids[i] = e.EventID
	}
	return ids
}

type eventsHarness struct {
	t          *testing.T
	ctx        context.Context
	store      *fakeStore
	clock      *quartz.Mock
	authz      *fakeAuthorizer
	logs       *bytes.Buffer
	session    *mcp.ClientSession
	deliveries int
}

func newEventsHarness(t *testing.T, mutate ...func(*faucet.Deps)) *eventsHarness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	t.Cleanup(cancel)

	clock := quartz.NewMock(t)
	store := &fakeStore{now: func() time.Time { return clock.Now() }}
	authz := &fakeAuthorizer{repos: map[string]int64{testRepo: repoID, otherRepo: otherRepoID}}
	logs := &bytes.Buffer{}

	deps := testDeps(store)
	deps.Authorizer = authz
	deps.Clock = clock
	deps.Logger = slog.Make(sloghuman.Sink(logs))
	for _, m := range mutate {
		m(&deps)
	}
	svc, err := faucet.New(deps)
	require.NoError(t, err)

	srv := mcp.NewServer(&mcp.Implementation{Name: "faucet-test", Version: "0"}, &mcp.ServerOptions{Capabilities: faucet.ServerCapabilities()})
	svc.Register(srv)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "0"}, nil)
	for _, method := range []string{methodList, methodPoll} {
		require.NoError(t, mcp.AddSendingCustomMethod[*rawParams, *rawResult](client, method))
	}
	session, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	return &eventsHarness{t: t, ctx: ctx, store: store, clock: clock, authz: authz, logs: logs, session: session}
}

func (h *eventsHarness) insert(repo int64, typ, sha string, prs ...int32) faucet.Event {
	h.t.Helper()
	h.deliveries++
	stored, inserted, err := h.store.InsertEvent(h.ctx, faucet.Event{
		Source:     "github",
		DeliveryID: fmt.Sprintf("delivery-%d", h.deliveries),
		Type:       typ,
		RepoID:     repo,
		PRs:        prs,
		HeadSHA:    sha,
		Data:       json.RawMessage(sampleData[typ]),
	})
	require.NoError(h.t, err)
	require.True(h.t, inserted)
	return stored
}

func (h *eventsHarness) call(method string, params *rawParams) (*rawResult, error) {
	h.t.Helper()
	return mcp.CallCustomMethod[*rawParams, *rawResult](h.ctx, h.session, method, params)
}

func raw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func pollParams(name string, arguments map[string]any, cursor json.RawMessage) *rawParams {
	return &rawParams{Name: name, Arguments: raw(arguments), Cursor: cursor}
}

func cursorOf(c string) json.RawMessage { return raw(c) }

func (h *eventsHarness) poll(params *rawParams) pollOutcome {
	h.t.Helper()
	res, err := h.call(methodPoll, params)
	require.NoError(h.t, err)
	for _, field := range []string{"resultType", "events", "cursor", "truncated", "hasMore", "nextPollMs"} {
		require.Contains(h.t, res.fields, field, "every poll result carries %s", field)
	}
	require.NotEqual(h.t, "null", string(res.fields["events"]), "events is an empty array, never null")
	require.NotEqual(h.t, `""`, string(res.fields["cursor"]))
	return decode[pollOutcome](h.t, res)
}

func (h *eventsHarness) pollError(params *rawParams) *jsonrpc.Error {
	h.t.Helper()
	res, err := h.call(methodPoll, params)
	require.Error(h.t, err, "result: %v", res)
	var rpcErr *jsonrpc.Error
	require.ErrorAs(h.t, err, &rpcErr)
	return rpcErr
}

func decode[T any](t *testing.T, res *rawResult) T {
	t.Helper()
	var out T
	b, err := json.Marshal(res.fields)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

func (h *eventsHarness) headCursor(arguments map[string]any) string {
	h.t.Helper()
	return h.poll(pollParams("ci.run_completed", arguments, nil)).Cursor
}

func repoArgs(extra ...any) map[string]any {
	return args(append([]any{"repo", testRepo}, extra...)...)
}

func TestEventsExtensionIsAdvertisedInDiscovery(t *testing.T) {
	t.Parallel()

	h := newEventsHarness(t)
	caps := h.session.InitializeResult().Capabilities
	require.NotNil(t, caps)
	settings, ok := caps.Extensions[faucet.EventsExtension]
	require.True(t, ok, "capabilities: %+v", caps)
	require.Equal(t, map[string]any{"listChanged": false}, settings)
	require.Equal(t, "io.modelcontextprotocol/events", faucet.EventsExtension)
}

func TestEventsListDescribesEveryEventType(t *testing.T) {
	t.Parallel()

	h := newEventsHarness(t)
	res, err := h.call(methodList, nil)
	require.NoError(t, err)
	require.JSONEq(t, `"complete"`, string(res.fields["resultType"]))
	require.NotContains(t, res.fields, "nextCursor")

	var list struct {
		Events []struct {
			Name          string          `json:"name"`
			Description   string          `json:"description"`
			Delivery      []string        `json:"delivery"`
			InputSchema   json.RawMessage `json:"inputSchema"`
			PayloadSchema json.RawMessage `json:"payloadSchema"`
		} `json:"events"`
	}
	require.NoError(t, json.Unmarshal(res.fields["events"], &list.Events))
	require.Len(t, list.Events, len(allTypes))
	for i, e := range list.Events {
		require.Equal(t, allTypes[i], e.Name)
		require.NotEmpty(t, e.Description, e.Name)
		require.Equal(t, []string{"poll"}, e.Delivery, e.Name)
		for what, schema := range map[string]json.RawMessage{"inputSchema": e.InputSchema, "payloadSchema": e.PayloadSchema} {
			var s jsonschema.Schema
			require.NoError(t, json.Unmarshal(schema, &s), "%s of %s", what, e.Name)
			_, err := s.Resolve(nil)
			require.NoError(t, err, "%s of %s is a valid JSON Schema", what, e.Name)
			require.Equal(t, "object", s.Type, "%s of %s", what, e.Name)
		}
		var input struct {
			Required   []string       `json:"required"`
			Properties map[string]any `json:"properties"`
		}
		require.NoError(t, json.Unmarshal(e.InputSchema, &input))
		require.Equal(t, []string{"repo"}, input.Required)
		require.Contains(t, input.Properties, "sha")
		require.Contains(t, input.Properties, "pr")
	}
}

func TestEventsListRejectsACursorItNeverIssued(t *testing.T) {
	t.Parallel()

	h := newEventsHarness(t)
	_, err := h.call(methodList, &rawParams{Cursor: cursorOf("page-2")})
	var rpcErr *jsonrpc.Error
	require.ErrorAs(t, err, &rpcErr)
	require.EqualValues(t, codeInvalidParams, rpcErr.Code)
}

func TestPolledDataMatchesThePayloadSchema(t *testing.T) {
	t.Parallel()

	h := newEventsHarness(t)
	res, err := h.call(methodList, nil)
	require.NoError(t, err)
	var events []struct {
		Name          string          `json:"name"`
		PayloadSchema json.RawMessage `json:"payloadSchema"`
	}
	require.NoError(t, json.Unmarshal(res.fields["events"], &events))
	schemas := map[string]*jsonschema.Resolved{}
	for _, e := range events {
		var s jsonschema.Schema
		require.NoError(t, json.Unmarshal(e.PayloadSchema, &s))
		schemas[e.Name], err = s.Resolve(nil)
		require.NoError(t, err)
	}

	start := h.headCursor(repoArgs())
	deliveries := []struct{ event, fixture string }{
		{"workflow_run", "workflow_run_completed"},
		{"workflow_run", "workflow_run_completed_two_prs"},
		{"check_run", "check_run_completed"},
		{"pull_request_review", "pull_request_review_submitted"},
		{"pull_request", "pull_request_closed"},
		{"pull_request", "pull_request_merged"},
		{"issue_comment", "issue_comment_created_on_pr"},
	}
	want := map[string]int{}
	for i, d := range deliveries {
		ev, err := faucet.Normalize(d.event, fixture(t, d.fixture), fmt.Sprintf("guid-%d", i))
		require.NoError(t, err)
		require.NotNil(t, ev, d.fixture)
		ev.RepoID = repoID
		_, inserted, err := h.store.InsertEvent(h.ctx, *ev)
		require.NoError(t, err)
		require.True(t, inserted)
		want[ev.Type]++
	}

	got := map[string]int{}
	for _, typ := range allTypes {
		out := h.poll(pollParams(typ, repoArgs(), cursorOf(start)))
		for _, e := range out.Events {
			require.Equal(t, typ, e.Name)
			require.NoError(t, schemas[typ].Validate(e.Data), "%s data %v", typ, e.Data)
			got[typ]++
		}
	}
	require.Equal(t, want, got)
}

func TestPollWithoutACursorReturnsNothingOldAndAHeadCursor(t *testing.T) {
	t.Parallel()

	for name, cursor := range map[string]json.RawMessage{"absent": nil, "null": json.RawMessage("null")} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newEventsHarness(t)
			old := h.insert(repoID, "ci.run_completed", testShaA, 7)
			h.clock.Advance(time.Minute)

			first := h.poll(pollParams("ci.run_completed", repoArgs(), cursor))
			require.Empty(t, first.Events, "an event from before the first poll is not replayed")
			require.Equal(t, "complete", first.ResultType)
			require.False(t, first.Truncated)
			require.False(t, first.HasMore)
			require.Positive(t, first.NextPollMs)

			h.clock.Advance(time.Minute)
			fresh := h.insert(repoID, "ci.run_completed", testShaB, 8)
			second := h.poll(pollParams("ci.run_completed", repoArgs(), cursorOf(first.Cursor)))
			require.Equal(t, []string{fresh.DeliveryID}, second.eventIDs())
			require.NotEqual(t, old.DeliveryID, second.Events[0].EventID)
		})
	}
}

func TestPollReplaysEventsAfterTheCursor(t *testing.T) {
	t.Parallel()

	h := newEventsHarness(t)
	start := h.headCursor(repoArgs())
	h.clock.Advance(time.Minute)
	a := h.insert(repoID, "ci.run_completed", testShaA, 7, 9)
	h.clock.Advance(time.Minute)
	b := h.insert(repoID, "ci.run_completed", "")

	first := h.poll(pollParams("ci.run_completed", repoArgs(), cursorOf(start)))
	require.Equal(t, []string{a.DeliveryID, b.DeliveryID}, first.eventIDs(), "the eventId is the GitHub delivery GUID")
	got := first.Events[0]
	require.Equal(t, "ci.run_completed", got.Name)
	require.True(t, a.ReceivedAt.Equal(got.Timestamp), "timestamp %v, received %v", got.Timestamp, a.ReceivedAt)
	require.Equal(t, time.UTC, got.Timestamp.Location())
	require.Equal(t, map[string]any{
		"run_id": float64(1), "name": "CI", "conclusion": "success", "head_branch": "main",
		"html_url": "https://example.test/run/1", "run_attempt": float64(1),
		"prs": []any{float64(7), float64(9)}, "head_sha": testShaA,
	}, got.Data)
	require.Equal(t, []any{}, first.Events[1].Data["prs"], "an event without pull requests carries an empty list")
	require.NotContains(t, first.Events[1].Data, "head_sha", "an event without a head SHA omits the field")

	again := h.poll(pollParams("ci.run_completed", repoArgs(), cursorOf(first.Cursor)))
	require.Empty(t, again.Events, "the cursor of the last poll skips what it returned")
	require.NotEmpty(t, again.Cursor, "an empty poll still returns a cursor")

	c := h.insert(repoID, "ci.run_completed", testShaB, 8)
	third := h.poll(pollParams("ci.run_completed", repoArgs(), cursorOf(again.Cursor)))
	require.Equal(t, []string{c.DeliveryID}, third.eventIDs())

	replay := h.poll(pollParams("ci.run_completed", repoArgs(), cursorOf(start)))
	require.Equal(t, []string{a.DeliveryID, b.DeliveryID, c.DeliveryID}, replay.eventIDs(), "an old cursor replays")
	h.requireNoLeak(first.Cursor)
}

func (h *eventsHarness) requireNoLeak(texts ...string) {
	h.t.Helper()
	requireNoToken(h.t, "poll output or logs", append(texts, h.logs.String())...)
}

func TestPollFilters(t *testing.T) {
	t.Parallel()

	h := newEventsHarness(t)
	start := h.headCursor(repoArgs())
	runA7 := h.insert(repoID, "ci.run_completed", testShaA, 7).DeliveryID
	runB9 := h.insert(repoID, "ci.run_completed", testShaB, 9).DeliveryID
	runA7and9 := h.insert(repoID, "ci.run_completed", testShaA, 7, 9).DeliveryID
	h.insert(repoID, "pr.closed", testShaA, 7)
	h.insert(otherRepoID, "ci.run_completed", testShaA, 7)

	tests := []struct {
		name string
		args map[string]any
		want []string
	}{
		{"repo and type only", repoArgs(), []string{runA7, runB9, runA7and9}},
		{"sha", repoArgs("sha", testShaA), []string{runA7, runA7and9}},
		{"sha in capitals", repoArgs("sha", strings.ToUpper(testShaA)), []string{runA7, runA7and9}},
		{"pr", repoArgs("pr", 9), []string{runB9, runA7and9}},
		{"sha and pr", repoArgs("sha", testShaA, "pr", 9), []string{runA7and9}},
		{"no match", repoArgs("sha", testShaB, "pr", 7), []string{}},
	}
	for _, tt := range tests {
		out := h.poll(pollParams("ci.run_completed", tt.args, cursorOf(start)))
		require.Equal(t, tt.want, out.eventIDs(), tt.name)
	}

	closed := h.poll(pollParams("pr.closed", repoArgs(), cursorOf(start)))
	require.Len(t, closed.Events, 1, "the event type selects what is returned")
	require.Equal(t, "pr.closed", closed.Events[0].Name)
	other := h.poll(pollParams("ci.run_completed", args("repo", otherRepo), cursorOf(start)))
	require.Len(t, other.Events, 1, "a repository gets only its own events")
}

func TestPollAppliesFiltersBeforeTheEventLimit(t *testing.T) {
	t.Parallel()

	h := newEventsHarness(t)
	start := h.headCursor(repoArgs())
	for range 3 {
		h.insert(repoID, "ci.run_completed", testShaB, 8)
	}
	want := h.insert(repoID, "ci.run_completed", testShaA, 7)

	out := h.poll(&rawParams{
		Name: "ci.run_completed", Arguments: raw(repoArgs("sha", testShaA)),
		Cursor: cursorOf(start), MaxEvents: raw(1),
	})
	require.Equal(t, []string{want.DeliveryID}, out.eventIDs())
	require.False(t, out.HasMore)
}

func TestPollPagesWithMaxEventsAndHasMore(t *testing.T) {
	t.Parallel()

	h := newEventsHarness(t)
	start := h.headCursor(repoArgs())
	ids := make([]string, 3)
	for i := range ids {
		ids[i] = h.insert(repoID, "ci.run_completed", testShaA, 7).DeliveryID
	}
	page := func(cursor string, maxEvents any) pollOutcome {
		return h.poll(&rawParams{Name: "ci.run_completed", Arguments: raw(repoArgs()), Cursor: cursorOf(cursor), MaxEvents: raw(maxEvents)})
	}

	first := page(start, 2)
	require.Equal(t, ids[:2], first.eventIDs())
	require.True(t, first.HasMore)
	second := page(first.Cursor, 2)
	require.Equal(t, ids[2:], second.eventIDs())
	require.False(t, second.HasMore)

	exact := page(start, 3)
	require.Equal(t, ids, exact.eventIDs())
	require.False(t, exact.HasMore, "a page that holds everything has no more")
	require.Equal(t, second.Cursor, exact.Cursor)

	for range 100 {
		h.insert(repoID, "ci.run_completed", testShaA, 7)
	}
	capped := page(start, 100_000)
	require.Len(t, capped.Events, 100, "the server caps a page at its own limit")
	require.True(t, capped.HasMore)
}

func TestPollMarksATruncatedReplay(t *testing.T) {
	t.Parallel()

	h := newEventsHarness(t)
	start := h.headCursor(repoArgs())
	h.clock.Advance(time.Minute)
	old := h.insert(repoID, "ci.run_completed", testShaA, 7)
	h.clock.Advance(10 * time.Minute)
	recent := h.insert(repoID, "ci.run_completed", testShaA, 7)
	h.clock.Advance(time.Minute)

	withAge := func(cursor json.RawMessage, maxAgeMs int) pollOutcome {
		return h.poll(&rawParams{Name: "ci.run_completed", Arguments: raw(repoArgs()), Cursor: cursor, MaxAgeMs: raw(maxAgeMs)})
	}
	fiveMinutes := int(5 * time.Minute / time.Millisecond)

	truncated := withAge(cursorOf(start), fiveMinutes)
	require.Equal(t, []string{recent.DeliveryID}, truncated.eventIDs(), "events older than maxAgeMs are skipped")
	require.True(t, truncated.Truncated, "skipping events is reported")
	fresh := h.poll(pollParams("ci.run_completed", repoArgs(), cursorOf(truncated.Cursor)))
	require.Empty(t, fresh.Events)
	require.False(t, fresh.Truncated)

	wide := withAge(cursorOf(start), int(time.Hour/time.Millisecond))
	require.Equal(t, []string{old.DeliveryID, recent.DeliveryID}, wide.eventIDs())
	require.False(t, wide.Truncated, "a floor older than the cursor skips nothing")

	current := withAge(cursorOf(truncated.Cursor), fiveMinutes)
	require.False(t, current.Truncated, "a cursor past the floor skips nothing")

	fromNow := withAge(nil, fiveMinutes)
	require.Empty(t, fromNow.Events)
	require.False(t, fromNow.Truncated, "maxAgeMs does not apply to a poll from now")
}

func (h *eventsHarness) storeReads() int {
	return len(h.store.pollQueries()) + len(h.store.lookups())
}

func TestPollOfADeniedRepoLooksLikeOneThatDoesNotExist(t *testing.T) {
	t.Parallel()

	h := newEventsHarness(t)
	h.insert(repoID, "ci.run_completed", testShaA, 7)
	h.insert(deniedRepoID, "ci.run_completed", testShaA, 7)

	for _, cursor := range []json.RawMessage{cursorOf("v1.0"), nil} {
		denied := h.pollError(pollParams("ci.run_completed", args("repo", deniedRepo), cursor))
		missing := h.pollError(pollParams("ci.run_completed", args("repo", "octo/does-not-exist"), cursor))
		require.EqualValues(t, codeForbidden, denied.Code)
		require.Equal(t, denied.Code, missing.Code)
		require.Equal(t, denied.Message, missing.Message)
		require.Empty(t, denied.Data)
		require.Empty(t, missing.Data)
		require.NotContains(t, denied.Message, deniedRepo)
	}
	require.Zero(t, h.storeReads(), "a denied poll reads nothing, not even the head cursor")
	require.Contains(t, h.logs.String(), "poll denied")
	h.requireNoLeak()
}

func TestPollAccessFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mutate   func(*faucet.Deps, *fakeAuthorizer)
		wantCode int64
		wantText string
	}{
		{
			name:     "rejected token",
			mutate:   func(_ *faucet.Deps, a *fakeAuthorizer) { a.err = faucet.ErrInvalidToken },
			wantCode: codeForbidden,
			wantText: "GitHub rejected the token",
		},
		{
			name:     "no principal",
			mutate:   func(d *faucet.Deps, _ *fakeAuthorizer) { d.PrincipalFrom = failingPrincipal },
			wantCode: codeForbidden,
			wantText: "authentication required",
		},
		{
			name:     "GitHub unreachable",
			mutate:   func(_ *faucet.Deps, a *fakeAuthorizer) { a.err = xerrors.New("dial tcp 10.1.2.3: refused") },
			wantCode: codeInternal,
			wantText: "could not verify repository access, try again",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var authz *fakeAuthorizer
			h := newEventsHarness(t, func(d *faucet.Deps) {
				authz = d.Authorizer.(*fakeAuthorizer)
				tt.mutate(d, authz)
			})
			h.insert(repoID, "ci.run_completed", testShaA, 7)
			rpcErr := h.pollError(pollParams("ci.run_completed", repoArgs(), cursorOf("v1.0")))
			require.EqualValues(t, tt.wantCode, rpcErr.Code)
			require.Equal(t, tt.wantText, rpcErr.Message)
			require.Zero(t, h.storeReads())
		})
	}
}

func failingPrincipal(context.Context) (faucet.Principal, error) {
	return faucet.Principal{}, xerrors.New("no principal")
}

func TestPollRejectsInvalidParams(t *testing.T) {
	t.Parallel()

	tooBig := fmt.Sprint(int64(1) << 40)
	tests := []struct {
		name   string
		params *rawParams
	}{
		{"no name", &rawParams{Arguments: raw(repoArgs())}},
		{"no arguments", &rawParams{Name: "ci.run_completed"}},
		{"null arguments", &rawParams{Name: "ci.run_completed", Arguments: json.RawMessage("null")}},
		{"arguments of the wrong type", &rawParams{Name: "ci.run_completed", Arguments: raw("octo/repo")}},
		{"no repo", pollParams("ci.run_completed", args("sha", testShaA), nil)},
		{"repo of the wrong type", pollParams("ci.run_completed", args("repo", 42), nil)},
		{"repo without an owner", pollParams("ci.run_completed", args("repo", "repo"), nil)},
		{"repo with a path", pollParams("ci.run_completed", args("repo", "octo/repo/extra"), nil)},
		{"repo that climbs", pollParams("ci.run_completed", args("repo", "octo/.."), nil)},
		{"short sha", pollParams("ci.run_completed", repoArgs("sha", "abc123"), nil)},
		{"sha that is not hex", pollParams("ci.run_completed", repoArgs("sha", strings.Repeat("z", 40)), nil)},
		{"zero pr", pollParams("ci.run_completed", repoArgs("pr", 0), nil)},
		{"negative pr", pollParams("ci.run_completed", repoArgs("pr", -3), nil)},
		{"fractional pr", pollParams("ci.run_completed", repoArgs("pr", 7.5), nil)},
		{"pr as text", pollParams("ci.run_completed", repoArgs("pr", "7"), nil)},
		{"pr beyond int32", &rawParams{Name: "ci.run_completed", Arguments: json.RawMessage(`{"repo":"octo/repo","pr":` + tooBig + `}`)}},
		{"an argument the type does not take", pollParams("ci.run_completed", repoArgs("types", []string{"pr.closed"}), nil)},
		{"empty cursor", pollParams("ci.run_completed", repoArgs(), cursorOf(""))},
		{"cursor of another server", pollParams("ci.run_completed", repoArgs(), cursorOf("12"))},
		{"negative cursor", pollParams("ci.run_completed", repoArgs(), cursorOf("v1.-1"))},
		{"cursor with a sign", pollParams("ci.run_completed", repoArgs(), cursorOf("v1.+1"))},
		{"cursor with a leading zero", pollParams("ci.run_completed", repoArgs(), cursorOf("v1.01"))},
		{"cursor that is not a number", pollParams("ci.run_completed", repoArgs(), cursorOf("v1.abc"))},
		{"cursor that is not text", pollParams("ci.run_completed", repoArgs(), raw(12))},
		{"zero maxEvents", &rawParams{Name: "ci.run_completed", Arguments: raw(repoArgs()), MaxEvents: raw(0)}},
		{"maxEvents beyond int32", &rawParams{Name: "ci.run_completed", Arguments: raw(repoArgs()), MaxEvents: json.RawMessage(tooBig)}},
		{"negative maxEvents", &rawParams{Name: "ci.run_completed", Arguments: raw(repoArgs()), MaxEvents: raw(-1)}},
		{"zero maxAgeMs", &rawParams{Name: "ci.run_completed", Arguments: raw(repoArgs()), Cursor: cursorOf("v1.0"), MaxAgeMs: raw(0)}},
		{"maxAgeMs as text", &rawParams{Name: "ci.run_completed", Arguments: raw(repoArgs()), Cursor: cursorOf("v1.0"), MaxAgeMs: raw("5")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newEventsHarness(t)
			rpcErr := h.pollError(tt.params)
			require.EqualValues(t, codeInvalidParams, rpcErr.Code, rpcErr.Message)
			require.Zero(t, h.authz.callCount(), "invalid params are rejected before the repository check")
			require.Zero(t, h.storeReads())
		})
	}
}

func TestPollOfAnUnknownEventIsNotFound(t *testing.T) {
	t.Parallel()

	h := newEventsHarness(t)
	for _, name := range []string{"ci.nothing", "CI.RUN_COMPLETED", "workflow_run.completed"} {
		rpcErr := h.pollError(pollParams(name, repoArgs(), nil))
		require.EqualValues(t, codeNotFound, rpcErr.Code, name)
		require.JSONEq(t, `{"kind":"event"}`, string(rpcErr.Data))
		require.Contains(t, rpcErr.Message, "ci.run_completed", "the error lists the event types")
	}
	require.Zero(t, h.authz.callCount())
}

func TestPollStoreFailureIsAnInternalError(t *testing.T) {
	t.Parallel()

	h := newEventsHarness(t)
	h.store.mu.Lock()
	h.store.err = xerrors.New("pq: password authentication failed for user \"postgres\"")
	h.store.mu.Unlock()

	for _, cursor := range []json.RawMessage{cursorOf("v1.0"), nil} {
		rpcErr := h.pollError(pollParams("ci.run_completed", repoArgs(), cursor))
		require.EqualValues(t, codeInternal, rpcErr.Code)
		require.Equal(t, "could not read events, try again", rpcErr.Message)
		require.NotContains(t, rpcErr.Message, "password")
	}
}
