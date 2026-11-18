package faucet_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
)

// A field filter is matched once for each event that a read visits, so a read
// costs its lists times the events. These are the limits that keep a list
// small; a caller who goes past one gets an error that names it.
const (
	listLimit   = 32
	lengthLimit = 255
	tooManyText = "greater than 32"
	tooLongText = "more than 255"
)

func tooMany(prefix string) []string {
	values := make([]string, listLimit+1)
	for i := range values {
		values[i] = fmt.Sprintf("%s%02d", prefix, i)
	}
	return values
}

func tooLong() []string { return []string{strings.Repeat("a", lengthLimit+1)} }

// overLimit is a list argument past a limit, and the text that the error
// names the limit with.
type overLimit struct {
	name      string
	event     string
	arguments map[string]any
	text      string
}

func typedOverLimits() []overLimit {
	service := func(extra ...any) map[string]any { return args(append([]any{"owner", "octo"}, extra...)...) }
	return []overLimit{
		{"too many conclusions", "ci.runs", repoArgs("conclusion", tooMany("c")), tooManyText},
		{"a conclusion too long", "ci.runs", repoArgs("conclusion", tooLong()), tooLongText},
		{"too many head branches", "ci.runs", repoArgs("head_branch", tooMany("b")), tooManyText},
		{"a head branch too long", "ci.runs", repoArgs("head_branch", tooLong()), tooLongText},
		{"too many base branches", "pr.activity", repoArgs("base_branch", tooMany("b")), tooManyText},
		{"a base branch too long", "pr.activity", repoArgs("base_branch", tooLong()), tooLongText},
		{"too many actions", "ci.runs", repoArgs("actions", slices.Repeat([]string{"completed"}, listLimit+1)), tooManyText},
		{"too many repos", "ci.runs", service("repos", tooMany("octo/r")), tooManyText},
		{"a repo too long", "ci.runs", service("repos", []string{"octo/" + strings.Repeat("r", lengthLimit)}), tooLongText},
	}
}

func requireRefusedFor(t *testing.T, rpcErr *jsonrpc.Error, c overLimit, method string) {
	t.Helper()
	require.NotNil(t, rpcErr, "%s %s", method, c.name)
	require.EqualValues(t, jsonrpc.CodeInvalidParams, rpcErr.Code, "%s %s: %s", method, c.name, rpcErr.Message)
	require.True(t, strings.Contains(rpcErr.Message, c.text), "%s %s names the limit: %.300s", method, c.name, rpcErr.Message)
}

func TestEveryEventsMethodRefusesAListPastItsLimitBeforeItReadsOrStores(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	for _, c := range typedOverLimits() {
		requireRefusedFor(t, h.pollError(pollParams(c.event, c.arguments, cursorOf("v1.0"))), c, "poll")
		subscribe := &rawParams{Name: c.event, Arguments: raw(c.arguments), Delivery: deliveryTo(callbackURL, newSecret(t, 32))}
		requireRefusedFor(t, h.subscribeError(subscribe), c, "subscribe")
		unsubscribe := &rawParams{Name: c.event, Arguments: raw(c.arguments), Delivery: raw(map[string]any{"url": callbackURL})}
		requireRefusedFor(t, h.callError(methodUnsubscribe, unsubscribe), c, "unsubscribe")
	}
	require.Empty(t, h.store.pollQueries(), "no refused call reads events")
	require.Empty(t, h.rows(), "no refused call stores a subscription")
}

func TestEveryRawMethodRefusesAListPastItsLimitBeforeItOpensOrStores(t *testing.T) {
	t.Parallel()

	h := newRawPollHarness(t)
	for _, c := range []overLimit{
		{"too many actions", "github.push", repoArgs("actions", tooMany("a")), tooManyText},
		{"an action too long", "github.push", repoArgs("actions", tooLong()), tooLongText},
		{"too many fields", "github.push", repoArgs("fields", tooMany("f")), tooManyText},
	} {
		requireRefusedFor(t, h.pollError(pollParams(c.event, c.arguments, nil)), c, "poll")
		requireRefusedFor(t, h.subscribeError(rawSubscribeTo(c.event, c.arguments, callbackURL, newSecret(t, 32))), c, "subscribe")
		var rpcErr *jsonrpc.Error
		require.ErrorAs(t, h.unsubscribeRaw(c.event, c.arguments, callbackURL), &rpcErr, c.name)
		requireRefusedFor(t, rpcErr, c, "unsubscribe")
	}
	require.Empty(t, h.rawInterests("push", ""), "no refused call opens a lease or stores a subscription")
	require.Empty(t, h.rows())
}

// readsRefused fails every read of events and counts them, so a wait that
// reached the store returns at once instead of waiting out its timeout.
type readsRefused struct {
	faucet.Store
	reads atomic.Int32
}

func (s *readsRefused) PollEvents(context.Context, faucet.PollQuery) (faucet.PollPage, error) {
	s.reads.Add(1)
	return faucet.PollPage{}, xerrors.New("this test refuses every read")
}

