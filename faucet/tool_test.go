package faucet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"cdr.dev/slog/v3/sloggers/sloghuman"
	"github.com/coder/faucet/faucet"
	"github.com/coder/quartz"
)

const (
	testRepo   = "octo/repo"
	otherRepo  = "octo/other"
	deniedRepo = "octo/private"

	repoID      = 42
	otherRepoID = 43

	testShaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testShaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	testTimeout = 30 * time.Second
)

var allTypes = []string{"ci.check_completed", "ci.run_completed", "pr.closed", "pr.comment_created", "pr.review_submitted"}

type fakeAuthorizer struct {
	mu    sync.Mutex
	repos map[string]int64
	err   error
	calls []string
}

func (a *fakeAuthorizer) RepoID(_ context.Context, p faucet.Principal, repo string) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, p.Login+" "+repo)
	if a.err != nil {
		return 0, a.err
	}
	id, ok := a.repos[repo]
	if !ok {
		return 0, faucet.ErrNoAccess
	}
	return id, nil
}

func (a *fakeAuthorizer) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

func testDeps(store faucet.Store) faucet.Deps {
	return faucet.Deps{
		Store:      store,
		Secret:     []byte(testSecret),
		Authorizer: &fakeAuthorizer{repos: map[string]int64{testRepo: repoID}},
		PrincipalFrom: func(context.Context) (faucet.Principal, error) {
			return faucet.NewPrincipal("octocat", testToken)
		},
		Clock: quartz.NewReal(),
	}
}

type waitHarness struct {
	t       *testing.T
	ctx     context.Context
	store   *fakeStore
	clock   *quartz.Mock
	authz   *fakeAuthorizer
	logs    *bytes.Buffer
	session *mcp.ClientSession
	timers  *quartz.Trap
	deps    faucet.Deps
}

func newWaitHarness(t *testing.T, mutate ...func(*faucet.Deps)) *waitHarness {
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

	srv := mcp.NewServer(&mcp.Implementation{Name: "faucet-test", Version: "0"}, nil)
	svc.Register(srv)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	timers := clock.Trap().NewTimer()
	t.Cleanup(timers.Close)
	return &waitHarness{t: t, ctx: ctx, store: store, clock: clock, authz: authz, logs: logs, session: session, timers: timers, deps: deps}
}

type waitResult struct {
	Events []struct {
		ID         int64           `json:"id"`
		Type       string          `json:"type"`
		RepoID     int64           `json:"repo_id"`
		PRs        []int32         `json:"prs"`
		HeadSHA    string          `json:"head_sha"`
		Data       json.RawMessage `json:"data"`
		ReceivedAt time.Time       `json:"received_at"`
	} `json:"events"`
	Cursor int64 `json:"cursor"`
}

func (r waitResult) ids() []int64 {
	ids := make([]int64, len(r.Events))
	for i, e := range r.Events {
		ids[i] = e.ID
	}
	return ids
}

type callOutcome struct {
	result *mcp.CallToolResult
	err    error
}

func (h *waitHarness) callAsync(ctx context.Context, args map[string]any) <-chan callOutcome {
	out := make(chan callOutcome, 1)
	go func() {
		res, err := h.session.CallTool(ctx, &mcp.CallToolParams{Name: "wait_for_github_events", Arguments: args})
		out <- callOutcome{res, err}
	}()
	return out
}

func (h *waitHarness) await(out <-chan callOutcome) callOutcome {
	h.t.Helper()
	select {
	case o := <-out:
		return o
	case <-h.ctx.Done():
		h.t.Fatal("the tool call did not return")
		return callOutcome{}
	}
}

// failure returns the text of a tool error, or the protocol error text when
// the SDK rejected the call before the handler ran.
func (o callOutcome) failure(t *testing.T) string {
	t.Helper()
	if o.err != nil {
		return o.err.Error()
	}
	require.NotNil(t, o.result)
	require.True(t, o.result.IsError, "expected a tool error")
	require.NotEmpty(t, o.result.Content)
	text, ok := o.result.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	return text.Text
}

func (o callOutcome) success(t *testing.T) waitResult {
	t.Helper()
	require.NoError(t, o.err)
	require.NotNil(t, o.result)
	require.False(t, o.result.IsError, "tool error: %v", o.result.Content)
	raw, err := json.Marshal(o.result.StructuredContent)
	require.NoError(t, err)
	var res waitResult
	require.NoError(t, json.Unmarshal(raw, &res))
	return res
}

