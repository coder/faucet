package faucet_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

var rawPollCursor = regexp.MustCompile(`^r1\.(\d+)\.[0-9a-f]{32}$`)

// rawPollHarness polls raw names over MCP and stores raw events through the
// webhook handler, as GitHub and a poller reach the service.
type rawPollHarness struct {
	*webhookHarness
	deliveries int
}

func newRawPollHarness(t *testing.T, mutate ...func(*faucet.Deps)) *rawPollHarness {
	t.Helper()
	return &rawPollHarness{webhookHarness: newWebhookHarness(t, append([]func(*faucet.Deps){withRawStore}, mutate...)...)}
}

// deliver posts a signed delivery and returns the handler's answer.
func (h *webhookHarness) deliver(event, delivery string, body []byte) string {
	h.t.Helper()
	rec := httptest.NewRecorder()
	h.svc.WebhookHandler().ServeHTTP(rec, newRequest(http.MethodPost, body, event, delivery))
	return strings.TrimSpace(rec.Body.String())
}

// push stores a push of the test repository with extra members, and returns
// its delivery ID.
func (h *rawPollHarness) push(extra map[string]any) string {
	h.t.Helper()
	h.deliveries++
	delivery := fmt.Sprintf("push-%d", h.deliveries)
	require.Equal(h.t, "stored", h.deliver("push", delivery, rawPayload(repoID, "", extra)))
	return delivery
}

// pollRaw polls a raw name. An empty cursor means none.
func (h *rawPollHarness) pollRaw(name string, arguments map[string]any, cursor string) pollOutcome {
	h.t.Helper()
	params := pollParams(name, arguments, nil)
	if cursor != "" {
		params.Cursor = raw(cursor)
	}
	return h.poll(params)
}

// open starts a lease on the pushes of the test repository.
func (h *rawPollHarness) open(extra ...any) pollOutcome {
	h.t.Helper()
	opened := h.pollRaw("github.push", repoArgs(extra...), "")
	require.Empty(h.t, opened.Events, "the first poll only opens the lease")
	require.False(h.t, opened.Truncated)
	require.Regexp(h.t, rawPollCursor, opened.Cursor)
	return opened
}

func (h *webhookHarness) allow(repo string, id int64) {
	h.authz.mu.Lock()
	defer h.authz.mu.Unlock()
	h.authz.repos[repo] = id
}

func TestPollOfARawNameReturnsWhatArrivedSinceTheLeaseOpened(t *testing.T) {
	t.Parallel()

	h := newRawPollHarness(t)
	require.Equal(t, "ignored", h.deliver("push", "before", rawPayload(repoID, "", nil)), "no lease wants pushes yet")
	opened := h.open()
	require.Positive(t, opened.NextPollMs)

	h.clock.Advance(time.Second)
	first := h.push(map[string]any{"ref": "refs/heads/a"})
	h.clock.Advance(time.Second)
	second := h.push(map[string]any{"ref": "refs/heads/b"})

	page := h.pollRaw("github.push", repoArgs(), opened.Cursor)
	require.Equal(t, []string{first, second}, page.eventIDs(), "in the order they arrived, named by GitHub's delivery ID")
	require.False(t, page.Truncated)
	require.False(t, page.HasMore)
	require.Regexp(t, rawPollCursor, page.Cursor)
	for i, ref := range []string{"refs/heads/a", "refs/heads/b"} {
		ev := page.Events[i]
		require.Equal(t, "github.push", ev.Name)
		require.Equal(t, ref, ev.Data["ref"])
		require.Equal(t, map[string]any{"id": float64(repoID), "full_name": testRepo}, ev.Data["repository"], "the data is the payload")
		require.True(t, ev.Timestamp.Equal(h.clock.Now().Add(time.Duration(i-1)*time.Second)), "the time faucet received it, got %s", ev.Timestamp)
	}

	again := h.pollRaw("github.push", repoArgs(), opened.Cursor)
	require.Equal(t, page.eventIDs(), again.eventIDs(), "the same cursor reads the same events")
	require.False(t, again.Truncated)

	next := h.pollRaw("github.push", repoArgs(), page.Cursor)
	require.Empty(t, next.Events, "the next cursor acknowledges them")
	require.False(t, next.Truncated)

	stale := h.pollRaw("github.push", repoArgs(), opened.Cursor)
	require.Empty(t, stale.Events, "acknowledged events are gone")
	require.True(t, stale.Truncated, "a cursor from before an acknowledgement has lost them")
}

