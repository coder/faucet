package faucet_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
)

// withRawStore turns raw event names on: the fake store is also the RawStore.
func withRawStore(d *faucet.Deps) {
	d.RawStore, d.RawEvents = d.Store.(faucet.RawStore), faucet.RawEventNames()
}

func rawSubscribeTo(name string, arguments map[string]any, url, secret string) *rawParams {
	return &rawParams{Name: name, Arguments: raw(arguments), Delivery: deliveryTo(url, secret)}
}

func (h *webhookHarness) unsubscribeRaw(name string, arguments map[string]any, url string) error {
	h.t.Helper()
	_, err := h.call(methodUnsubscribe, &rawParams{Name: name, Arguments: raw(arguments), Delivery: raw(map[string]any{"url": url})})
	return err
}

// storeRawEvent stores one raw event of another repository, for a lease of
// its own, and returns the raw ID. It moves the head without giving the
// subscriptions under test anything.
func storeRawEvent(t *testing.T, store *fakeStore, now time.Time, n uint64) int64 {
	t.Helper()
	ctx := context.Background()
	lease := faucet.RawLease{
		Key: faucet.NewSubscriptionKeyHash("someone", "", "github.fork", []byte(`{"repo":"other/repo"}`)), Principal: "someone",
		Event: "fork", Target: faucet.RepoTarget(otherRepoID), ExpiresAt: now.Add(time.Hour),
	}
	binary.BigEndian.PutUint64(lease.ID[8:], n)
	_, err := store.OpenRawLease(ctx, lease, now, faucet.RawLimits{Live: 100, PerTarget: 1000, PerPrincipalTarget: 1000})
	require.NoError(t, err)
	res, err := store.IngestRaw(ctx, faucet.RawDelivery{DeliveryID: fmt.Sprintf("head-%d", n), Event: "fork", RepoID: otherRepoID}, now,
		func([]faucet.RawInterest) faucet.RawPlan {
			return faucet.RawPlan{Body: []byte(`{}`), Unread: []faucet.RawLeaseID{lease.ID}}
		})
	require.NoError(t, err)
	require.True(t, res.Matched)
	return res.ID
}

// rawInterests returns the interests that a delivery of event and action for
// the test repository matches now.
func (h *webhookHarness) rawInterests(event, action string) []faucet.RawInterest {
	h.t.Helper()
	var got []faucet.RawInterest
	_, err := h.store.IngestRaw(h.ctx, faucet.RawDelivery{DeliveryID: "probe-" + event + "-" + action, Event: event, Action: action, RepoID: repoID}, h.clock.Now(),
		func(interests []faucet.RawInterest) faucet.RawPlan {
			got = interests
			return faucet.RawPlan{}
		})
	require.NoError(h.t, err)
	return got
}

func TestSubscribeToARawNameStoresASubscriptionThatTheEventLogNeverReads(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t, withRawStore)
	head := storeRawEvent(t, h.store, h.clock.Now(), 1)
	h.insert(repoID, "ci.run_completed", "")
	h.clock.Advance(time.Second)

	arguments := repoArgs("actions", []string{"opened", "closed", "opened"}, "fields", []string{"issue.title", "action"})
	got := h.subscribe(rawSubscribeTo("github.issues", arguments, callbackURL, newSecret(t, 32)))
	require.Equal(t, "complete", got.ResultType)
	require.False(t, got.Truncated)
	require.Equal(t, fmt.Sprintf("r1.%d", head), got.Cursor, "the position is the newest raw event, not one of the event log")
	require.Equal(t, h.clock.Now().Add(10*time.Minute).UTC().Format(time.RFC3339), got.RefreshBefore)

	rows := h.rows()
	require.Len(t, rows, 1)
	row := rows[0]
	require.Equal(t, "octocat", row.Principal)
	require.Equal(t, callbackURL, row.CallbackURL)
	require.Equal(t, "github.issues", row.EventType)
	require.Equal(t, []string{"github.issues"}, row.EventTypes)
	require.Equal(t, `{"repo":"octo/repo","actions":["closed","opened"],"fields":["action","issue.title"]}`, string(row.Arguments),
		"the arguments are stored in their one spelling")
	require.Equal(t, faucet.RepoTarget(repoID), row.Target)
	require.Equal(t, got.ID, row.KeyHash.ID())
	require.EqualValues(t, math.MaxInt64, row.Cursor, "the delivery queue of the event log never reaches this position")
	require.Equal(t, 1, h.verifier.callCount())

	targets, err := h.store.ScanTargets(h.ctx, h.clock.Now(), 100, 100)
	require.NoError(t, err)
	require.Empty(t, targets, "the event log has an event of the repository, and still does not scan the raw subscription")

	interests := h.rawInterests("issues", "opened")
	require.Len(t, interests, 1, "a delivery of the event and a listed action matches the subscription")
	require.Equal(t, row.KeyHash, interests[0].Subscription)
	require.Equal(t, []string{"action", "issue.title"}, interests[0].Fields)
	require.Empty(t, h.rawInterests("issues", "edited"), "an action that the subscription did not list")
	require.Empty(t, h.rawInterests("push", ""), "another event")
}