func (h *waitHarness) call(args map[string]any) callOutcome {
	h.t.Helper()
	return h.await(h.callAsync(h.ctx, args))
}

// step lets the wait loop sleep once: it waits for the loop to arm its timer,
// checks the duration, and advances the clock past it. The loop is blocked
// while the timer call is held, so whileParked runs between two polls.
func (h *waitHarness) step(want time.Duration, whileParked ...func()) {
	h.t.Helper()
	call := h.timers.MustWait(h.ctx)
	require.Equal(h.t, want, call.Duration)
	for _, f := range whileParked {
		f()
	}
	call.MustRelease(h.ctx)
	h.clock.Advance(want).MustWait(h.ctx)
}

func (h *waitHarness) insert(repo int64, typ, sha string, prs ...int32) faucet.Event {
	h.t.Helper()
	ev := faucet.Event{
		Source:     "github",
		DeliveryID: fmt.Sprintf("delivery-%d", len(h.store.stored())+1),
		Type:       typ,
		RepoID:     repo,
		PRs:        prs,
		HeadSHA:    sha,
		Data:       json.RawMessage(`{"conclusion":"success"}`),
	}
	stored, inserted, err := h.store.InsertEvent(h.ctx, ev)
	require.NoError(h.t, err)
	require.True(h.t, inserted)
	return stored
}

func (h *waitHarness) requireNoLeak(texts ...string) {
	h.t.Helper()
	requireNoToken(h.t, "tool output or logs", append(texts, h.logs.String())...)
}

func args(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func TestNewRequiresCollaborators(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*faucet.Deps){
		"authorizer":     func(d *faucet.Deps) { d.Authorizer = nil },
		"principal from": func(d *faucet.Deps) { d.PrincipalFrom = nil },
		"clock":          func(d *faucet.Deps) { d.Clock = nil },
	} {
		deps := testDeps(&fakeStore{})
		mutate(&deps)
		svc, err := faucet.New(deps)
		require.Error(t, err, name)
		require.Nil(t, svc, name)
	}
}

func TestWaitToolIsListed(t *testing.T) {
	t.Parallel()

	h := newWaitHarness(t)
	tools, err := h.session.ListTools(h.ctx, nil)
	require.NoError(t, err)
	require.Len(t, tools.Tools, 1)

	tool := tools.Tools[0]
	require.Equal(t, "wait_for_github_events", tool.Name)
	require.True(t, tool.Annotations.ReadOnlyHint)
	for _, typ := range allTypes {
		require.Contains(t, tool.Description, typ)
	}
	schema, err := json.Marshal(tool.InputSchema)
	require.NoError(t, err)
	var parsed struct {
		Required []string `json:"required"`
	}
	require.NoError(t, json.Unmarshal(schema, &parsed))
	require.ElementsMatch(t, []string{"repo", "types"}, parsed.Required)
}

func TestWaitReturnsEventsForAnAllowedRepo(t *testing.T) {
	t.Parallel()

	h := newWaitHarness(t)
	ev := h.insert(repoID, "ci.run_completed", testShaA, 7, 9)
	noPRs := h.insert(repoID, "pr.closed", "")

	res := h.call(args("repo", testRepo, "types", allTypes, "cursor", 0)).success(t)
	require.Equal(t, []int64{ev.ID, noPRs.ID}, res.ids())
	require.Equal(t, noPRs.ID, res.Cursor)

	got := res.Events[0]
	require.Equal(t, "ci.run_completed", got.Type)
	require.EqualValues(t, repoID, got.RepoID)
	require.Equal(t, []int32{7, 9}, got.PRs)
	require.Equal(t, testShaA, got.HeadSHA)
	require.JSONEq(t, `{"conclusion":"success"}`, string(got.Data))
	require.True(t, ev.ReceivedAt.Equal(got.ReceivedAt))

	require.NotNil(t, res.Events[1].PRs, "an event without pull requests carries an empty list, not null")
	require.Empty(t, res.Events[1].PRs)
	require.Equal(t, []string{"octocat " + testRepo}, h.authz.calls)
	h.requireNoLeak()
}