func TestPollOfARawNamePagesByEventsAndByBytes(t *testing.T) {
	t.Parallel()

	h := newRawPollHarness(t)
	opened := h.open()
	var all []string
	for i := range 5 {
		all = append(all, h.push(map[string]any{"n": i}))
	}

	limited := pollParams("github.push", repoArgs(), raw(opened.Cursor))
	limited.MaxEvents = raw(2)
	var got []string
	cursor := opened.Cursor
	for _, want := range []struct {
		events  int
		hasMore bool
	}{{2, true}, {2, true}, {1, false}} {
		limited.Cursor = raw(cursor)
		page := h.poll(limited)
		require.Len(t, page.Events, want.events)
		require.Equal(t, want.hasMore, page.HasMore)
		require.False(t, page.Truncated)
		got = append(got, page.eventIDs()...)
		cursor = page.Cursor
	}
	require.Equal(t, all, got)

	big := strings.Repeat("x", 200<<10)
	all = all[:0]
	for i := range 8 {
		all = append(all, h.push(map[string]any{"n": i, "blob": big}))
	}
	first := h.pollRaw("github.push", repoArgs(), cursor)
	require.Len(t, first.Events, 6, "a page ends after the event that takes its payloads to 1 MiB")
	require.True(t, first.HasMore)
	rest := h.pollRaw("github.push", repoArgs(), first.Cursor)
	require.Len(t, rest.Events, 2)
	require.False(t, rest.HasMore)
	require.Equal(t, all, append(first.eventIDs(), rest.eventIDs()...))
}

func TestPollOfARawNameOpensANewLeaseForACursorThatIsNotItsOwn(t *testing.T) {
	t.Parallel()

	h := newRawPollHarness(t)
	h.allow(otherRepo, otherRepoID)
	opened := h.open()
	h.push(nil)

	requireNewLease := func(got pollOutcome, why string) {
		t.Helper()
		require.Empty(t, got.Events, why)
		require.True(t, got.Truncated, why)
		require.Regexp(t, rawPollCursor, got.Cursor, why)
		require.NotEqual(t, opened.Cursor[strings.LastIndex(opened.Cursor, "."):], got.Cursor[strings.LastIndex(got.Cursor, "."):], why)
	}
	requireNewLease(h.pollRaw("github.push", repoArgs(), "r1.0"), "a position without a lease, as a subscribe result has")
	requireNewLease(h.pollRaw("github.push", repoArgs(), "r1.0.000102030405060708090a0b0c0d0e0f"), "a lease that does not exist")
	requireNewLease(h.pollRaw("github.push", repoArgs("fields", []string{"ref"}), opened.Cursor), "other arguments")
	requireNewLease(h.pollRaw("github.issues", repoArgs(), opened.Cursor), "another event name")
	requireNewLease(h.pollRaw("github.push", args("repo", otherRepo), opened.Cursor), "another repository")
	h.as("mallory")
	requireNewLease(h.pollRaw("github.push", repoArgs(), opened.Cursor), "another login")
	h.as("octocat")

	page := h.pollRaw("github.push", repoArgs(), opened.Cursor)
	require.Len(t, page.Events, 1, "none of them read or ended the lease")
	require.False(t, page.Truncated)

	for _, cursor := range []any{"v1.0", "r1.x", "r1.1.zz", 7} {
		params := pollParams("github.push", repoArgs(), raw(cursor))
		rpcErr := h.pollError(params)
		require.EqualValues(t, codeInvalidParams, rpcErr.Code, "cursor %v", cursor)
	}
	require.Equal(t, "cursor is not one this server returned", h.pollError(pollParams("github.push", repoArgs(), raw("v1.0"))).Message)
}

