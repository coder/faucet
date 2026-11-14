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

// envelopes are the control envelopes of one type that the endpoint at path got.
func (r *hookReceiver) envelopesOf(path, kind string) []hookRequest {
	var out []hookRequest
	for _, g := range r.requests(path) {
		var typed struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(g.Body, &typed) == nil && typed.Type == kind {
			out = append(out, g)
		}
	}
	return out
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

	require.Len(t, d.rcv.requests("/hook"), 2, "one gap envelope and one event")
	gaps := d.rcv.envelopesOf("/hook", "gap")
	require.Len(t, gaps, 1)
	members := d.requireEnvelope(gaps[0], "gap", ep.secret, sub.ID)
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
	d.rcv.answerEnvelope("/hook", "gap", func() int { return http.StatusInternalServerError })
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
	won, err := s.Store.EnqueueDeliveries(ctx, now, faucet.Enqueue{Key: e.Key, ExpectedScannedThrough: e.ExpectedScannedThrough, NewScannedThrough: e.NewScannedThrough, GapThrough: e.GapThrough})
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
	require.Zero(t, res.Gaps, "the run that lost the enqueue queues no gap: the winner did")
	require.Equal(t, 1, res.GapsSent, "the gap that the winner recorded goes out")
	d.deliverDue()
	require.Len(t, d.rcv.envelopesOf("/hook", "gap"), 1, "one gap sends one envelope, whichever run takes it")
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
	clearErr error

	mu    sync.Mutex
	polls int
}

func (s *scriptedStore) ExpireDeliveries(ctx context.Context, now time.Time) (int64, error) {
	if s.onExpire != nil {
		s.onExpire()
	}
	return s.Store.ExpireDeliveries(ctx, now)
}

func (s *scriptedStore) ClearExpiredPreviousSecrets(ctx context.Context, now time.Time) (int64, error) {
	if s.clearErr != nil {
		return 0, s.clearErr
	}
	return s.Store.ClearExpiredPreviousSecrets(ctx, now)
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
	d.rcv.answerEnvelope("/slow", "gap", func() int {
		started <- struct{}{}
		<-release
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

func TestDeliverDueKeepsAGapThatItDidNotSendForTheNextRun(t *testing.T) {
	t.Parallel()

	requireSentOnce := func(t *testing.T, d *deliverHarness) {
		t.Helper()
		next := d.deliverDueWith(context.Background())
		require.Equal(t, 1, next.GapsSent, "the next run sends the gap that the first one queued")
		require.Zero(t, d.deliverDueWith(context.Background()).GapsSent)
		require.Len(t, d.rcv.envelopesOf("/hook", "gap"), 1, "and it sends it once")
	}

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
		require.Zero(t, res.GapsSent+res.GapsFailed, "the run had no time to send it")
		require.True(t, res.Incomplete)
		require.Zero(t, d.rcv.total())
		requireSentOnce(t, d)
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
		d.gappedThenBusy(d.endpoint("/hook"))

		res, err := d.svc.DeliverDue(d.ctx)
		require.Error(t, err)
		require.Equal(t, 1, res.Gaps)
		require.Zero(t, d.rcv.total())
		requireSentOnce(t, d)
	})
}

func TestDeliverDueLeavesAGapOwedWhenTheRunEndsDuringItsRequest(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	d.purgedBeforeRead(ep)
	ctx, cancel := context.WithCancel(d.ctx)
	defer cancel()
	d.rcv.answerEnvelope("/hook", "gap", func() int {
		cancel()
		time.Sleep(50 * time.Millisecond)
		return http.StatusOK
	})

	res := d.deliverDueWith(ctx)
	require.Equal(t, 1, res.Gaps)
	require.Zero(t, res.GapsSent+res.GapsFailed, "the run does not know whether the endpoint got the envelope, so it counts neither")
	require.True(t, res.Incomplete)

	d.advance(faucet.DeliveryLease)
	next := d.deliverDueWith(context.Background())
	require.Equal(t, 1, next.GapsSent, "the next run sends the gap again after the lease")
	require.Len(t, d.rcv.envelopesOf("/hook", "gap"), 2)
}

func TestDeliverDueSendsAGapToTheSubscriptionAsItIsWhenTheEnvelopeGoesOut(t *testing.T) {
	t.Parallel()

	t.Run("a subscription that ended after the gap was queued gets nothing", func(t *testing.T) {
		t.Parallel()
		var (
			d    *deliverHarness
			ep   hookEndpoint
			once sync.Once
		)
		d = newDeliverHarness(t, func(deps *faucet.Deps) {
			deps.Store = &scriptedStore{Store: deps.Store, onExpire: func() {
				once.Do(func() { require.Nil(t, d.unsubscribe(ep.args, d.rcv.url(ep.path))) })
			}}
		})
		d.ledgerSkip = true
		ep = d.endpoint("/hook")
		d.purgedBeforeRead(ep)

		res := d.deliverDue()
		require.Equal(t, 1, res.Gaps, "the run queued the gap")
		require.Zero(t, res.GapsSent+res.GapsFailed)
		require.Zero(t, d.rcv.total(), "the subscription is gone, so nothing goes to its endpoint")
	})

	t.Run("a secret that rotated after the gap was queued signs it", func(t *testing.T) {
		t.Parallel()
		var (
			d       *deliverHarness
			ep      hookEndpoint
			rotated string
			once    sync.Once
		)
		d = newDeliverHarness(t, func(deps *faucet.Deps) {
			deps.Store = &scriptedStore{Store: deps.Store, onExpire: func() {
				once.Do(func() { rotated = d.rotate(ep, newSecret(t, 32)).secret })
			}}
		})
		ep = d.endpoint("/hook")
		sub, _, _ := d.purgedBeforeRead(ep)

		require.Equal(t, 1, d.deliverDue().GapsSent)
		gaps := d.rcv.envelopesOf("/hook", "gap")
		require.Len(t, gaps, 1)
		d.requireEnvelope(gaps[0], "gap", rotated, sub.ID)
	})
}

func TestDeliverDueSendsGapsAndDeliveriesWhenTheSweepOfPreviousSecretsFails(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t, func(deps *faucet.Deps) {
		deps.Store = &scriptedStore{Store: deps.Store, clearErr: xerrors.New("database is busy")}
	})
	d.purgedBeforeRead(d.endpoint("/hook"))

	res := d.deliverDue()
	require.Equal(t, 1, res.GapsSent, "a failed sweep does not keep the gap envelope from going out")
	require.Equal(t, 1, res.Delivered, "or the delivery")
	require.True(t, d.logsContain("clear expired previous secrets"))
}