func TestWaitFiltersEvents(t *testing.T) {
	t.Parallel()

	h := newWaitHarness(t)
	e := []faucet.Event{
		h.insert(repoID, "ci.run_completed", testShaA, 7),
		h.insert(repoID, "pr.closed", "", 7),
		h.insert(repoID, "ci.run_completed", testShaB, 9),
		h.insert(repoID, "pr.review_submitted", testShaA, 7, 9),
		h.insert(otherRepoID, "ci.run_completed", testShaA, 7),
	}

	tests := []struct {
		name string
		args map[string]any
		want []faucet.Event
	}{
		{"repo", args("repo", testRepo, "types", allTypes), []faucet.Event{e[0], e[1], e[2], e[3]}},
		{"other repo", args("repo", otherRepo, "types", allTypes), []faucet.Event{e[4]}},
		{"one type", args("repo", testRepo, "types", []string{"pr.closed"}), []faucet.Event{e[1]}},
		{"two types", args("repo", testRepo, "types", []string{"pr.closed", "pr.review_submitted"}), []faucet.Event{e[1], e[3]}},
		{"repeated type", args("repo", testRepo, "types", []string{"pr.closed", "pr.closed"}), []faucet.Event{e[1]}},
		{"sha", args("repo", testRepo, "types", allTypes, "sha", testShaA), []faucet.Event{e[0], e[3]}},
		{"upper case sha", args("repo", testRepo, "types", allTypes, "sha", strings.ToUpper(testShaB)), []faucet.Event{e[2]}},
		{"pr", args("repo", testRepo, "types", allTypes, "pr", 9), []faucet.Event{e[2], e[3]}},
		{"cursor", args("repo", testRepo, "types", allTypes, "cursor", e[1].ID), []faucet.Event{e[2], e[3]}},
		{"everything", args("repo", testRepo, "types", []string{"ci.run_completed"}, "sha", testShaA, "pr", 7, "cursor", 0), []faucet.Event{e[0]}},
	}
	for _, tt := range tests {
		if _, ok := tt.args["cursor"]; !ok {
			tt.args["cursor"] = 0
		}
		res := h.call(tt.args).success(t)
		want := make([]int64, len(tt.want))
		for i, ev := range tt.want {
			want[i] = ev.ID
		}
		require.Equal(t, want, res.ids(), tt.name)
	}
}

func TestWaitAcceptsEveryCatalogTypeAndTheLimits(t *testing.T) {
	t.Parallel()

	h := newWaitHarness(t)
	for _, typ := range allTypes {
		h.insert(repoID, typ, "")
	}
	for _, timeout := range []int{1, 50} {
		res := h.call(args("repo", testRepo, "types", allTypes, "cursor", 0, "timeout_s", timeout)).success(t)
		require.Len(t, res.Events, len(allTypes))
	}
}

func TestWaitCursorOfNothingMatchingIsTheHead(t *testing.T) {
	t.Parallel()

	h := newWaitHarness(t)
	h.insert(repoID, "pr.closed", "")
	last := h.insert(otherRepoID, "ci.run_completed", testShaA)

	out := h.callAsync(h.ctx, args("repo", testRepo, "types", []string{"ci.run_completed"}, "cursor", 0, "timeout_s", 2))
	h.step(2 * time.Second)
	res := h.await(out).success(t)
	require.Empty(t, res.Events)
	require.Equal(t, last.ID, res.Cursor, "the cursor skips events of other repositories")
}

func TestWaitStartsAtTheHeadWithoutACursor(t *testing.T) {
	t.Parallel()

	h := newWaitHarness(t)
	old := h.insert(repoID, "ci.run_completed", testShaA)
	h.clock.Advance(time.Second).MustWait(h.ctx)

	out := h.callAsync(h.ctx, args("repo", testRepo, "types", allTypes, "timeout_s", 4))
	var fresh faucet.Event
	h.step(2*time.Second, func() { fresh = h.insert(repoID, "ci.run_completed", testShaB) })

	res := h.await(out).success(t)
	require.Equal(t, []int64{fresh.ID}, res.ids(), "the event from before the call is not returned")
	require.Greater(t, fresh.ID, old.ID)
	require.Equal(t, fresh.ID, res.Cursor)
}

func TestWaitReturnsAnEventThatArrivesWhileWaiting(t *testing.T) {
	t.Parallel()

	h := newWaitHarness(t)
	out := h.callAsync(h.ctx, args("repo", testRepo, "types", []string{"ci.run_completed"}, "sha", testShaA, "cursor", 0))

	h.step(2*time.Second, func() {
		h.insert(repoID, "ci.run_completed", testShaB)
		h.insert(otherRepoID, "ci.run_completed", testShaA)
	})
	var want faucet.Event
	h.step(2*time.Second, func() { want = h.insert(repoID, "ci.run_completed", testShaA) })

	res := h.await(out).success(t)
	require.Equal(t, []int64{want.ID}, res.ids())
	require.Equal(t, want.ID, res.Cursor)
	require.Len(t, h.store.pollQueries(), 3, "one poll at the start and one every 2 seconds")
}