func TestALeaseOfARawNameLivesOneHourAfterEachPoll(t *testing.T) {
	t.Parallel()

	h := newRawPollHarness(t)
	opened := h.open()
	h.clock.Advance(59 * time.Minute)
	kept := h.pollRaw("github.push", repoArgs(), opened.Cursor)
	require.False(t, kept.Truncated, "a poll within the hour keeps the lease")
	h.clock.Advance(59 * time.Minute)
	h.push(nil)

	h.clock.Advance(time.Hour)
	require.Equal(t, "ignored", h.deliver("push", "after-the-lease", rawPayload(repoID, "", nil)), "an expired lease wants nothing")
	lapsed := h.pollRaw("github.push", repoArgs(), kept.Cursor)
	require.Empty(t, lapsed.Events)
	require.True(t, lapsed.Truncated, "the poller was away for over an hour and lost what came")

	fresh := h.push(nil)
	page := h.pollRaw("github.push", repoArgs(), lapsed.Cursor)
	require.Equal(t, []string{fresh}, page.eventIDs())
	require.False(t, page.Truncated)
}

func TestTheLeasesOfOneLoginAreLimitedAndTheOnePolledLongestAgoEnds(t *testing.T) {
	t.Parallel()

	h := newRawPollHarness(t)
	first := h.open()
	h.clock.Advance(time.Second)
	second := h.open("fields", []string{"ref"})
	events := faucet.RawEventNames()
	for i := range 98 {
		h.clock.Advance(time.Second)
		require.Regexp(t, rawPollCursor, h.pollRaw("github."+events[i%len(events)], repoArgs("fields", []string{"x"}), "").Cursor)
	}
	h.clock.Advance(time.Second)
	require.False(t, h.pollRaw("github.push", repoArgs(), first.Cursor).Truncated, "the login holds 100 leases, and the first is polled again")

	h.clock.Advance(time.Second)
	h.open()
	require.False(t, h.pollRaw("github.push", repoArgs(), first.Cursor).Truncated, "a lease that was polled since is kept")
	require.True(t, h.pollRaw("github.push", repoArgs("fields", []string{"ref"}), second.Cursor).Truncated,
		"one lease more ended the one polled longest ago")

	h.as("mallory")
	require.False(t, h.open().Truncated, "another login has leases of its own")
}

func TestPollOfARawNameSaysTruncatedForWhatTheLeaseMissed(t *testing.T) {
	t.Parallel()

	t.Run("an event older than maxAgeMs", func(t *testing.T) {
		t.Parallel()
		h := newRawPollHarness(t)
		opened := h.open()
		h.push(nil)
		h.clock.Advance(10 * time.Minute)
		fresh := h.push(nil)

		params := pollParams("github.push", repoArgs(), raw(opened.Cursor))
		params.MaxAgeMs = raw(60_000)
		page := h.poll(params)
		require.Equal(t, []string{fresh}, page.eventIDs())
		require.True(t, page.Truncated)
	})

	t.Run("a payload that is too large for a lease without fields", func(t *testing.T) {
		t.Parallel()
		h := newRawPollHarness(t)
		opened := h.open()
		h.push(map[string]any{"blob": strings.Repeat("x", 300<<10)})
		small := h.push(nil)

		page := h.pollRaw("github.push", repoArgs(), opened.Cursor)
		require.Equal(t, []string{small}, page.eventIDs())
		require.True(t, page.Truncated)
		require.False(t, h.pollRaw("github.push", repoArgs(), page.Cursor).Truncated, "a loss is reported once")
	})

	t.Run("a payload that fits once the lease asks for fields", func(t *testing.T) {
		t.Parallel()
		h := newRawPollHarness(t)
		opened := h.open("fields", []string{"ref"})
		big := h.push(map[string]any{"ref": "refs/heads/main", "blob": strings.Repeat("x", 300<<10)})

		page := h.pollRaw("github.push", repoArgs("fields", []string{"ref"}), opened.Cursor)
		require.Equal(t, []string{big}, page.eventIDs())
		require.Equal(t, map[string]any{"ref": "refs/heads/main"}, page.Events[0].Data)
		require.False(t, page.Truncated)
	})

	t.Run("a stored body that does not hold what the lease reads", func(t *testing.T) {
		t.Parallel()
		h := newRawPollHarness(t)
		opened := h.open()
		for i, plan := range []faucet.RawPlan{
			{Body: []byte(`{"ref":"refs/heads/main"}`), Fields: []string{"ref"}},
			{Body: []byte(`{"blob":"` + strings.Repeat("x", 300<<10) + `"}`)},
		} {
			_, err := h.store.IngestRaw(h.ctx, faucet.RawDelivery{DeliveryID: fmt.Sprintf("odd-%d", i), Event: "push", RepoID: repoID}, h.clock.Now(),
				func(interests []faucet.RawInterest) faucet.RawPlan {
					require.Len(t, interests, 1)
					plan.Unread = []faucet.RawLeaseID{interests[0].Lease}
					return plan
				})
			require.NoError(t, err)
		}
		good := h.push(nil)

		page := h.pollRaw("github.push", repoArgs(), opened.Cursor)
		require.Equal(t, []string{good}, page.eventIDs(), "an event that cannot be served whole is left out, never served in part")
		require.True(t, page.Truncated)
		require.Equal(t, 2, strings.Count(h.logs.String(), "skip raw event"), "each is logged as an error")
	})
}

