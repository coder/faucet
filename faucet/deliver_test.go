package faucet_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
)

var messageIDShape = regexp.MustCompile(`^msg_[A-Za-z0-9_-]{22}$`)

func TestDeliverDueSendsSignedMatchingEventsOnly(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	all, onlyPR7 := d.endpoint("/all"), d.endpoint("/pr7", "pr", 7)
	allSub, pr7Sub := d.subscribeEndpoint(all), d.subscribeEndpoint(onlyPR7)
	d.advance(time.Second)

	first := d.newRunEvent(7)
	second := d.newRunEvent(8)
	d.insert(repoID, "pr.closed", "", 7)
	d.insert(otherRepoID, "ci.run_completed", "", 7)

	res := d.deliverDue()
	require.Equal(t, 3, res.Enqueued, "two events for the first subscription, one for the second")
	require.Equal(t, 3, res.Delivered)
	require.Equal(t, 3, res.Sent)
	require.Zero(t, res.GaveUpTotal())

	toAll, toPR7 := d.rcv.requests("/all"), d.rcv.requests("/pr7")
	require.Len(t, toAll, 2, "the other type and the other repository are not sent")
	require.Len(t, toPR7, 1, "the pr filter holds")
	require.Equal(t, 3, d.rcv.total(), "nothing reached another path")

	ids := map[string]bool{}
	for _, tc := range []struct {
		got    []hookRequest
		secret string
		other  string
		id     string
		prs    []float64
	}{
		{toAll, all.secret, onlyPR7.secret, allSub.ID, []float64{7, 8}},
		{toPR7, onlyPR7.secret, all.secret, pr7Sub.ID, []float64{7}},
	} {
		var seen []float64
		for _, g := range tc.got {
			require.True(t, g.signatureValid(tc.secret), "the signature verifies over the bytes hookRequest, with the subscriber's secret")
			require.False(t, g.signatureValid(tc.other), "and not with another subscription's secret")
			require.Equal(t, "application/json", g.Header.Get("Content-Type"))
			require.Equal(t, tc.id, g.Header.Get("X-MCP-Subscription-Id"))
			require.Equal(t, strconv.FormatInt(d.clock.Now().Unix(), 10), g.Header.Get("webhook-timestamp"))
			require.Empty(t, g.Header.Get("Authorization"))
			require.Empty(t, g.Header.Get("Cookie"))

			id := g.messageID()
			require.Regexp(t, messageIDShape, id)
			require.False(t, ids[id], "a message ID is used once")
			ids[id] = true

			body := g.body(t)
			require.Equal(t, id, body.EventID, "eventId is the webhook-id, and not the GitHub delivery ID that events/poll shows")
			require.Equal(t, "ci.run_completed", body.Name)
			require.Regexp(t, `^v1\.\d+$`, body.Cursor)
			prs, ok := body.Data["prs"].([]any)
			require.True(t, ok, "data carries the pull requests, as events/poll shows them")
			require.Len(t, prs, 1)
			seen = append(seen, prs[0].(float64))
			require.EqualValues(t, 1, body.Data["run_id"])
			if prs[0].(float64) == 7 {
				require.True(t, first.ReceivedAt.UTC().Equal(body.Timestamp), "timestamp is when the event was hookRequest")
			} else {
				require.True(t, second.ReceivedAt.UTC().Equal(body.Timestamp))
			}
		}
		require.ElementsMatch(t, tc.prs, seen)
	}
	require.Len(t, ids, 3, "the same event on two subscriptions is two deliveries with two message IDs")

	again := d.deliverDue()
	require.Zero(t, again.Sent, "a delivery that ended is not sent again")
	require.Zero(t, again.Enqueued, "and events that were read are not queued again")
}

func TestDeliverDueMatchesTheSHAWhateverItsCase(t *testing.T) {
	t.Parallel()

	const sha = "0123456789abcdef0123456789abcdef01234567"
	d := newDeliverHarness(t)
	upper := d.endpoint("/upper", "sha", strings.ToUpper(sha))
	other := d.endpoint("/other", "sha", "ffffffffffffffffffffffffffffffffffffffff")
	d.subscribeEndpoint(upper)
	d.subscribeEndpoint(other)
	d.advance(time.Second)

	d.insert(repoID, "ci.run_completed", sha)
	res := d.deliverDue()
	require.Equal(t, 1, res.Enqueued, "a subscription written in capitals matches the head SHA that GitHub sent in lower case")
	require.Len(t, d.rcv.requests("/upper"), 1)
	require.Empty(t, d.rcv.requests("/other"), "another SHA matches nothing")
}