func TestSubscribeToARawNameAddressesOneSubscriptionByAnySpellingOfItsArguments(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t, withRawStore)
	secret := newSecret(t, 32)
	first := h.subscribe(rawSubscribeTo("github.issues", repoArgs("actions", []string{"opened", "closed"}), callbackURL, secret))
	h.clock.Advance(time.Minute)

	res, err := h.call(methodSubscribe, rawSubscribeTo("github.issues", repoArgs("actions", []string{"closed", "opened", "closed"}), callbackURL, secret))
	require.NoError(t, err)
	renewed := decode[subscribed](t, res)
	require.Equal(t, first.ID, renewed.ID)
	require.Len(t, h.rows(), 1)
	require.Contains(t, res.fields, "deliveryStatus", "a renewal reports how delivery went")

	other := h.subscribe(rawSubscribeTo("github.issues", repoArgs("actions", []string{"closed"}), callbackURL, secret))
	require.NotEqual(t, first.ID, other.ID, "other actions are another subscription")
	require.Len(t, h.rows(), 2)
}

func TestSubscribeToARawNameSaysTruncatedOnlyForANewSubscriptionWithACursor(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t, withRawStore)
	secret := newSecret(t, 32)
	withCursor := func(url, cursor string) *rawParams {
		p := rawSubscribeTo("github.push", repoArgs(), url, secret)
		p.Cursor = raw(cursor)
		return p
	}

	fresh := h.subscribe(withCursor(callbackURL, "r1.0"))
	require.True(t, fresh.Truncated, "no raw event is kept for a subscription before it exists")
	require.Equal(t, "r1.0", fresh.Cursor)

	h.clock.Advance(time.Minute)
	renewed := h.subscribe(withCursor(callbackURL, "r1.0"))
	require.False(t, renewed.Truncated, "a live subscription missed nothing")
	require.Equal(t, fresh.ID, renewed.ID)

	fromPoll := h.subscribe(withCursor(callbackURL+"/2", "r1.7.000102030405060708090a0b0c0d0e0f"))
	require.True(t, fromPoll.Truncated, "a poll cursor is a position too")

	lapsed := h.subscribe(rawSubscribeTo("github.push", repoArgs(), callbackURL+"/3", secret))
	h.clock.Advance(11 * time.Minute)
	again := h.subscribe(withCursor(callbackURL+"/3", lapsed.Cursor))
	require.True(t, again.Truncated, "a subscription that lapsed is new again")
}

func TestSubscribeToARawNameRefusesWhatItCannotServeAndStoresNothing(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t, withRawStore)
	secret := newSecret(t, 32)
	params := func(arguments map[string]any, change func(*rawParams)) *rawParams {
		p := rawSubscribeTo("github.push", arguments, callbackURL, secret)
		if change != nil {
			change(p)
		}
		return p
	}
	for _, tc := range []struct {
		name    string
		params  *rawParams
		code    int64
		message string
	}{
		{name: "a typed cursor", params: params(repoArgs(), func(p *rawParams) { p.Cursor = raw("v1.5") }), code: codeInvalidParams, message: "cursor is not one this server returned"},
		{name: "a cursor that is no string", params: params(repoArgs(), func(p *rawParams) { p.Cursor = raw(5) }), code: codeInvalidParams, message: "cursor must be a string"},
		{name: "an argument of the typed names", params: params(repoArgs("sha", strings.Repeat("a", 40)), nil), code: codeInvalidParams},
		{name: "a pull request number", params: params(repoArgs("pr", 7), nil), code: codeInvalidParams},
		{name: "empty actions", params: params(repoArgs("actions", []string{}), nil), code: codeInvalidParams},
		{name: "a field path with a wildcard", params: params(repoArgs("fields", []string{"commits.*"}), nil), code: codeInvalidParams},
		{name: "no repository", params: params(map[string]any{}, nil), code: codeInvalidParams},
		{name: "maxAgeMs of zero", params: params(repoArgs(), func(p *rawParams) { p.MaxAgeMs = raw(0) }), code: codeInvalidParams, message: "maxAgeMs must be positive"},
		{name: "a repository that the caller cannot read", params: params(args("repo", deniedRepo), nil), code: codeForbidden},
	} {
		rpcErr := h.subscribeError(tc.params)
		require.EqualValues(t, tc.code, rpcErr.Code, tc.name)
		if tc.message != "" {
			require.Equal(t, tc.message, rpcErr.Message, tc.name)
		}
		require.NotContains(t, rpcErr.Message, secret, tc.name)
		requireNothingStored(t, h, tc.name)
	}
	require.Zero(t, h.verifier.callCount(), "no refused call reaches the callback")
}