func TestPollOfARawNameChecksTheCallerAndTheRepository(t *testing.T) {
	t.Parallel()

	h := newRawPollHarness(t)
	denied := h.pollError(pollParams("github.push", args("repo", deniedRepo), nil))
	require.EqualValues(t, codeForbidden, denied.Code)
	require.NotContains(t, denied.Message, deniedRepo)
	require.Equal(t, "ignored", h.deliver("push", "denied", rawPayload(deniedRepoID, "", nil)), "a refused poll opens no lease")

	anonymous := newRawPollHarness(t, func(d *faucet.Deps) { d.PrincipalFrom = failingPrincipal })
	rpcErr := anonymous.pollError(pollParams("github.push", repoArgs(), nil))
	require.Equal(t, "authentication required", rpcErr.Message)
}

// pollData polls and returns the data of each event as it came over the wire.
func (h *rawPollHarness) pollData(params *rawParams) []json.RawMessage {
	h.t.Helper()
	res, err := h.call(methodPoll, params)
	require.NoError(h.t, err)
	var events []struct {
		Data json.RawMessage `json:"data"`
	}
	require.NoError(h.t, json.Unmarshal(res.fields["events"], &events))
	data := make([]json.RawMessage, len(events))
	for i, ev := range events {
		data[i] = ev.Data
	}
	return data
}