func TestDeliverDueGivesAReplayOfAnEventTheMessageIDItHadBefore(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	d.subscribeEndpoint(ep)
	d.advance(time.Second)
	ev := d.newRunEvent(7)
	require.Equal(t, 1, d.deliverDue().Delivered)

	d.advance(time.Second)
	require.Nil(t, d.unsubscribe(ep.args, d.rcv.url(ep.path)))
	d.advance(time.Second)
	again := subscribeTo(ep.args, d.rcv.url(ep.path), ep.secret)
	again.Cursor = cursorOf(fmt.Sprintf("v1.%d", ev.ID-1))
	d.subscribe(again)
	d.advance(time.Second)
	require.Equal(t, 1, d.deliverDue().Delivered, "a subscribe from an older cursor sends the event again")

	requests := d.rcv.requests("/hook")
	require.Len(t, requests, 2)
	require.Equal(t, requests[0].messageID(), requests[1].messageID(),
		"the same subscription and event keep one webhook-id, so a receiver that deduplicates on it drops the replay")
	require.Equal(t, requests[0].body(t).EventID, requests[1].body(t).EventID)
	require.True(t, requests[1].signatureValid(ep.secret))
}

func TestDeliverDueBodyCursorIsTheWatermark(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	d.subscribeEndpoint(ep)
	d.advance(time.Second)
	failing := true
	d.rcv.answer("/hook", func(n int) int {
		if failing && n == 1 {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	})

	first := d.newRunEvent(1)
	d.deliverDue()
	second := d.newRunEvent(2)
	d.deliverDue()
	requests := d.rcv.requests("/hook")
	require.Len(t, requests, 2)
	require.Equal(t, fmt.Sprintf("v1.%d", first.ID-1), requests[1].body(t).Cursor,
		"the second event is sent while the first is unsent, so its watermark stays below the first")

	d.advance(15 * time.Second)
	d.deliverDue()
	requests = d.rcv.requests("/hook")
	require.Len(t, requests, 3)
	require.Equal(t, requests[0].messageID(), requests[2].messageID())
	require.Equal(t, fmt.Sprintf("v1.%d", first.ID-1), requests[2].body(t).Cursor, "the oldest unsent event carries the watermark below itself")

	third := d.newRunEvent(3)
	d.deliverDue()
	requests = d.rcv.requests("/hook")
	require.Len(t, requests, 4)
	require.Equal(t, fmt.Sprintf("v1.%d", third.ID-1), requests[3].body(t).Cursor,
		"with every earlier event delivered, the watermark reaches the event before this one")
	require.Equal(t, second.ID+1, third.ID)
}

func TestDeliverDueRetriesOnTheScheduleWithOneMessageID(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	d.subscribeEndpoint(ep)
	d.advance(time.Second)
	d.rcv.answer("/hook", always(http.StatusInternalServerError))
	d.newRunEvent()

	waits := []time.Duration{0, 15 * time.Second, time.Minute, 3 * time.Minute, 6 * time.Minute}
	var tally faucet.DeliverResult
	tally.GaveUp = map[faucet.GiveUpReason]int{}
	for i, wait := range waits {
		if wait > 0 {
			d.advance(wait - time.Second)
			d.subscribeEndpoint(ep)
			early := d.deliverDue()
			require.Zero(t, early.Sent, "attempt %d is not due one second before its backoff ends", i+1)
			d.advance(time.Second)
		}
		d.subscribeEndpoint(ep)
		res := d.deliverDue()
		require.Equal(t, 1, res.Sent, "attempt %d", i+1)
		tally.Retried += res.Retried
		tally.GaveUp[faucet.GiveUpAttemptsExhausted] += res.GaveUp[faucet.GiveUpAttemptsExhausted]
	}
	require.Equal(t, 4, tally.Retried, "the first four failures are retried")
	require.Equal(t, 1, tally.GaveUp[faucet.GiveUpAttemptsExhausted], "the fifth ends the delivery, counted")

	requests := d.rcv.requests("/hook")
	require.Len(t, requests, 5, "a delivery gets five attempts")
	timestamps := map[string]bool{}
	for _, g := range requests {
		require.Equal(t, requests[0].messageID(), g.messageID(), "every attempt has the same webhook-id")
		require.Equal(t, requests[0].body(t).EventID, g.body(t).EventID)
		require.True(t, g.signatureValid(ep.secret), "every attempt is signed afresh")
		timestamps[g.Header.Get("webhook-timestamp")] = true
	}
	require.Len(t, timestamps, 5, "every attempt carries its own timestamp")

	d.advance(time.Hour)
	require.Zero(t, d.deliverDue().Sent, "nothing follows the fifth attempt")
	require.Len(t, d.rcv.requests("/hook"), 5)
}

func TestDeliverDueEndsOn410And413ButKeepsTheSubscription(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		status int
		reason faucet.GiveUpReason
	}{
		{http.StatusGone, faucet.GiveUpEndpointGone},
		{http.StatusRequestEntityTooLarge, faucet.GiveUpEndpointPayloadTooLarge},
	} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			t.Parallel()
			d := newDeliverHarness(t)
			ep := d.endpoint("/hook")
			d.subscribeEndpoint(ep)
			d.advance(time.Second)
			d.rcv.answer("/hook", func(n int) int {
				if n == 1 {
					return tc.status
				}
				return http.StatusOK
			})

			d.newRunEvent()
			res := d.deliverDue()
			require.Equal(t, 1, res.Sent)
			require.Equal(t, map[faucet.GiveUpReason]int{tc.reason: 1}, res.GaveUp)
			require.Zero(t, res.Retried, "the answer ends the delivery, with no retry")

			d.advance(5 * time.Minute)
			healthy := d.refreshStatus(ep)
			require.NotNil(t, healthy)
			require.Nil(t, healthy.LastError, "the endpoint refused this delivery, which does not touch the subscription's status")
			require.Zero(t, d.deliverDue().Sent, "the delivery that ended is not retried")
			require.Len(t, d.rcv.requests("/hook"), 1)

			d.newRunEvent()
			res = d.deliverDue()
			require.Equal(t, 1, res.Delivered, "the subscription lives on, and the next event reaches the hookEndpoint")
			require.Len(t, d.rcv.requests("/hook"), 2)
		})
	}
}

