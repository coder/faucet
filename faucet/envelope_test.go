package faucet_test

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
)

// requireEnvelope checks what every control envelope owes the receiver: a
// signature that the subscriber's secret verifies, a per-message webhook-id of
// the form msg_<kind>_<random>, the subscription's ID and the time. It returns
// the members of the body.
func (d *deliverHarness) requireEnvelope(g hookRequest, kind, secret, subscriptionID string) map[string]json.RawMessage {
	d.t.Helper()
	require.True(d.t, g.signatureValid(secret), "the envelope is signed with the subscriber's secret")
	require.Regexp(d.t, regexp.MustCompile(`^msg_`+kind+`_[A-Za-z0-9_-]{22}$`), g.messageID())
	require.Equal(d.t, subscriptionID, g.Header.Get("X-MCP-Subscription-Id"))
	require.Equal(d.t, "application/json", g.Header.Get("Content-Type"))
	require.Equal(d.t, strconv.FormatInt(d.clock.Now().Unix(), 10), g.Header.Get("webhook-timestamp"))
	var members map[string]json.RawMessage
	require.NoError(d.t, json.Unmarshal(g.Body, &members))
	require.JSONEq(d.t, strconv.Quote(kind), string(members["type"]))
	return members
}

func (d *deliverHarness) requireNoCredentialInLogs(ep hookEndpoint) {
	d.t.Helper()
	for _, needle := range []string{ep.secret, ep.secret[len("whsec_"):], d.rcv.url(ep.path)} {
		require.NotContains(d.t, d.logs.String(), needle, "the log holds neither the secret nor the callback URL")
	}
}

// purgedBeforeRead subscribes at path, stores two events that a purge deletes
// before any run reads them, and one that survives it. It returns the highest
// purged event and the survivor.
func (d *deliverHarness) purgedBeforeRead(ep hookEndpoint) (sub subscribed, purged, survivor faucet.Event) {
	d.t.Helper()
	sub = d.subscribeEndpoint(ep)
	d.advance(time.Second)
	d.newRunEvent()
	purged = d.newRunEvent()
	d.advance(5 * time.Minute)
	survivor = d.newRunEvent()
	res, err := d.store.Purge(d.ctx, d.clock.Now().Add(-time.Minute))
	require.NoError(d.t, err)
	require.EqualValues(d.t, 2, res.Deleted)
	return sub, purged, survivor
}

func TestDeliverDueSendsOneSignedGapEnvelopeThatResumesWithoutLosingEvents(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	sub, purged, survivor := d.purgedBeforeRead(ep)

	res := d.deliverDue()
	require.Equal(t, 1, res.Gaps)
	require.Equal(t, 1, res.GapsSent)
	require.Zero(t, res.GapsFailed)
	require.Equal(t, 1, res.Delivered, "the event that survived the purge is still delivered")

	requests := d.rcv.requests("/hook")
	require.Len(t, requests, 2, "one gap envelope and one event")
	members := d.requireEnvelope(requests[0], "gap", ep.secret, sub.ID)
	require.Len(t, members, 2, "a gap body has a type and a cursor and nothing else")
	var cursor string
	require.NoError(t, json.Unmarshal(members["cursor"], &cursor))
	require.Equal(t, "v1."+strconv.FormatInt(purged.ID, 10), cursor, "the cursor is the purge mark, the oldest position that still has events above it")

	resumed := d.poll(pollParams("ci.run_completed", repoArgs(), cursorOf(cursor)))
	require.False(t, resumed.Truncated, "the gap cursor is a position the server can serve from")
	require.Equal(t, []string{survivor.DeliveryID}, resumed.eventIDs(), "resuming from the gap cursor replays every event that survived")

	d.newRunEvent()
	again := d.deliverDue()
	require.Zero(t, again.Gaps)
	require.Equal(t, 1, again.Delivered)
	require.Len(t, d.rcv.requests("/hook"), 3, "a gap is sent once")
	d.requireNoCredentialInLogs(ep)
}