func TestPollOfARawNameWithFieldsReturnsOnlyThoseMembers(t *testing.T) {
	t.Parallel()

	big := strings.Repeat("x", 200<<10)
	for _, tc := range []struct {
		name   string
		body   string
		fields []string
		want   string
	}{
		{"top level members", `{"b":1,"a":"x","c":true}`, []string{"c", "a"}, `{"a":"x","c":true}`},
		{"nested members keep their nesting", `{"head_commit":{"id":"abc","message":"m","author":{"name":"n","email":"e"}},"ref":"r"}`, []string{"head_commit.id", "head_commit.author.name", "ref"}, `{"head_commit":{"author":{"name":"n"},"id":"abc"},"ref":"r"}`},
		{"a shorter path wins over a longer one, given first", `{"h":{"id":"1", "m" : 2}}`, []string{"h", "h.id"}, `{"h":{"id":"1","m":2}}`},
		{"a shorter path wins over a longer one, given last", `{"h":{"id":"1", "m" : 2}}`, []string{"h.id", "h"}, `{"h":{"id":"1","m":2}}`},
		{"a segment is a whole name, not a prefix", `{"head":1,"head_commit":{"id":"1"}}`, []string{"head"}, `{"head":1}`},
		{"a path through an array applies to each element that is an object", `{"commits":[{"id":"1","x":2},{"x":3},5,"s",null,true,[{"id":"9"}],{"id":"2"}]}`, []string{"commits.id"}, `{"commits":[{"id":"1"},{},{"id":"2"}]}`},
		{"an array of objects where nothing matches keeps its elements", `{"a":[{"x":1},{"y":2}]}`, []string{"a.b"}, `{"a":[{},{}]}`},
		{"an array of scalars that a path continues through is left out", `{"a":[1,2],"k":1}`, []string{"a.b", "k"}, `{"k":1}`},
		{"an empty array that a path continues through is left out", `{"a":[]}`, []string{"a.b"}, `{}`},
		{"a whole array", `{"a":[1, 2 ,{"b":3}]}`, []string{"a"}, `{"a":[1,2,{"b":3}]}`},
		{"an array in an array is not entered", `{"a":[[{"b":1}]]}`, []string{"a.b"}, `{}`},
		{"an array inside an element of an array", `{"a":[{"b":[{"c":1,"d":2},{"d":3}]}]}`, []string{"a.b.c"}, `{"a":[{"b":[{"c":1},{}]}]}`},
		{"members that the payload does not have", `{"a":{"x":1}}`, []string{"nope", "a.nope", "a.x.y"}, `{}`},
		{"a path through a string, a number, a boolean or null", `{"s":"x","n":1,"b":true,"z":null}`, []string{"s.a", "n.a", "b.a", "z.a"}, `{}`},
		{"a null that is selected", `{"z":null}`, []string{"z"}, `{"z":null}`},
		{"an object that ends up empty is left out", `{"a":{},"b":{"c":1}}`, []string{"a.x", "b.c"}, `{"b":{"c":1}}`},
		{"an empty object that is selected", `{"a":{}}`, []string{"a"}, `{"a":{}}`},
		{"escapes and numbers as GitHub wrote them", `{"a":"\u0000<>&\u003c\"","b":1.50e+3,"c":"é","d":-0}`, []string{"a", "b", "c", "d"}, `{"a":"\u0000<>&\u003c\"","b":1.50e+3,"c":"é","d":-0}`},
		{"a member name that is repeated: the last one", `{"a":1,"a":2}`, []string{"a"}, `{"a":2}`},
		{"a member name written with an escape", `{"\u0069d":1}`, []string{"id"}, `{"id":1}`},
		{"a path that is given twice", `{"a":1}`, []string{"a", "a"}, `{"a":1}`},
		{"a value of 200 KiB", `{"big":"` + big + `","small":1}`, []string{"big"}, `{"big":"` + big + `"}`},
		{"a path of eight segments", `{"a":{"b":{"c":{"d":{"e":{"f":{"g":{"h":1,"i":2}}}}}}}}`, []string{"a.b.c.d.e.f.g.h"}, `{"a":{"b":{"c":{"d":{"e":{"f":{"g":{"h":1}}}}}}}}`},
		{"nothing selected", `{"a":1}`, []string{"b"}, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newRawPollHarness(t)
			opened := h.open("fields", tc.fields)
			body := `{"repository":{"id":42},` + tc.body[1:]
			require.Equal(t, "stored", h.deliver("push", "fields", []byte(body)))

			data := h.pollData(pollParams("github.push", repoArgs("fields", tc.fields), raw(opened.Cursor)))
			require.Len(t, data, 1)
			require.JSONEq(t, tc.want, string(data[0]))
		})
	}
}