func TestDeliverDueRetriesTheOtherAnswersAndFollowsNoRedirect(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		status   int
		category string
	}{
		{http.StatusBadRequest, "http_4xx"},
		{http.StatusNotFound, "http_4xx"},
		{http.StatusTooManyRequests, "http_4xx"},
		{http.StatusInternalServerError, "http_5xx"},
		{http.StatusServiceUnavailable, "http_5xx"},
		{http.StatusMovedPermanently, "http_4xx"},
	} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			t.Parallel()
			d := newDeliverHarness(t)
			ep := d.endpoint("/hook")
			d.subscribeEndpoint(ep)
			d.advance(time.Second)
			d.rcv.answer("/hook", always(tc.status))

			d.newRunEvent()
			res := d.deliverDue()
			require.Equal(t, 1, res.Sent)
			require.Equal(t, 1, res.Retried)
			require.Zero(t, res.GaveUpTotal())
			require.Empty(t, d.rcv.requests("/elsewhere"), "a redirect is never followed")

			d.advance(time.Second)
			refreshed := d.refreshStatus(ep)
			require.NotNil(t, refreshed)
			require.Equal(t, tc.category, *refreshed.LastError, "the category of the failure, and nothing the hookEndpoint sent")
		})
	}
}

func TestDeliverDueCountsAnAnswerWithOversizedHeadersAsAServerError(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	d.subscribeEndpoint(ep)
	d.advance(time.Second)
	d.rcv.padHeaders(ep.path)

	d.newRunEvent()
	res := d.deliverDue()
	require.Equal(t, 1, res.Retried, "the client refuses headers past 16 KiB, so the attempt fails")
	d.advance(time.Second)
	status := d.refreshStatus(ep)
	require.NotNil(t, status)
	require.Equal(t, "http_5xx", *status.LastError, "and the category blames the endpoint, as it does for a body that is too large")
}