func TestDeliverDueCountsAGapEnvelopeThatDidNotArrive(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	d.rcv.answer("/hook", func(n int) int {
		if n == 1 {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	})
	d.purgedBeforeRead(ep)

	res := d.deliverDue()
	require.Equal(t, 1, res.Gaps)
	require.Zero(t, res.GapsSent)
	require.Equal(t, 1, res.GapsFailed, "a failed gap envelope is counted")
	require.Equal(t, 1, res.Delivered, "later events keep flowing")
	require.True(t, d.logsContain("http_5xx"), "the log names the category of the failed send")

	d.newRunEvent()
	d.deliverDue()
	require.Len(t, d.rcv.requests("/hook"), 3, "a failed gap envelope is not retried: it was one gap and one attempt")
	d.requireNoCredentialInLogs(ep)
}

type rivalStore struct {
	faucet.Store
	t *testing.T
}

func (s rivalStore) EnqueueDeliveries(ctx context.Context, now time.Time, e faucet.Enqueue) (bool, error) {
	s.t.Helper()
	won, err := s.Store.EnqueueDeliveries(ctx, now, faucet.Enqueue{Key: e.Key, ExpectedScannedThrough: e.ExpectedScannedThrough, NewScannedThrough: e.NewScannedThrough})
	require.NoError(s.t, err)
	require.True(s.t, won, "the rival run takes the subscription first")
	return s.Store.EnqueueDeliveries(ctx, now, e)
}

func TestDeliverDueSendsNoGapEnvelopeWhenAnotherRunTookTheSubscription(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t, func(deps *faucet.Deps) {
		deps.Store = rivalStore{Store: deps.Store, t: t}
	})
	d.ledgerSkip = true
	d.purgedBeforeRead(d.endpoint("/hook"))

	res := d.deliverDue()
	require.Zero(t, res.Gaps, "the run that lost the enqueue reports no gap: the winner does")
	require.Zero(t, res.GapsSent)
	require.Zero(t, d.rcv.total(), "the run that lost the enqueue sends nothing")
}

const revokedEnvelope = `{"type":"terminated","error":{"code":-32024,"message":"Forbidden","data":{"reason":"Access revoked"}}}`

func (d *deliverHarness) revoke() {
	d.t.Helper()
	d.authz.mu.Lock()
	delete(d.authz.repos, testRepo)
	d.authz.mu.Unlock()
}

func (d *deliverHarness) renewalParams(ep hookEndpoint, secret string) *rawParams {
	return subscribeTo(ep.args, d.rcv.url(ep.path), secret)
}

func TestRenewalAfterRevokedAccessSendsOneSignedTerminatedEnvelopeAndEndsTheSubscription(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	sub := d.subscribeEndpoint(ep)
	other := d.subscribeEndpoint(d.endpoint("/other"))
	require.Len(t, d.rows(), 2)
	d.revoke()
	renewalSecret := newSecret(t, 32)

	rpcErr := d.subscribeError(d.renewalParams(ep, renewalSecret))
	require.EqualValues(t, -32024, rpcErr.Code, "the caller still gets the Forbidden of a denied subscribe")

	requests := d.rcv.requests("/hook")
	require.Len(t, requests, 1)
	d.requireEnvelope(requests[0], "terminated", ep.secret, sub.ID)
	require.JSONEq(t, revokedEnvelope, string(requests[0].Body))
	require.False(t, requests[0].signatureValid(renewalSecret), "the stored secret signs, not the one that the renewal brought")
	require.Equal(t, 1, d.rcv.total(), "the other subscription hears nothing")
	require.Len(t, d.rows(), 1, "the subscription is gone")
	require.Equal(t, other.ID, d.rows()[0].KeyHash.ID())
	require.Equal(t, faucet.Terminations{Sent: 1}, d.svc.Terminations())

	again := d.subscribeError(d.renewalParams(ep, renewalSecret))
	require.EqualValues(t, -32024, again.Code)
	require.Equal(t, 1, d.rcv.total(), "the subscription no longer exists, so a second renewal sends nothing")
	require.Equal(t, faucet.Terminations{Sent: 1}, d.svc.Terminations())
	d.requireNoCredentialInLogs(ep)
}

func TestRenewalThatStillHasAccessSendsNothing(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	d.subscribeEndpoint(ep)
	d.subscribeEndpoint(ep)

	require.Zero(t, d.rcv.total())
	require.Len(t, d.rows(), 1)
	require.Zero(t, d.svc.Terminations())
}

func TestDeniedFirstSubscribeSendsNothing(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	d.revoke()

	require.EqualValues(t, -32024, d.subscribeError(d.renewalParams(ep, ep.secret)).Code)
	require.Zero(t, d.rcv.total(), "there was no subscription to end")
	require.Zero(t, d.svc.Terminations())
}