func TestARawNameNeedsTheScopeOfItsMethod(t *testing.T) {
	t.Parallel()

	reader := newWebhookHarness(t, withRawStore, serviceWith(t, faucet.ScopeEventsRead))
	require.Empty(t, reader.poll(pollParams("github.push", repoArgs(), nil)).Events, "events:read covers a poll of a raw name")
	calls := reader.authz.callCount()
	rpcErr := reader.subscribeError(rawSubscribeTo("github.push", repoArgs(), callbackURL, newSecret(t, 32)))
	require.EqualValues(t, codeForbidden, rpcErr.Code)
	require.Contains(t, rpcErr.Message, "events:subscribe")
	var unsubErr interface{ Error() string }
	require.ErrorAs(t, reader.unsubscribeRaw("github.push", repoArgs(), callbackURL), &unsubErr)
	require.Contains(t, unsubErr.Error(), "events:subscribe")
	requireNothingStored(t, reader, "a token without events:subscribe stores no subscription")
	require.Equal(t, calls, reader.authz.callCount(), "a missing scope is refused before any repository lookup")

	subscriber := newWebhookHarness(t, withRawStore, serviceWith(t, faucet.ScopeEventsSubscribe))
	subscriber.subscribe(rawSubscribeTo("github.push", repoArgs(), callbackURL, newSecret(t, 32)))
	require.NoError(t, subscriber.unsubscribeRaw("github.push", repoArgs(), callbackURL))
	rpcErr = subscriber.pollError(pollParams("github.push", repoArgs(), nil))
	require.EqualValues(t, codeForbidden, rpcErr.Code)
	require.Contains(t, rpcErr.Message, "events:read")
}

func TestRawAndTypedSubscriptionsShareTheLimitOfAPrincipal(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t, withRawStore)
	secret := newSecret(t, 32)
	events := faucet.RawEventNames()
	for i := range 100 {
		url := fmt.Sprintf("%s/%d", callbackURL, i)
		if i%2 == 0 {
			h.subscribe(subscribeTo(repoArgs(), url, secret))
			continue
		}
		h.subscribe(rawSubscribeTo("github."+events[i%len(events)], repoArgs(), url, secret))
	}

	rpcErr := h.subscribeError(rawSubscribeTo("github.push", repoArgs(), callbackURL+"/one-more", secret))
	require.EqualValues(t, codeResourceExhausted, rpcErr.Code)
	require.Len(t, h.rows(), 100)
}

func TestUnsubscribeFromARawNameRemovesOnlyTheCallersLiveSubscription(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t, withRawStore)
	secret := newSecret(t, 32)
	h.subscribe(rawSubscribeTo("github.push", repoArgs(), callbackURL, secret))
	h.subscribe(rawSubscribeTo("github.push", repoArgs("fields", []string{"ref"}), callbackURL, secret))

	requireNotFound := func(err error, why string) {
		t.Helper()
		require.Error(t, err, why)
		require.Contains(t, err.Error(), "no such subscription", why)
	}
	h.as("mallory")
	requireNotFound(h.unsubscribeRaw("github.push", repoArgs(), callbackURL), "another login's subscription")
	h.as("octocat")
	requireNotFound(h.unsubscribeRaw("github.issues", repoArgs(), callbackURL), "another event")
	requireNotFound(h.unsubscribeRaw("github.push", repoArgs(), callbackURL+"/other"), "another callback")
	require.Len(t, h.rows(), 2)

	require.NoError(t, h.unsubscribeRaw("github.push", repoArgs(), callbackURL))
	rows := h.rows()
	require.Len(t, rows, 1, "the subscription with fields is another one")
	require.JSONEq(t, `{"repo":"octo/repo","fields":["ref"]}`, string(rows[0].Arguments))
	requireNotFound(h.unsubscribeRaw("github.push", repoArgs(), callbackURL), "it is gone")

	h.clock.Advance(11 * time.Minute)
	requireNotFound(h.unsubscribeRaw("github.push", repoArgs("fields", []string{"ref"}), callbackURL), "an expired subscription")
}