func TestDeliverDueGivesUpWhenTheWindowHasPassed(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	d.subscribeEndpoint(ep)
	d.advance(time.Second)
	d.rcv.answer("/hook", always(http.StatusInternalServerError))
	d.newRunEvent()
	require.Equal(t, 1, d.deliverDue().Retried)

	for range 3 {
		d.advance(5 * time.Minute)
		d.subscribeEndpoint(ep)
	}
	d.advance(time.Minute)
	d.subscribeEndpoint(ep)
	res := d.deliverDue()
	require.Zero(t, res.Sent, "no request goes out after the window")
	require.Equal(t, map[faucet.GiveUpReason]int{faucet.GiveUpWindowExpired: 1}, res.GaveUp)
	require.Len(t, d.rcv.requests("/hook"), 1)
}

func TestDeliverDueExpiredSubscriptionsGetNothing(t *testing.T) {
	t.Parallel()

	t.Run("expired before the events were read", func(t *testing.T) {
		t.Parallel()
		d := newDeliverHarness(t)
		d.subscribeEndpoint(d.endpoint("/hook"))
		d.advance(time.Second)
		d.newRunEvent()

		d.advance(10*time.Minute + time.Second)
		res := d.deliverDue()
		require.Zero(t, res.Enqueued, "an expired subscription reads no events")
		require.Zero(t, res.Sent)
		require.Zero(t, d.rcv.total())
	})

	t.Run("expired with a delivery waiting", func(t *testing.T) {
		t.Parallel()
		d := newDeliverHarness(t)
		d.subscribeEndpoint(d.endpoint("/hook"))
		d.advance(time.Second)
		d.rcv.answer("/hook", always(http.StatusInternalServerError))
		d.newRunEvent()
		require.Equal(t, 1, d.deliverDue().Retried)

		d.advance(10*time.Minute + time.Second)
		res := d.deliverDue()
		require.Zero(t, res.Sent, "an expired subscription gets no retry")
		require.Equal(t, map[faucet.GiveUpReason]int{faucet.GiveUpSubscriptionExpired: 1}, res.GaveUp, "its waiting delivery ends, counted")
		require.Equal(t, 1, d.rcv.total())
	})
}

func TestDeliverDueSendsOneRequestPerAttemptUnderConcurrentRuns(t *testing.T) {
	t.Parallel()

	const (
		events = 40
		runs   = 6
	)
	d := newDeliverHarness(t)
	d.rcv.delay = 20 * time.Millisecond
	d.subscribeEndpoint(d.endpoint("/hook"))
	d.advance(time.Second)
	for range events {
		d.newRunEvent()
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []faucet.DeliverResult
	)
	start := make(chan struct{})
	for range runs {
		wg.Go(func() {
			<-start
			res, err := d.svc.DeliverDue(d.ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("run: %v", err)
			}
			results = append(results, res)
		})
	}
	close(start)
	wg.Wait()

	perMessage := map[string]int{}
	for _, g := range d.rcv.requests("/hook") {
		perMessage[g.messageID()]++
	}
	require.Len(t, perMessage, events, "every event is sent")
	for id, n := range perMessage {
		require.Equal(t, 1, n, "message %s was sent %d times", id, n)
	}
	for _, res := range results {
		d.record(res)
	}
	require.Equal(t, events, d.ledger.Enqueued, "the runs queued each event once between them")
	require.Equal(t, events, d.ledger.Delivered)
}