func TestTheWaitToolRefusesAListPastItsLimitBeforeItReads(t *testing.T) {
	t.Parallel()

	store := &readsRefused{}
	h := newWaitHarness(t, func(d *faucet.Deps) { store.Store = d.Store; d.Store = store })
	wait := func(extra ...any) map[string]any {
		return repoArgs(append([]any{"types", []string{"ci.runs"}, "cursor", 0, "timeout_s", 1}, extra...)...)
	}
	for name, c := range map[string]struct {
		arguments map[string]any
		text      string
	}{
		"too many types":         {repoArgs("types", slices.Repeat([]string{"ci.runs"}, listLimit+1), "cursor", 0, "timeout_s", 1), tooManyText},
		"a type too long":        {repoArgs("types", tooLong(), "cursor", 0, "timeout_s", 1), tooLongText},
		"too many actions":       {wait("actions", slices.Repeat([]string{"completed"}, listLimit+1)), tooManyText},
		"too many conclusions":   {wait("conclusion", tooMany("c")), tooManyText},
		"a conclusion too long":  {wait("conclusion", tooLong()), tooLongText},
		"too many head branches": {wait("head_branch", tooMany("b")), tooManyText},
		"a head branch too long": {wait("head_branch", tooLong()), tooLongText},
		"too many base branches": {repoArgs("types", []string{"pr.activity"}, "cursor", 0, "timeout_s", 1, "base_branch", tooMany("b")), tooManyText},
	} {
		failure := h.call(c.arguments).failure(t)
		require.True(t, strings.Contains(failure, c.text), "%s names the limit: %.300s", name, failure)
	}
	require.Zero(t, store.reads.Load(), "no refused wait reads events")
	require.Empty(t, h.store.lookups(), "no refused wait looks up a cursor")
}

// boundedLists fails for each array in schema without maxItems, and each free
// string item of an array without maxLength. A string item with an enum, a
// pattern or a const has its own bound.
func boundedLists(t *testing.T, where string, schema any) {
	t.Helper()
	node, ok := schema.(map[string]any)
	if !ok {
		return
	}
	if hasType(node, "array") {
		require.Contains(t, node, "maxItems", "%s is a list without maxItems", where)
		if items, ok := node["items"].(map[string]any); ok && hasType(items, "string") {
			_, enum := items["enum"]
			_, pattern := items["pattern"]
			_, constant := items["const"]
			if !enum && !pattern && !constant {
				require.Contains(t, items, "maxLength", "%s is a list of strings without maxLength", where)
			}
		}
	}
	for name, child := range node {
		switch child := child.(type) {
		case map[string]any:
			boundedLists(t, where+"."+name, child)
		case []any:
			for i, c := range child {
				boundedLists(t, fmt.Sprintf("%s.%s[%d]", where, name, i), c)
			}
		}
	}
}

func hasType(node map[string]any, want string) bool {
	switch typ := node["type"].(type) {
	case string:
		return typ == want
	case []any:
		return slices.Contains(typ, any(want))
	}
	return false
}

func TestEveryAdvertisedListArgumentHasALimit(t *testing.T) {
	t.Parallel()

	h := newRawPollHarness(t)
	res, err := h.call(methodList, nil)
	require.NoError(t, err)
	var events []struct {
		Name        string         `json:"name"`
		InputSchema map[string]any `json:"inputSchema"`
	}
	require.NoError(t, json.Unmarshal(res.fields["events"], &events))
	require.True(t, slices.ContainsFunc(events, func(e struct {
		Name        string         `json:"name"`
		InputSchema map[string]any `json:"inputSchema"`
	}) bool {
		return e.Name == "github.push"
	}), "the list has the raw names")
	for _, e := range events {
		boundedLists(t, e.Name, e.InputSchema)
	}

	w := newWaitHarness(t)
	tools, err := w.session.ListTools(w.ctx, nil)
	require.NoError(t, err)
	require.Len(t, tools.Tools, 1)
	b, err := json.Marshal(tools.Tools[0].InputSchema)
	require.NoError(t, err)
	var input map[string]any
	require.NoError(t, json.Unmarshal(b, &input))
	boundedLists(t, tools.Tools[0].Name, input)
}

// A recorded workflow run passes filters at the limits: the bounds refuse no
// list that a real event can need.
func TestFiltersAtTheLimitsMatchARecordedEvent(t *testing.T) {
	t.Parallel()

	conclusions := tooMany("c")[:listLimit]
	conclusions[0] = "success"
	branches := []string{"master", strings.Repeat("b", lengthLimit)}

	h := newEventsHarness(t)
	start := h.headCursor(repoArgs())
	ev := storePayload(h.ctx, t, h.store, "workflow_run", fixture(t, "workflow_run_completed"), "recorded-run")
	got := h.poll(pollParams("ci.runs", repoArgs("conclusion", conclusions, "head_branch", branches), cursorOf(start)))
	require.Equal(t, []string{ev.DeliveryID}, got.eventIDs())

	w := newWaitHarness(t)
	waited := storePayload(w.ctx, t, w.store, "workflow_run", fixture(t, "workflow_run_completed"), "recorded-run")
	res := w.call(repoArgs("types", []string{"ci.runs"}, "cursor", 0, "conclusion", conclusions, "head_branch", branches)).success(t)
	require.Equal(t, []int64{waited.ID}, res.ids())
}