func TestRenewalThatCouldNotCheckAccessKeepsTheSubscription(t *testing.T) {
	t.Parallel()

	for name, err := range map[string]error{
		"GitHub rejected the token": faucet.ErrInvalidToken,
		"GitHub could not answer":   xerrors.New("GitHub is down"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := newDeliverHarness(t)
			ep := d.endpoint("/hook")
			d.subscribeEndpoint(ep)
			d.authz.err = err

			d.subscribeError(d.renewalParams(ep, ep.secret))
			require.Zero(t, d.rcv.total(), "only a denial ends a subscription")
			require.Len(t, d.rows(), 1)
		})
	}
}

func TestTerminatedEnvelopeThatDidNotArriveIsCountedAndTheSubscriptionIsStillEnded(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	d.subscribeEndpoint(ep)
	d.rcv.answer("/hook", always(http.StatusInternalServerError))
	d.revoke()

	require.EqualValues(t, -32024, d.subscribeError(d.renewalParams(ep, ep.secret)).Code)
	require.Len(t, d.rcv.requests("/hook"), 1, "one attempt")
	require.Equal(t, faucet.Terminations{Failed: 1}, d.svc.Terminations())
	require.Empty(t, d.rows())
	require.True(t, d.logsContain("http_5xx"))

	d.subscribeError(d.renewalParams(ep, ep.secret))
	require.Len(t, d.rcv.requests("/hook"), 1, "a failed envelope is not retried")
	d.requireNoCredentialInLogs(ep)
}

func TestConcurrentRenewalsAfterRevokedAccessSendOneTerminatedEnvelope(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	d.subscribeEndpoint(ep)
	d.revoke()

	const renewals = 8
	var wg sync.WaitGroup
	errs := make([]error, renewals)
	for i := range renewals {
		wg.Go(func() { _, errs[i] = d.call(methodSubscribe, d.renewalParams(ep, ep.secret)) })
	}
	wg.Wait()

	for _, err := range errs {
		require.Error(t, err)
	}
	require.Len(t, d.rcv.requests("/hook"), 1, "the one renewal whose delete removed the row sent it")
	require.Equal(t, faucet.Terminations{Sent: 1}, d.svc.Terminations())
}

func TestRenewalDuringAGitHubRateLimitKeepsTheSubscriptionAndARealDenialStillEndsIt(t *testing.T) {
	t.Parallel()

	var answer atomic.Pointer[http.HandlerFunc]
	serve := func(status int, body string, headers map[string]string) {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for name, value := range headers {
				w.Header().Set(name, value)
			}
			reply(status, body)(w, r)
		})
		answer.Store(&h)
	}
	stub := newGitHubStub(t, func(w http.ResponseWriter, r *http.Request) { (*answer.Load())(w, r) })
	d := newDeliverHarness(t, func(deps *faucet.Deps) {
		deps.Authorizer = faucet.TokenAuthorizer{BaseURL: stub.URL, Client: stub.Client()}
	})
	ep := d.endpoint("/hook")
	serve(http.StatusOK, `{"id": 42}`, nil)
	d.subscribeEndpoint(ep)

	for name, limited := range map[string]struct {
		status  int
		headers map[string]string
		body    string
	}{
		"primary":                {http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0"}, `{"message":"Forbidden"}`},
		"primary, 429":           {http.StatusTooManyRequests, map[string]string{"X-RateLimit-Remaining": "0"}, `{"message":"API rate limit exceeded"}`},
		"secondary, Retry-After": {http.StatusForbidden, map[string]string{"Retry-After": "60"}, `{"message":"Forbidden"}`},
		"secondary, message":     {http.StatusForbidden, nil, `{"message":"You have exceeded a secondary rate limit."}`},
	} {
		serve(limited.status, limited.body, limited.headers)
		rpcErr := d.subscribeError(d.renewalParams(ep, ep.secret))
		require.EqualValues(t, jsonrpc.CodeInternalError, rpcErr.Code, "%s rate limit: a failed check is retryable, not Forbidden", name)
		require.Len(t, d.rows(), 1, "%s rate limit: the subscription stays", name)
		require.Zero(t, d.rcv.total(), "%s rate limit: nothing is sent", name)
	}
	require.Zero(t, d.svc.Terminations())

	serve(http.StatusForbidden, `{"message":"Resource not accessible by integration"}`, map[string]string{"X-RateLimit-Remaining": "4999"})
	require.EqualValues(t, -32024, d.subscribeError(d.renewalParams(ep, ep.secret)).Code)
	require.Len(t, d.rcv.requests("/hook"), 1, "a real denial still ends the subscription, once")
	require.Empty(t, d.rows())
	require.Equal(t, faucet.Terminations{Sent: 1}, d.svc.Terminations())
}