func TestDeliverDueTakesBackADeliveryWhenALeaseRunsOut(t *testing.T) {
	t.Parallel()

	queue := func(d *deliverHarness) (faucet.DeliveryClaim, faucet.ScanTarget) {
		t.Helper()
		d.subscribeEndpoint(d.endpoint("/hook"))
		d.advance(time.Second)
		ev := d.newRunEvent()
		targets, err := d.store.ScanTargets(d.ctx, d.clock.Now(), 100, 10)
		require.NoError(t, err)
		require.Len(t, targets, 1)
		won, err := d.store.EnqueueDeliveries(d.ctx, d.clock.Now(), faucet.Enqueue{
			Key: targets[0].KeyHash, ExpectedScannedThrough: targets[0].ScannedThrough, NewScannedThrough: ev.ID,
			Deliveries: []faucet.NewDelivery{{EventID: ev.ID, MessageID: "msg_crashed_run"}},
		})
		require.NoError(t, err)
		require.True(t, won)
		d.ledger.Enqueued++
		claim, ok, err := d.store.ClaimDelivery(d.ctx, d.clock.Now(), faucet.DeliveryLease)
		require.NoError(t, err)
		require.True(t, ok, "the run that is about to crash holds the delivery")
		return claim, targets[0]
	}

	t.Run("the lease holds, then runs out", func(t *testing.T) {
		t.Parallel()
		d := newDeliverHarness(t)
		queue(d)

		require.Zero(t, d.deliverDue().Sent, "a live lease keeps the delivery from a second sender")
		d.advance(faucet.DeliveryLease - time.Second)
		require.Zero(t, d.deliverDue().Sent)

		d.advance(time.Second)
		res := d.deliverDue()
		require.Equal(t, 1, res.Delivered, "the lease ran out, so the next run sends it")
		requests := d.rcv.requests("/hook")
		require.Len(t, requests, 1)
		require.Equal(t, "msg_crashed_run", requests[0].messageID(), "the webhook-id survived the crash")
	})

	t.Run("a crash on the last attempt ends the delivery without a request", func(t *testing.T) {
		t.Parallel()
		d := newDeliverHarness(t)
		queue(d)
		for attempt := 2; attempt <= faucet.MaxDeliveryAttempts; attempt++ {
			d.advance(faucet.DeliveryLease)
			_, ok, err := d.store.ClaimDelivery(d.ctx, d.clock.Now(), faucet.DeliveryLease)
			require.NoError(t, err)
			require.True(t, ok, "attempt %d is claimed once the lease runs out", attempt)
		}

		d.advance(faucet.DeliveryLease)
		res := d.deliverDue()
		require.Zero(t, res.Sent, "five attempts were used, so there is no sixth")
		require.Equal(t, map[faucet.GiveUpReason]int{faucet.GiveUpAttemptsExhausted: 1}, res.GaveUp)
		require.Zero(t, d.rcv.total())
	})
}

func TestDeliverDueEndsEveryDeliveryItCannotSendWithAReason(t *testing.T) {
	t.Parallel()

	failOnce := func(d *deliverHarness) {
		d.rcv.answer("/hook", func(n int) int {
			if n == 1 {
				return http.StatusInternalServerError
			}
			return http.StatusOK
		})
	}
	insertRaw := func(d *deliverHarness, data string) faucet.Event {
		d.t.Helper()
		d.deliveries++
		ev, _, err := d.store.InsertEvent(d.ctx, faucet.Event{
			Source: "github", DeliveryID: fmt.Sprintf("delivery-%d", d.deliveries), Type: "ci.run_completed", RepoID: repoID, Data: json.RawMessage(data),
		})
		require.NoError(d.t, err)
		return ev
	}

	for name, tc := range map[string]struct {
		reason faucet.GiveUpReason
		run    func(d *deliverHarness, ep hookEndpoint) faucet.DeliverResult
	}{
		"the event was purged while the delivery waited": {faucet.GiveUpEventPurged, func(d *deliverHarness, _ hookEndpoint) faucet.DeliverResult {
			failOnce(d)
			d.newRunEvent()
			require.Equal(d.t, 1, d.deliverDue().Retried)
			_, err := d.store.Purge(d.ctx, d.clock.Now().Add(time.Hour))
			require.NoError(d.t, err)
			d.advance(15 * time.Second)
			return d.deliverDue()
		}},
		"the event is not a JSON object": {faucet.GiveUpEventMalformed, func(d *deliverHarness, _ hookEndpoint) faucet.DeliverResult {
			insertRaw(d, `[1,2]`)
			return d.deliverDue()
		}},
		"the body would pass 256 KiB": {faucet.GiveUpEventTooLarge, func(d *deliverHarness, _ hookEndpoint) faucet.DeliverResult {
			insertRaw(d, `{"blob":"`+strings.Repeat("x", 300<<10)+`"}`)
			return d.deliverDue()
		}},
		"the name now means another repository": {faucet.GiveUpRepoMismatch, func(d *deliverHarness, ep hookEndpoint) faucet.DeliverResult {
			failOnce(d)
			d.newRunEvent()
			require.Equal(d.t, 1, d.deliverDue().Retried)
			d.authz.mu.Lock()
			d.authz.repos[testRepo] = deniedRepoID
			d.authz.mu.Unlock()
			d.advance(time.Second)
			d.subscribeEndpoint(ep)
			d.advance(15 * time.Second)
			return d.deliverDue()
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := newDeliverHarness(t)
			ep := d.endpoint("/hook")
			d.subscribeEndpoint(ep)
			d.advance(time.Second)

			res := tc.run(d, ep)
			require.Equal(t, map[faucet.GiveUpReason]int{tc.reason: 1}, res.GaveUp, "the delivery ends with its reason, counted")
			require.Zero(t, res.Delivered)
			require.LessOrEqual(t, d.rcv.total(), 1, "no request goes out for it")
			require.True(t, d.logsContain(string(tc.reason)), "one log line names the reason")
		})
	}

	t.Run("a body just under the limit is sent", func(t *testing.T) {
		t.Parallel()
		d := newDeliverHarness(t)
		d.subscribeEndpoint(d.endpoint("/hook"))
		d.advance(time.Second)
		insertRaw(d, `{"blob":"`+strings.Repeat("x", 200<<10)+`"}`)
		require.Equal(t, 1, d.deliverDue().Delivered)
	})
}