func TestRawNamesAreKnownOnlyToAServiceWithARawStore(t *testing.T) {
	t.Parallel()

	off := newWebhookHarness(t)
	rpcErr := off.subscribeError(rawSubscribeTo("github.push", repoArgs(), callbackURL, newSecret(t, 32)))
	require.EqualValues(t, codeNotFound, rpcErr.Code)
	require.Contains(t, rpcErr.Message, "ci.run_completed")
	require.NotContains(t, rpcErr.Message, "github.star", "a service without raw events lists none")
	require.Error(t, off.unsubscribeRaw("github.push", repoArgs(), callbackURL))

	on := newWebhookHarness(t, withRawStore)
	rpcErr = on.subscribeError(rawSubscribeTo("github.nope", repoArgs(), callbackURL, newSecret(t, 32)))
	require.EqualValues(t, codeNotFound, rpcErr.Code)
	require.Contains(t, rpcErr.Message, "ci.run_completed")
	require.Contains(t, rpcErr.Message, "github.push", "the answer lists the raw names after the typed ones")
	err := on.unsubscribeRaw("github.nope", repoArgs(), callbackURL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "github.push")

	noWebhook := newEventsHarness(t, withRawStore)
	_, err = noWebhook.call(methodSubscribe, rawSubscribeTo("github.push", repoArgs(), callbackURL, newSecret(t, 32)))
	require.Error(t, err)
	require.Contains(t, err.Error(), "webhook delivery is not supported")
}

func TestRenewalOfARawSubscriptionAfterRevokedAccessEndsItWithTheTerminatedEnvelope(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t, withRawStore)
	ep := d.endpoint("/hook")
	sub := d.subscribe(rawSubscribeTo("github.push", ep.args, d.rcv.url(ep.path), ep.secret))
	require.Len(t, d.rows(), 1)
	d.revoke()

	rpcErr := d.subscribeError(rawSubscribeTo("github.push", ep.args, d.rcv.url(ep.path), newSecret(t, 32)))
	require.EqualValues(t, codeForbidden, rpcErr.Code)

	requests := d.rcv.requests("/hook")
	require.Len(t, requests, 1)
	d.requireEnvelope(requests[0], "terminated", ep.secret, sub.ID)
	require.JSONEq(t, revokedEnvelope, string(requests[0].Body))
	require.Empty(t, d.rows(), "the subscription is gone")
	require.Equal(t, faucet.Terminations{Sent: 1}, d.svc.Terminations())
}

// cachingAuthorizer answers RepoID from what it saw last, as a cache does,
// and asks inner on every RepoIDFresh.
type cachingAuthorizer struct {
	inner *fakeAuthorizer
	mu    sync.Mutex
	seen  map[string]int64
}

func (a *cachingAuthorizer) RepoID(ctx context.Context, p faucet.Principal, repo string) (int64, error) {
	a.mu.Lock()
	id, ok := a.seen[repo]
	a.mu.Unlock()
	if ok {
		return id, nil
	}
	return a.RepoIDFresh(ctx, p, repo)
}

func (a *cachingAuthorizer) RepoIDFresh(ctx context.Context, p faucet.Principal, repo string) (int64, error) {
	id, err := a.inner.RepoID(ctx, p, repo)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err == nil {
		a.seen[repo] = id
	} else {
		delete(a.seen, repo)
	}
	return id, err
}

func TestSubscribeToARawNameAsksGitHubAndNotTheCache(t *testing.T) {
	t.Parallel()

	inner := &fakeAuthorizer{repos: map[string]int64{testRepo: repoID}}
	h := newRawPollHarness(t, func(d *faucet.Deps) { d.Authorizer = &cachingAuthorizer{inner: inner, seen: map[string]int64{}} })
	h.open()
	inner.mu.Lock()
	delete(inner.repos, testRepo)
	inner.mu.Unlock()
	h.open()

	rpcErr := h.subscribeError(rawSubscribeTo("github.push", repoArgs(), callbackURL, newSecret(t, 32)))
	require.EqualValues(t, codeForbidden, rpcErr.Code, "a cache that still holds the access does not make a subscription")
	require.Empty(t, h.rows())
}