func TestWaitGivenCursorWinsOverShaLookback(t *testing.T) {
	t.Parallel()

	h := newWaitHarness(t)
	first := h.insert(repoID, "ci.run_completed", testShaA)
	second := h.insert(repoID, "ci.run_completed", testShaA)
	h.clock.Advance(time.Hour).MustWait(h.ctx)

	res := h.call(args("repo", testRepo, "types", allTypes, "sha", testShaA, "cursor", first.ID)).success(t)
	require.Equal(t, []int64{second.ID}, res.ids(), "an hour-old event after the cursor is returned despite the 10 minute lookback")
	require.Empty(t, h.store.lookups(), "a given cursor needs no lookup")
}

func TestWaitShaLooksBackTenMinutes(t *testing.T) {
	t.Parallel()

	h := newWaitHarness(t)
	h.insert(repoID, "ci.run_completed", testShaA)
	h.clock.Advance(6 * time.Minute).MustWait(h.ctx)
	recent := h.insert(repoID, "ci.run_completed", testShaA)
	h.clock.Advance(5 * time.Minute).MustWait(h.ctx)

	res := h.call(args("repo", testRepo, "types", allTypes, "sha", testShaA)).success(t)
	require.Equal(t, []int64{recent.ID}, res.ids(), "the event from 11 minutes ago is outside the lookback, the one from 5 is inside")

	out := h.callAsync(h.ctx, args("repo", testRepo, "types", allTypes, "timeout_s", 2))
	h.step(2 * time.Second)
	require.Empty(t, h.await(out).success(t).Events, "without a sha there is no lookback")
}

func TestWaitDeadline(t *testing.T) {
	t.Parallel()

	h := newWaitHarness(t)
	h.insert(otherRepoID, "ci.run_completed", testShaA)
	head := h.insert(otherRepoID, "pr.closed", "")

	out := h.callAsync(h.ctx, args("repo", testRepo, "types", allTypes, "timeout_s", 5))
	h.step(2 * time.Second)
	h.step(2 * time.Second)
	h.step(time.Second)

	res := h.await(out).success(t)
	require.NotNil(t, res.Events)
	require.Empty(t, res.Events)
	require.Equal(t, head.ID, res.Cursor)

	queries := h.store.pollQueries()
	require.Len(t, queries, 4)
	require.Zero(t, queries[0].After)
	for _, q := range queries[1:] {
		require.Equal(t, head.ID, q.After, "each poll resumes from the cursor of the one before")
	}
}

func TestWaitTimeout(t *testing.T) {
	t.Parallel()

	for name, tt := range map[string]struct {
		extra   map[string]any
		seconds int
	}{
		"default is 45 seconds": {nil, 45},
		"explicit 1 second":     {args("timeout_s", 1), 1},
		"explicit 50 seconds":   {args("timeout_s", 50), 50},
		"explicit 7 seconds":    {args("timeout_s", 7), 7},
	} {
		h := newWaitHarness(t)
		a := args("repo", testRepo, "types", allTypes, "cursor", 0)
		for k, v := range tt.extra {
			a[k] = v
		}
		out := h.callAsync(h.ctx, a)
		for elapsed := 0; elapsed < tt.seconds; {
			d := min(2, tt.seconds-elapsed)
			h.step(time.Duration(d) * time.Second)
			elapsed += d
		}
		require.Empty(t, h.await(out).success(t).Events, name)
		require.Len(t, h.store.pollQueries(), (tt.seconds+1)/2+1, name)
	}
}

func TestWaitCursorResumesWhereTheLastWaitStopped(t *testing.T) {
	t.Parallel()

	h := newWaitHarness(t)
	h.insert(repoID, "ci.run_completed", testShaA)
	h.clock.Advance(time.Second).MustWait(h.ctx)

	out := h.callAsync(h.ctx, args("repo", testRepo, "types", allTypes, "timeout_s", 1))
	h.step(time.Second)
	idle := h.await(out).success(t)
	require.Empty(t, idle.Events)

	missed := h.insert(repoID, "pr.closed", "")
	res := h.call(args("repo", testRepo, "types", allTypes, "cursor", idle.Cursor)).success(t)
	require.Equal(t, []int64{missed.ID}, res.ids())
}