func TestDeliverDueEndsADeliveryWhoseSecretDoesNotOpen(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	sub := d.subscribeEndpoint(ep)
	d.advance(time.Second)
	for range 3 {
		d.newRunEvent()
	}
	d.store.tamperSecret(d.rows()[0].KeyHash)

	res := d.deliverDue()
	require.Equal(t, 3, res.GaveUp[faucet.GiveUpSecretUnreadable], "the delivery that was tried and the two that waited behind it")
	require.Zero(t, d.rcv.total(), "nothing is sent without the secret")
	require.Empty(t, d.rows(), "the subscription is dead, and a refresh seals the secret again")
	require.True(t, d.logsContain(sub.ID))

	d.advance(time.Second)
	d.subscribeEndpoint(ep)
	d.newRunEvent()
	require.Equal(t, 1, d.deliverDue().Delivered, "the refresh made a working subscription")
}

func TestDeliverDueQueuesNoMoreThanTheCapAndLosesNothing(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	d.subscribeEndpoint(d.endpoint("/hook"))
	d.advance(time.Second)
	d.rcv.answer("/hook", always(http.StatusInternalServerError))
	const events = faucet.MaxPendingDeliveries + 20
	for range events {
		d.newRunEvent()
	}

	res := d.deliverDue()
	require.Equal(t, faucet.MaxPendingDeliveries, res.Enqueued, "the queue stops at the cap")
	require.Equal(t, faucet.MaxPendingDeliveries, res.Retried)
	d.advance(time.Second)
	require.Zero(t, d.deliverDue().Enqueued, "a subscription at the cap reads no more events")

	d.rcv.answer("/hook", always(http.StatusOK))
	d.advance(15 * time.Second)
	require.Equal(t, faucet.MaxPendingDeliveries, d.deliverDue().Delivered)
	res = d.deliverDue()
	require.Equal(t, 20, res.Enqueued, "the events behind the cap were waiting in the log")
	require.Equal(t, 20, res.Delivered)

	ok := map[string]bool{}
	for _, g := range d.rcv.requests("/hook") {
		ok[g.messageID()] = true
	}
	require.Len(t, ok, events, "every event reached the hookEndpoint")
}

func TestDeliverDueFillsOnlyTheSlotsThatAreLeftUnderTheCap(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	d.subscribeEndpoint(d.endpoint("/hook"))
	d.advance(time.Second)
	d.rcv.answer("/hook", always(http.StatusInternalServerError))
	for range faucet.MaxPendingDeliveries - 1 {
		d.newRunEvent()
	}
	require.Equal(t, faucet.MaxPendingDeliveries-1, d.deliverDue().Enqueued)

	for range 3 {
		d.newRunEvent()
	}
	d.advance(time.Second)
	require.Equal(t, 1, d.deliverDue().Enqueued, "99 are waiting, so one more event fits and the other two stay in the log")
}