// scriptedStore lets a test act at two points of a DeliverDue run.
type scriptedStore struct {
	faucet.Store
	onExpire func()
	pollErr  func(call int) error

	mu    sync.Mutex
	polls int
}

func (s *scriptedStore) ExpireDeliveries(ctx context.Context, now time.Time) (int64, error) {
	if s.onExpire != nil {
		s.onExpire()
	}
	return s.Store.ExpireDeliveries(ctx, now)
}

func (s *scriptedStore) PollEvents(ctx context.Context, q faucet.PollQuery) (faucet.PollPage, error) {
	s.mu.Lock()
	s.polls++
	call := s.polls
	s.mu.Unlock()
	if s.pollErr != nil {
		if err := s.pollErr(call); err != nil {
			return faucet.PollPage{}, err
		}
	}
	return s.Store.PollEvents(ctx, q)
}

// gappedThenBusy is a subscription that a purge overtook, at slow, and after it
// a subscription of the other repository at fast, which has a new event. The
// run reads the one furthest behind first.
func (d *deliverHarness) gappedThenBusy(slow hookEndpoint) hookEndpoint {
	d.t.Helper()
	d.purgedBeforeRead(slow)
	fast := hookEndpoint{path: "/fast", secret: newSecret(d.t, 32), args: args("repo", otherRepo)}
	d.subscribeEndpoint(fast)
	d.insert(otherRepoID, "ci.run_completed", "")
	return fast
}

func TestDeliverDueGapEndpointThatStallsDoesNotDelayQueueingOrSendingOtherSubscriptions(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	slow := d.endpoint("/slow")
	started, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	d.rcv.answer("/slow", func(n int) int {
		if n == 1 {
			started <- struct{}{}
			<-release
		}
		return http.StatusOK
	})
	fast := d.gappedThenBusy(slow)

	type outcome struct {
		res faucet.DeliverResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := d.svc.DeliverDue(d.ctx)
		done <- outcome{res, err}
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the gap envelope was never sent")
	}
	require.Eventually(t, func() bool { return len(d.rcv.requests(fast.path)) == 1 }, 5*time.Second, 10*time.Millisecond,
		"while the gap endpoint stalls, another subscription's new event is queued and sent")

	once.Do(func() { close(release) })
	got := <-done
	require.NoError(t, got.err)
	d.record(got.res)
	require.Equal(t, 1, got.res.Gaps)
	require.Equal(t, 1, got.res.GapsSent)
	require.Equal(t, 2, got.res.Delivered, "the survivor of the gapped subscription and the new event of the other")
}

func TestDeliverDueCountsGapsThatItDidNotSend(t *testing.T) {
	t.Parallel()

	t.Run("the run ended before the send phase", func(t *testing.T) {
		t.Parallel()
		var d *deliverHarness
		d = newDeliverHarness(t, func(deps *faucet.Deps) {
			deps.Store = &scriptedStore{Store: deps.Store, onExpire: func() { d.advance(16 * time.Second) }}
		})
		d.clock.Set(time.Now()).MustWait(d.ctx)
		d.purgedBeforeRead(d.endpoint("/hook"))

		short, cancel := context.WithDeadline(context.Background(), d.clock.Now().Add(25*time.Second))
		defer cancel()
		res := d.deliverDueWith(short)
		require.Equal(t, 1, res.Gaps)
		require.Equal(t, 1, res.GapsFailed, "a gap that the run had no time to send is counted, not dropped")
		require.Zero(t, res.GapsSent)
		require.True(t, res.Incomplete)
		require.Zero(t, d.rcv.total())
		require.True(t, d.logsContain("the gap envelope was not sent"))
	})

	t.Run("the run failed after queueing it", func(t *testing.T) {
		t.Parallel()
		d := newDeliverHarness(t, func(deps *faucet.Deps) {
			deps.Store = &scriptedStore{Store: deps.Store, pollErr: func(call int) error {
				if call == 2 {
					return xerrors.New("database is down")
				}
				return nil
			}}
		})
		d.ledgerSkip = true
		d.gappedThenBusy(d.endpoint("/slow"))

		res, err := d.svc.DeliverDue(d.ctx)
		require.Error(t, err)
		require.Equal(t, 1, res.Gaps)
		require.Equal(t, 1, res.GapsFailed, "a gap queued before the failure is counted as not sent")
		require.Zero(t, d.rcv.total())
	})
}