func TestWaitStopsWhenTheCallerCancels(t *testing.T) {
	t.Parallel()

	h := newWaitHarness(t)
	stops := h.clock.Trap().TimerStop()
	defer stops.Close()
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()

	out := h.callAsync(ctx, args("repo", testRepo, "types", allTypes, "timeout_s", 50))
	armed := h.timers.MustWait(h.ctx)
	armed.MustRelease(h.ctx)
	pollsBefore := len(h.store.pollQueries())

	cancel()
	stopped := stops.MustWait(h.ctx)
	stopped.MustRelease(h.ctx)
	require.Error(t, h.await(out).err)

	h.clock.Advance(time.Minute).MustWait(h.ctx)
	require.Len(t, h.store.pollQueries(), pollsBefore, "no poll after the cancel")
	_, pending := h.clock.Peek()
	require.False(t, pending, "no timer left behind")
}

func TestWaitRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	h := newWaitHarness(t)
	bad := map[string]map[string]any{
		"no repo":             args("types", allTypes),
		"empty repo":          args("repo", "", "types", allTypes),
		"repo without name":   args("repo", "octo", "types", allTypes),
		"repo with extra":     args("repo", "octo/repo/x", "types", allTypes),
		"repo with query":     args("repo", "octo/repo?x=1", "types", allTypes),
		"repo with dots":      args("repo", "octo/..", "types", allTypes),
		"repo with a url":     args("repo", "https://github.com/octo/repo", "types", allTypes),
		"no types":            args("repo", testRepo),
		"empty types":         args("repo", testRepo, "types", []string{}),
		"unknown type":        args("repo", testRepo, "types", []string{"ci.run_completed", "ci.nothing"}),
		"timeout over max":    args("repo", testRepo, "types", allTypes, "timeout_s", 51),
		"timeout zero":        args("repo", testRepo, "types", allTypes, "timeout_s", 0),
		"timeout negative":    args("repo", testRepo, "types", allTypes, "timeout_s", -1),
		"timeout not integer": args("repo", testRepo, "types", allTypes, "timeout_s", "soon"),
		"short sha":           args("repo", testRepo, "types", allTypes, "sha", "abc123"),
		"sha not hex":         args("repo", testRepo, "types", allTypes, "sha", strings.Repeat("z", 40)),
		"negative pr":         args("repo", testRepo, "types", allTypes, "pr", -1),
		"negative cursor":     args("repo", testRepo, "types", allTypes, "cursor", -1),
		"unknown argument":    args("repo", testRepo, "types", allTypes, "repository_id", 42),
	}
	for name, a := range bad {
		msg := h.call(a).failure(t)
		require.NotEmpty(t, msg, name)
	}
	require.Zero(t, h.authz.callCount(), "invalid input must not reach the authorizer")
	require.Empty(t, h.store.pollQueries())
	require.Empty(t, h.store.lookups())

	require.Contains(t, h.call(args("repo", testRepo, "types", []string{"nope"})).failure(t), "ci.run_completed", "the error names the valid types")
	require.Contains(t, h.call(args("repo", testRepo, "types", allTypes, "timeout_s", 51)).failure(t), "50")
}

func TestWaitDeniesWithoutRevealingWhetherARepoExists(t *testing.T) {
	t.Parallel()

	h := newWaitHarness(t)
	h.insert(otherRepoID, "ci.run_completed", testShaA)
	h.authz.repos = map[string]int64{testRepo: repoID}

	exists := h.call(args("repo", otherRepo, "types", allTypes, "cursor", 0)).failure(t)
	missing := h.call(args("repo", "octo/never-heard-of-it", "types", allTypes, "cursor", 0)).failure(t)
	require.Equal(t, exists, missing, "a repo the caller cannot see and one that does not exist look the same")
	require.Contains(t, exists, "not found or not accessible")

	require.Empty(t, h.store.pollQueries(), "a denied call reads no events")
	require.Empty(t, h.store.lookups())
	h.requireNoLeak(exists, missing)
}

func TestWaitDeniedResultHasNoEvents(t *testing.T) {
	t.Parallel()

	h := newWaitHarness(t)
	h.insert(otherRepoID, "ci.run_completed", testShaA)
	h.authz.repos = map[string]int64{}

	out := h.call(args("repo", otherRepo, "types", allTypes, "cursor", 0))
	require.NotEmpty(t, out.failure(t))
	require.Nil(t, out.result.StructuredContent)
}