func TestDeliverDueStopsAtItsLimits(t *testing.T) {
	t.Parallel()

	t.Run("200 claims a run", func(t *testing.T) {
		t.Parallel()
		d := newDeliverHarness(t)
		for _, path := range []string{"/a", "/b", "/c"} {
			d.subscribeEndpoint(d.endpoint(path))
		}
		d.advance(time.Second)
		for range faucet.MaxPendingDeliveries {
			d.newRunEvent()
		}

		first := d.deliverDue()
		require.Equal(t, 300, first.Enqueued)
		require.Equal(t, 200, first.Claimed)
		require.True(t, first.Incomplete, "due work is left")
		second := d.deliverDue()
		require.Equal(t, 100, second.Claimed)
		require.False(t, second.Incomplete)
	})

	t.Run("a run that is out of time still sends what is queued and reports no error", func(t *testing.T) {
		t.Parallel()
		d := newDeliverHarness(t)
		d.clock.Set(time.Now()).MustWait(d.ctx)
		d.subscribeEndpoint(d.endpoint("/hook"))
		d.advance(time.Second)
		d.rcv.answer("/hook", func(n int) int {
			if n == 1 {
				return http.StatusInternalServerError
			}
			return http.StatusOK
		})
		d.newRunEvent()
		require.Equal(t, 1, d.deliverDue().Retried)
		d.newRunEvent()
		d.advance(15 * time.Second)

		short, cancel := context.WithDeadline(d.ctx, d.clock.Now().Add(15*time.Second))
		defer cancel()
		res := d.deliverDueWith(short)
		require.Zero(t, res.Enqueued, "with under 20 seconds left, no new event is read")
		require.Equal(t, 1, res.Delivered, "and the retry that is due still goes out")
		require.True(t, res.Incomplete)
		require.Equal(t, 1, d.deliverDueWith(context.Background()).Enqueued, "the next run reads the event")

		ended, endNow := context.WithCancel(d.ctx)
		endNow()
		res = d.deliverDueWith(ended)
		require.True(t, res.Incomplete, "a context that ended is no failure")
	})

	t.Run("a run reads the events of more than 200 subscriptions", func(t *testing.T) {
		t.Parallel()
		d := newDeliverHarness(t)
		const principals, perPrincipal = 3, 90
		for p := range principals {
			d.as(fmt.Sprintf("user-%d", p))
			for n := range perPrincipal {
				d.subscribeEndpoint(d.endpoint(fmt.Sprintf("/hook/%d/%d", p, n)))
			}
		}
		d.advance(time.Second)
		d.newRunEvent()

		res := d.deliverDue()
		require.Equal(t, principals*perPrincipal, res.Enqueued, "every subscription's event is queued in one run")
		require.Equal(t, 200, res.Claimed)
		require.True(t, res.Incomplete)
	})

	t.Run("no attempt starts with under 10 seconds left", func(t *testing.T) {
		t.Parallel()
		d := newDeliverHarness(t)
		d.clock.Set(time.Now()).MustWait(d.ctx) // The run's deadline is a real time, so the mock clock must be near it.
		d.subscribeEndpoint(d.endpoint("/hook"))
		d.advance(time.Second)
		d.newRunEvent()

		short, cancel := context.WithDeadline(d.ctx, d.clock.Now().Add(9*time.Second))
		defer cancel()
		res := d.deliverDueWith(short)
		require.Zero(t, res.Claimed)
		require.True(t, res.Incomplete)
		require.Zero(t, d.rcv.total())

		long, cancelLong := context.WithDeadline(d.ctx, d.clock.Now().Add(21*time.Second))
		defer cancelLong()
		got := d.deliverDueWith(long)
		require.Equal(t, 1, got.Delivered, "%+v", got)
	})
}