func TestDeliverDueSendsNoGapToASubscriptionWhoseRepositoryLostNoEvents(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	quiet := d.endpoint("/quiet")
	d.subscribeEndpoint(quiet)
	d.advance(time.Second)
	d.insert(otherRepoID, "ci.run_completed", "")
	d.insert(otherRepoID, "ci.run_completed", "")
	d.advance(5 * time.Minute)
	d.insert(otherRepoID, "ci.run_completed", "")
	res, err := d.store.Purge(d.ctx, d.clock.Now().Add(-time.Minute))
	require.NoError(t, err)
	require.EqualValues(t, 2, res.Deleted, "the purge deleted events of another repository only")

	got := d.deliverDue()
	require.Zero(t, got.Gaps, "no event of the subscription's repository was lost")
	require.Zero(t, d.rcv.total())

	lossy := d.endpoint("/lossy")
	d.subscribeEndpoint(lossy)
	d.advance(time.Second)
	d.newRunEvent()
	d.advance(5 * time.Minute)
	d.insert(otherRepoID, "ci.run_completed", "")
	_, err = d.store.Purge(d.ctx, d.clock.Now().Add(-time.Minute))
	require.NoError(t, err)
	d.advance(time.Second)
	d.insert(otherRepoID, "ci.run_completed", "")
	_, err = d.store.Purge(d.ctx, d.clock.Now().Add(-time.Second))
	require.NoError(t, err)

	got = d.deliverDue()
	require.Equal(t, 1, got.Gaps, "the subscription that lost an event gets its gap, also after a second purge")
	require.Len(t, d.rcv.requests("/lossy"), 1)
	require.Empty(t, d.rcv.requests("/quiet"), "the quiet subscription still hears nothing")
}