func TestWaitDeniesWhenTheAuthorizerFails(t *testing.T) {
	t.Parallel()

	for name, tt := range map[string]struct {
		err  error
		want string
	}{
		"github down":    {xerrors.New("dial tcp 10.1.2.3: connection refused"), "could not verify repository access"},
		"token rejected": {faucet.ErrInvalidToken, "rejected the token"},
	} {
		h := newWaitHarness(t)
		h.insert(repoID, "ci.run_completed", testShaA)
		h.authz.err = tt.err

		msg := h.call(args("repo", testRepo, "types", allTypes, "cursor", 0)).failure(t)
		require.Contains(t, msg, tt.want, name)
		require.NotContains(t, msg, "10.1.2.3", name)
		require.Empty(t, h.store.pollQueries(), name)
	}
}

func TestWaitDeniesAnAuthorizerThatReturnsNoRepoID(t *testing.T) {
	t.Parallel()

	h := newWaitHarness(t)
	h.authz.repos = map[string]int64{testRepo: 0}
	msg := h.call(args("repo", testRepo, "types", allTypes, "cursor", 0)).failure(t)
	require.Contains(t, msg, "could not verify repository access")
	require.Empty(t, h.store.pollQueries())
}

func TestWaitDeniesWithoutAPrincipal(t *testing.T) {
	t.Parallel()

	h := newWaitHarness(t, func(d *faucet.Deps) {
		d.PrincipalFrom = func(context.Context) (faucet.Principal, error) {
			return faucet.Principal{}, xerrors.New("no session cookie for u-123")
		}
	})
	msg := h.call(args("repo", testRepo, "types", allTypes, "cursor", 0)).failure(t)
	require.Contains(t, msg, "authentication required")
	require.NotContains(t, msg, "u-123")
	require.Zero(t, h.authz.callCount())
	require.Empty(t, h.store.pollQueries())
}

func TestWaitHidesStoreErrors(t *testing.T) {
	t.Parallel()

	h := newWaitHarness(t)
	h.store.err = xerrors.New("pq: password authentication failed for user faucet_admin")

	msg := h.call(args("repo", testRepo, "types", allTypes)).failure(t)
	require.Contains(t, msg, "could not read events")
	require.NotContains(t, msg, "faucet_admin")
	require.Contains(t, h.logs.String(), "faucet_admin", "the operator still sees the cause in the log")

	msg = h.call(args("repo", testRepo, "types", allTypes, "cursor", 0)).failure(t)
	require.Contains(t, msg, "could not read events")
}

func TestWaitWithTheRealAuthorizer(t *testing.T) {
	t.Parallel()

	stub := newGitHubStub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/octo/repo":
			reply(http.StatusOK, `{"id": 42}`)(w, r)
		case "/repos/octo/hidden":
			reply(http.StatusNotFound, `{"message":"Not Found"}`)(w, r)
		case "/repos/octo/forbidden":
			reply(http.StatusForbidden, `{"message":"Forbidden"}`)(w, r)
		default:
			reply(http.StatusInternalServerError, `{}`)(w, r)
		}
	})
	h := newWaitHarness(t, func(d *faucet.Deps) {
		d.Authorizer = faucet.TokenAuthorizer{BaseURL: stub.URL, Client: stub.Client()}
	})
	ev := h.insert(repoID, "ci.run_completed", testShaA)
	h.insert(otherRepoID, "ci.run_completed", testShaA)

	res := h.call(args("repo", testRepo, "types", allTypes, "cursor", 0)).success(t)
	require.Equal(t, []int64{ev.ID}, res.ids())
	require.Equal(t, "Bearer "+testToken, <-stub.tokens)

	var denials []string
	for _, repo := range []string{"octo/hidden", "octo/forbidden"} {
		denials = append(denials, h.call(args("repo", repo, "types", allTypes, "cursor", 0)).failure(t))
	}
	require.Equal(t, denials[0], denials[1])
	require.Contains(t, denials[0], "not found or not accessible")
	h.requireNoLeak(denials...)

	msg := h.call(args("repo", "octo/broken", "types", allTypes, "cursor", 0)).failure(t)
	require.Contains(t, msg, "could not verify repository access")
	h.requireNoLeak(msg)
	require.Len(t, h.store.pollQueries(), 1, "only the allowed call read events")
}