func TestDeliverDueKeepsTheSecretAndTheURLOutOfLogsRowsAndErrors(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook-token-5f3a91")
	d.subscribeEndpoint(ep)
	d.advance(time.Second)
	d.rcv.answer(ep.path, func(n int) int {
		if n == 1 {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	})
	d.newRunEvent()
	d.deliverDue()
	d.advance(15 * time.Second)
	d.deliverDue()
	d.newRunEvent()
	d.store.err = xerrors.New("database is down")
	_, err := d.svc.DeliverDue(d.ctx)
	require.Error(t, err)
	d.store.err = nil
	d.ledgerSkip = true

	key := decodedSecret(t, ep.secret)
	needles := []string{ep.secret, strings.TrimPrefix(ep.secret, "whsec_"), "hook-token-5f3a91", d.rcv.url(ep.path)}
	for _, needle := range needles {
		require.NotContains(t, d.logs.String(), needle, "the log never holds the secret or the callback URL")
		require.NotContains(t, err.Error(), needle)
	}
	for _, row := range d.rows() {
		require.NotContains(t, string(row.SecretEnc), string(key), "the row holds the secret sealed")
	}
	require.NotEmpty(t, d.logs.String())
}

func TestDeliverDueReportsEventsThatAPurgeDeletedBeforeTheyWereRead(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	sub := d.subscribeEndpoint(d.endpoint("/hook"))
	d.advance(time.Second)
	d.newRunEvent()
	d.newRunEvent()
	_, err := d.store.Purge(d.ctx, d.clock.Now().Add(time.Hour))
	require.NoError(t, err)

	res := d.deliverDue()
	require.Equal(t, 1, res.Gaps, "the subscription missed events")
	require.Zero(t, res.Enqueued)
	require.Equal(t, 1, d.rcv.total(), "the gap envelope is the only request")
	require.True(t, d.logsContain(sub.ID))

	d.newRunEvent()
	res = d.deliverDue()
	require.Zero(t, res.Gaps, "the next event is read from past the purge")
	require.Equal(t, 1, res.Delivered)
}

func TestDeliverDueNeedsWebhookDeliveryToBeOn(t *testing.T) {
	t.Parallel()

	svc, err := faucet.New(testDeps(&fakeStore{}))
	require.NoError(t, err)
	_, err = svc.DeliverDue(context.Background())
	require.ErrorIs(t, err, faucet.ErrNoWebhook)
}

func TestDeliverDueReturnsTheStoreError(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	d.subscribeEndpoint(d.endpoint("/hook"))
	d.advance(time.Second)
	d.newRunEvent()
	d.store.err = xerrors.New("database is down")
	_, err := d.svc.DeliverDue(d.ctx)
	require.ErrorContains(t, err, "database is down")
	require.Zero(t, d.rcv.total())
	d.store.err = nil
	d.ledgerSkip = true
	require.Equal(t, 1, d.deliverDue().Delivered, "the next run picks the work up")
}

func (d *deliverHarness) refreshStatus(ep hookEndpoint) *deliveredStatus {
	d.t.Helper()
	res, err := d.call(methodSubscribe, subscribeTo(ep.args, d.rcv.url(ep.path), ep.secret))
	require.NoError(d.t, err)
	raw, ok := res.fields["deliveryStatus"]
	if !ok {
		return nil
	}
	var st deliveredStatus
	require.NoError(d.t, json.Unmarshal(raw, &st))
	return &st
}

type deliveredStatus struct {
	Active         bool    `json:"active"`
	LastDeliveryAt string  `json:"lastDeliveryAt"`
	LastError      *string `json:"lastError"`
	FailedSince    string  `json:"failedSince"`
}

func TestSubscribeReportsTheDeliveryStatusOfARefreshedSubscription(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	res, err := d.call(methodSubscribe, subscribeTo(ep.args, d.rcv.url(ep.path), ep.secret))
	require.NoError(t, err)
	require.NotContains(t, res.fields, "deliveryStatus", "a new subscription has no delivery history")
	d.advance(time.Second)
	d.rcv.answer(ep.path, func(n int) int {
		if n == 1 {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	})

	healthy := d.refreshStatus(ep)
	require.NotNil(t, healthy)
	require.True(t, healthy.Active)
	require.Nil(t, healthy.LastError)
	require.Empty(t, healthy.LastDeliveryAt)
	require.Empty(t, healthy.FailedSince)

	d.newRunEvent()
	failedAt := d.clock.Now()
	d.deliverDue()
	d.advance(time.Second)
	failing := d.refreshStatus(ep)
	require.Equal(t, "http_5xx", *failing.LastError)
	require.Equal(t, failedAt.UTC().Format(time.RFC3339), failing.FailedSince)
	require.True(t, failing.Active, "faucet does not suspend")

	d.advance(15 * time.Second)
	deliveredAt := d.clock.Now()
	d.deliverDue()
	d.advance(time.Second)
	recovered := d.refreshStatus(ep)
	require.Nil(t, recovered.LastError, "a 2xx clears the failure")
	require.Equal(t, deliveredAt.UTC().Format(time.RFC3339), recovered.LastDeliveryAt)
	require.Empty(t, recovered.FailedSince)
}
