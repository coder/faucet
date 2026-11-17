package faucet_test

import (
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
func (h *rawPollHarness) deliver(event, delivery string, body []byte) string {
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

func (h *rawPollHarness) allow(repo string, id int64) {
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