func TestEachLeaseOfARawNameGetsWhatItAskedFor(t *testing.T) {
	t.Parallel()

	h := newRawPollHarness(t)
	payload := map[string]any{"ref": "refs/heads/main", "after": "abc", "commits": []any{map[string]any{"id": "1", "message": "m"}}}
	refs := h.open("fields", []string{"ref"})
	commits := h.open("fields", []string{"commits.id", "repository.id"})
	h.push(payload)

	require.Equal(t, map[string]any{"ref": "refs/heads/main"},
		h.pollRaw("github.push", repoArgs("fields", []string{"ref"}), refs.Cursor).Events[0].Data, "one lease's fields are not served to another")
	require.Equal(t, map[string]any{"commits": []any{map[string]any{"id": "1"}}, "repository": map[string]any{"id": float64(repoID)}},
		h.pollRaw("github.push", repoArgs("fields", []string{"commits.id", "repository.id"}), commits.Cursor).Events[0].Data)

	whole := h.open()
	h.push(payload)
	got := h.pollRaw("github.push", repoArgs(), whole.Cursor).Events[0].Data
	require.Equal(t, "abc", got["after"], "a lease without fields gets the whole payload")
	require.Equal(t, map[string]any{"id": float64(repoID), "full_name": testRepo}, got["repository"])
	page := h.pollRaw("github.push", repoArgs("fields", []string{"ref"}), refs.Cursor)
	require.Len(t, page.Events, 2)
	require.Equal(t, map[string]any{"ref": "refs/heads/main"}, page.Events[1].Data, "and a lease with fields still gets only its own")
}

func TestPollOfARawNameChecksItsArguments(t *testing.T) {
	t.Parallel()

	h := newRawPollHarness(t)
	many := make([]string, 33)
	for i := range many {
		many[i] = fmt.Sprintf("f%d", i)
	}
	for name, arguments := range map[string]map[string]any{
		"an argument of the typed names":  repoArgs("sha", strings.Repeat("a", 40)),
		"empty actions":                   repoArgs("actions", []string{}),
		"an empty action":                 repoArgs("actions", []string{"opened", ""}),
		"empty fields":                    repoArgs("fields", []string{}),
		"more than 32 fields":             repoArgs("fields", many),
		"a field path with an empty part": repoArgs("fields", []string{"a..b"}),
		"a field path with an index":      repoArgs("fields", []string{"commits[0]"}),
		"a field path of nine parts":      repoArgs("fields", []string{"a.b.c.d.e.f.g.h.i"}),
		"a repository without an owner":   args("repo", "repo"),
		"no repository":                   {},
	} {
		rpcErr := h.pollError(pollParams("github.push", arguments, nil))
		require.EqualValues(t, codeInvalidParams, rpcErr.Code, name)
	}
	for name, change := range map[string]func(*rawParams){
		"maxEvents of zero": func(p *rawParams) { p.MaxEvents = raw(0) },
		"maxAgeMs of zero":  func(p *rawParams) { p.MaxAgeMs = raw(0) },
	} {
		params := pollParams("github.push", repoArgs(), nil)
		change(params)
		require.EqualValues(t, codeInvalidParams, h.pollError(params).Code, name)
	}
	require.Equal(t, "ignored", h.deliver("push", "refused", rawPayload(repoID, "", nil)), "a refused poll opens no lease")

	opened := h.open("actions", []string{"b", "a"}, "fields", []string{"y", "x"})
	respelled := h.pollRaw("github.push", repoArgs("fields", []string{"x", "y", "x"}, "actions", []string{"a", "b"}), opened.Cursor)
	require.False(t, respelled.Truncated, "another spelling of the same arguments reads the same lease")
}

func TestPollKnowsRawNamesOnlyOnAServiceWithARawStore(t *testing.T) {
	t.Parallel()

	on := newRawPollHarness(t)
	rpcErr := on.pollError(pollParams("github.nope", repoArgs(), nil))
	require.EqualValues(t, codeNotFound, rpcErr.Code)
	require.Contains(t, rpcErr.Message, "ci.run_completed")
	require.Contains(t, rpcErr.Message, "github.push")
	require.Empty(t, on.poll(pollParams("ci.run_completed", repoArgs(), nil)).Events, "a typed name is polled as before")

	off := newEventsHarness(t)
	rpcErr = off.pollError(pollParams("github.push", repoArgs(), nil))
	require.EqualValues(t, codeNotFound, rpcErr.Code)
	require.Contains(t, rpcErr.Message, "ci.run_completed")
	require.NotContains(t, rpcErr.Message, "github.star")
}
