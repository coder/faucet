package faucet_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
)

func newRawDeliverHarness(t *testing.T, mutate ...func(*faucet.Deps)) *deliverHarness {
	t.Helper()
	return newDeliverHarness(t, append([]func(*faucet.Deps){withRawStore}, mutate...)...)
}

func (s *fakeStore) rawOutboxRows() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rawOutbox)
}

// corruptRawArguments gives a raw subscription arguments that do not decode.
func (s *fakeStore) corruptRawArguments(key faucet.SubscriptionKeyHash) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub := s.subs[key]
	sub.Arguments = []byte(`{"repo":"octo/repo","fields":"not a list"}`)
	s.subs[key] = sub
}

// subscribeRaw subscribes the endpoint to a raw name, or renews it.
func (d *deliverHarness) subscribeRaw(name string, ep hookEndpoint) subscribed {
	d.t.Helper()
	return d.subscribe(rawSubscribeTo(name, ep.args, d.rcv.url(ep.path), ep.secret))
}

// deliverRaw posts a signed delivery through the webhook handler, which must
// store it, and returns its raw ID. It adds the raw deliveries that the
// handler queued to the ledger.
func (d *deliverHarness) deliverRaw(event string, body []byte) int64 {
	d.t.Helper()
	d.deliveries++
	before := d.store.rawOutboxRows()
	rec := httptest.NewRecorder()
	d.svc.WebhookHandler().ServeHTTP(rec, newRequest(http.MethodPost, body, event, fmt.Sprintf("raw-delivery-%d", d.deliveries)))
	require.Equal(d.t, "stored", strings.TrimSpace(rec.Body.String()))
	d.rawQueued += d.store.rawOutboxRows() - before
	head, err := d.store.RawHead(d.ctx)
	require.NoError(d.t, err)
	return head
}

// pushRaw stores a push of the test repository with extra members.
func (d *deliverHarness) pushRaw(extra map[string]any) int64 {
	d.t.Helper()
	return d.deliverRaw("push", rawPayload(repoID, "", extra))
}

func TestDeliverDueSendsARawEventAsASignedWebhookWithThePayloadAsData(t *testing.T) {
	t.Parallel()

	d := newRawDeliverHarness(t)
	ep := d.endpoint("/hook")
	sub := d.subscribeRaw("github.push", ep)
	d.advance(time.Second)
	first := d.pushRaw(map[string]any{"ref": "refs/heads/a"})
	receivedAt := d.clock.Now()
	d.advance(time.Second)

	res := d.deliverDue()
	require.Equal(t, 1, res.Claimed)
	require.Equal(t, 1, res.Sent)
	require.Equal(t, 1, res.Delivered)
	require.Zero(t, res.Enqueued, "the webhook delivery queued it, not the run")

	requests := d.rcv.requests("/hook")
	require.Len(t, requests, 1)
	g := requests[0]
	require.True(t, g.signatureValid(ep.secret))
	require.Equal(t, sub.ID, g.Header.Get("X-MCP-Subscription-Id"))
	body := g.body(t)
	require.Equal(t, "github.push", body.Name)
	require.Equal(t, g.messageID(), body.EventID)
	require.True(t, body.Timestamp.Equal(receivedAt), "the time faucet received the event, got %s", body.Timestamp)
	require.Equal(t, map[string]any{"ref": "refs/heads/a", "repository": map[string]any{"id": float64(repoID), "full_name": testRepo}}, body.Data)
	require.Equal(t, fmt.Sprintf("r1.%d", first-1), body.Cursor, "every event at or below the cursor is delivered or given up")

	second := d.pushRaw(map[string]any{"ref": "refs/heads/b"})
	require.Equal(t, 1, d.deliverDue().Delivered)
	requests = d.rcv.requests("/hook")
	require.Len(t, requests, 2)
	require.Equal(t, fmt.Sprintf("r1.%d", second-1), requests[1].body(t).Cursor)
	require.NotEqual(t, requests[0].messageID(), requests[1].messageID(), "each event has a webhook-id of its own")
	require.Zero(t, d.deliverDue().Claimed, "nothing is left to send")
}

func TestDeliverDueSendsARawSubscriptionOnlyTheFieldsItNamed(t *testing.T) {
	t.Parallel()

	d := newRawDeliverHarness(t)
	ep := d.endpoint("/hook", "fields", []string{"repository.id", "commits.id"})
	d.subscribeRaw("github.push", ep)
	d.advance(time.Second)
	d.pushRaw(map[string]any{"ref": "refs/heads/a", "commits": []any{map[string]any{"id": "1", "message": "m"}, map[string]any{"id": "2"}}})

	require.Equal(t, 1, d.deliverDue().Delivered)
	requests := d.rcv.requests("/hook")
	require.Len(t, requests, 1)
	require.Equal(t, map[string]any{
		"commits":    []any{map[string]any{"id": "1"}, map[string]any{"id": "2"}},
		"repository": map[string]any{"id": float64(repoID)},
	}, requests[0].body(t).Data)
}

func TestDeliverDueRetriesARawDeliveryOnTheScheduleWithOneMessageID(t *testing.T) {
	t.Parallel()

	d := newRawDeliverHarness(t)
	ep := d.endpoint("/hook")
	d.subscribeRaw("github.push", ep)
	d.advance(time.Second)
	d.rcv.answer("/hook", always(http.StatusInternalServerError))
	d.pushRaw(nil)

	var retried, exhausted int
	for i, wait := range []time.Duration{0, 15 * time.Second, time.Minute, 3 * time.Minute, 6 * time.Minute} {
		if wait > 0 {
			d.advance(wait - time.Second)
			d.subscribeRaw("github.push", ep)
			require.Zero(t, d.deliverDue().Sent, "attempt %d is not due one second before its backoff ends", i+1)
			d.advance(time.Second)
		}
		d.subscribeRaw("github.push", ep)
		res := d.deliverDue()
		require.Equal(t, 1, res.Sent, "attempt %d", i+1)
		retried += res.Retried
		exhausted += res.GaveUp[faucet.GiveUpAttemptsExhausted]
	}
	require.Equal(t, 4, retried, "the first four failures are retried")
	require.Equal(t, 1, exhausted, "the fifth ends the delivery, counted")

	requests := d.rcv.requests("/hook")
	require.Len(t, requests, 5)
	for _, g := range requests {
		require.Equal(t, requests[0].messageID(), g.messageID(), "every attempt has the same webhook-id")
		require.True(t, g.signatureValid(ep.secret))
	}
	d.advance(time.Hour)
	require.Zero(t, d.deliverDue().Sent, "nothing follows the fifth attempt")
}

func TestDeliverDueEndsEveryRawDeliveryItCannotSendWithAReason(t *testing.T) {
	t.Parallel()

	failOnce := func(d *deliverHarness) {
		d.rcv.answer("/hook", func(n int) int {
			if n == 1 {
				return http.StatusInternalServerError
			}
			return http.StatusOK
		})
	}
	// waiting leaves one raw delivery that failed once and is due again.
	waiting := func(d *deliverHarness) {
		d.t.Helper()
		failOnce(d)
		d.pushRaw(nil)
		require.Equal(d.t, 1, d.deliverDue().Retried)
		d.advance(15 * time.Second)
	}
	purge := func(d *deliverHarness, p faucet.RawPurge) {
		d.t.Helper()
		p.Now = d.clock.Now()
		_, err := d.store.PurgeRawEvents(d.ctx, p)
		require.NoError(d.t, err)
	}

	for name, tc := range map[string]struct {
		reason faucet.GiveUpReason
		run    func(d *deliverHarness, ep hookEndpoint) faucet.DeliverResult
	}{
		"the payload fits and the body with its envelope does not": {faucet.GiveUpEventTooLarge, func(d *deliverHarness, _ hookEndpoint) faucet.DeliverResult {
			room := 256<<10 - len(rawPayload(repoID, "", map[string]any{"blob": ""}))
			d.pushRaw(map[string]any{"blob": strings.Repeat("x", room)})
			return d.deliverDue()
		}},
		"a purge dropped the payload while the delivery waited": {faucet.GiveUpEventPurged, func(d *deliverHarness, _ hookEndpoint) faucet.DeliverResult {
			waiting(d)
			purge(d, faucet.RawPurge{ClearBefore: d.clock.Now().Add(time.Hour)})
			return d.deliverDue()
		}},
		"a purge deleted the event while the delivery waited": {faucet.GiveUpEventPurged, func(d *deliverHarness, _ hookEndpoint) faucet.DeliverResult {
			waiting(d)
			purge(d, faucet.RawPurge{DeleteBefore: d.clock.Now().Add(time.Hour)})
			return d.deliverDue()
		}},
		"the name now means another repository": {faucet.GiveUpRepoMismatch, func(d *deliverHarness, ep hookEndpoint) faucet.DeliverResult {
			waiting(d)
			d.authz.mu.Lock()
			d.authz.repos[testRepo] = deniedRepoID
			d.authz.mu.Unlock()
			d.subscribeRaw("github.push", ep)
			return d.deliverDue()
		}},
		"the endpoint answers 410": {faucet.GiveUpEndpointGone, func(d *deliverHarness, _ hookEndpoint) faucet.DeliverResult {
			d.rcv.answer("/hook", always(http.StatusGone))
			d.pushRaw(nil)
			return d.deliverDue()
		}},
		"the endpoint answers 413": {faucet.GiveUpEndpointPayloadTooLarge, func(d *deliverHarness, _ hookEndpoint) faucet.DeliverResult {
			d.rcv.answer("/hook", always(http.StatusRequestEntityTooLarge))
			d.pushRaw(nil)
			return d.deliverDue()
		}},
		"the stored body holds only some fields, and the subscription wants the whole payload": {faucet.GiveUpEventMalformed, func(d *deliverHarness, _ hookEndpoint) faucet.DeliverResult {
			_, err := d.store.IngestRaw(d.ctx, faucet.RawDelivery{DeliveryID: "projected", Event: "push", RepoID: repoID}, d.clock.Now(),
				func(interests []faucet.RawInterest) faucet.RawPlan {
					require.Len(d.t, interests, 1)
					return faucet.RawPlan{
						Body: []byte(`{"ref":"refs/heads/main"}`), Fields: []string{"ref"},
						Deliveries: []faucet.RawOutbox{{Key: interests[0].Subscription, Incarnation: interests[0].Incarnation}},
					}
				})
			require.NoError(d.t, err)
			d.rawQueued++
			return d.deliverDue()
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := newRawDeliverHarness(t)
			ep := d.endpoint("/hook")
			d.subscribeRaw("github.push", ep)
			d.advance(time.Second)

			res := tc.run(d, ep)
			require.Equal(t, map[faucet.GiveUpReason]int{tc.reason: 1}, res.GaveUp, "the delivery ends with its reason, counted")
			require.Zero(t, res.Delivered)
			gaps := len(d.rcv.envelopesOf("/hook", "gap"))
			require.LessOrEqual(t, d.rcv.total()-gaps, 1, "no request goes out for it but the one that made it wait or that the endpoint refused")
			require.True(t, d.logsContain(string(tc.reason)), "one log line names the reason")
			require.Len(t, d.rows(), 1, "the subscription stays")
			require.Zero(t, d.deliverDue().Claimed, "the delivery is over")
		})
	}

	t.Run("a payload just under the limit is sent", func(t *testing.T) {
		t.Parallel()
		d := newRawDeliverHarness(t)
		d.subscribeRaw("github.push", d.endpoint("/hook"))
		d.advance(time.Second)
		d.pushRaw(map[string]any{"blob": strings.Repeat("x", 200<<10)})
		require.Equal(t, 1, d.deliverDue().Delivered)
	})
}

func TestDeliverDueEndsARawSubscriptionWhoseSecretDoesNotOpen(t *testing.T) {
	t.Parallel()

	d := newRawDeliverHarness(t)
	ep := d.endpoint("/hook")
	sub := d.subscribeRaw("github.push", ep)
	d.advance(time.Second)
	for range 3 {
		d.pushRaw(nil)
	}
	d.store.tamperSecret(d.rows()[0].KeyHash)

	res := d.deliverDue()
	require.Equal(t, 3, res.GaveUp[faucet.GiveUpSecretUnreadable], "the delivery that was tried and the two that waited behind it")
	require.Zero(t, d.rcv.total(), "nothing is sent without the secret")
	require.Empty(t, d.rows(), "the subscription is dead, and a refresh seals the secret again")
	require.True(t, d.logsContain(sub.ID))

	d.advance(time.Second)
	d.subscribeRaw("github.push", ep)
	d.pushRaw(nil)
	require.Equal(t, 1, d.deliverDue().Delivered, "the refresh made a working subscription")
}

func TestDeliverDueEndsARawSubscriptionWhoseArgumentsDoNotDecode(t *testing.T) {
	t.Parallel()

	d := newRawDeliverHarness(t)
	ep := d.endpoint("/hook")
	d.subscribeRaw("github.push", ep)
	other := d.subscribeRaw("github.push", d.endpoint("/other"))
	d.advance(time.Second)
	d.pushRaw(nil)
	d.pushRaw(nil)
	for _, row := range d.rows() {
		if row.KeyHash.ID() != other.ID {
			d.store.corruptRawArguments(row.KeyHash)
		}
	}

	res := d.deliverDue()
	require.Equal(t, 2, res.GaveUp[faucet.GiveUpArgumentsUndecodable], "the delivery that was tried and the one that waited behind it")
	require.Equal(t, 2, res.Delivered, "the other subscription gets its events")
	require.Empty(t, d.rcv.requests("/hook"), "nothing is sent for arguments that faucet cannot read")
	require.Len(t, d.rows(), 1, "the subscription is deleted, and the other stays")
	require.True(t, d.logsContain("stored arguments do not decode"))
}

func TestDeliverDueTellsARawSubscriptionWhichEventsItMissedAtItsLimit(t *testing.T) {
	t.Parallel()

	d := newRawDeliverHarness(t)
	ep := d.endpoint("/hook")
	sub := d.subscribeRaw("github.push", ep)
	d.advance(time.Second)
	for range 100 {
		d.pushRaw(nil)
	}
	d.pushRaw(nil)
	missed := d.pushRaw(nil)
	require.Equal(t, 100, d.rawQueued, "a subscription holds 100 unsent deliveries and no more")

	res := d.deliverDue()
	require.Equal(t, 1, res.GapsSent)
	require.Zero(t, res.GapsFailed)
	require.Equal(t, 100, res.Delivered)

	gaps := d.rcv.envelopesOf("/hook", "gap")
	require.Len(t, gaps, 1, "one gap for the two events that it missed")
	members := d.requireEnvelope(gaps[0], "gap", ep.secret, sub.ID)
	require.Len(t, members, 2, "a gap body has a type and a cursor and nothing else")
	require.JSONEq(t, fmt.Sprintf(`"r1.%d"`, missed), string(members["cursor"]), "the cursor is the newest event that the subscription missed")
	line := logLine(t, d.logs.String(), "the gap envelope was sent")
	require.Contains(t, line, "the subscription missed raw events")
	require.Contains(t, line, fmt.Sprintf("missed_through=%d", missed))
	require.NotContains(t, line, "purged", "no purge is behind this gap")

	next := d.pushRaw(nil)
	res = d.deliverDue()
	require.Zero(t, res.GapsSent, "a gap is sent once")
	require.Equal(t, 1, res.Delivered, "with room again, the next event is delivered")
	requests := d.rcv.requests("/hook")
	require.Equal(t, fmt.Sprintf("r1.%d", next-1), requests[len(requests)-1].body(t).Cursor)
}

// An event over 256 KiB is never queued, and one whose payload fits and
// whose webhook body does not ends at its first attempt. Either way the
// subscription learns that it missed the event, and the cursor of the next
// event passes it.
func TestDeliverDueTellsARawSubscriptionOfAnEventTooLargeToSend(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		blob int
		// queued is 1 when the event is queued and ends as event_too_large
		// when a run builds its body.
		queued int
	}{
		"the payload is over 256 KiB":                             {300 << 10, 0},
		"the payload fits, and the body that carries it does not": {256<<10 - len(rawPayload(repoID, "", map[string]any{"blob": ""})), 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := newRawDeliverHarness(t)
			ep := d.endpoint("/hook")
			sub := d.subscribeRaw("github.push", ep)
			d.advance(time.Second)
			missed := d.pushRaw(map[string]any{"blob": strings.Repeat("x", tc.blob)})
			require.Equal(t, tc.queued, d.rawQueued)

			first := d.deliverDue()
			require.Equal(t, tc.queued, first.Claimed)
			require.Equal(t, tc.queued, first.GaveUp[faucet.GiveUpEventTooLarge])
			require.Len(t, first.GaveUp, tc.queued)
			sent := d.pushRaw(nil)
			second := d.deliverDue()
			require.Equal(t, 1, second.Delivered, "the next event that fits is delivered")
			// A gap that the run wrote can go out in the same run or the next.
			require.Equal(t, 1, first.GapsSent+second.GapsSent)
			gaps := d.rcv.envelopesOf("/hook", "gap")
			require.Len(t, gaps, 1, "the subscription learns that it missed the event")
			members := d.requireEnvelope(gaps[0], "gap", ep.secret, sub.ID)
			require.JSONEq(t, fmt.Sprintf(`"r1.%d"`, missed), string(members["cursor"]), "the cursor is the event that the subscription missed")
			// The requests without a type are the events.
			events := d.rcv.envelopesOf("/hook", "")
			require.Len(t, events, 1, "nothing goes out for the event that is too large")
			require.Equal(t, fmt.Sprintf("r1.%d", sent-1), events[0].body(t).Cursor)
			require.Zero(t, d.deliverDue().GapsSent, "the gap is sent once")
		})
	}
}

// A stored raw event that a run cannot send to the subscription ends as
// event_malformed: a projection where the subscription wants the whole
// payload, or a body that is not JSON, which no webhook body can carry. The
// subscription learns that it missed the event, and the cursor of the next
// event passes it.
func TestDeliverDueTellsARawSubscriptionOfAnEventItCannotDecode(t *testing.T) {
	t.Parallel()

	for name, stored := range map[string]faucet.RawPlan{
		"a projection, and the subscription wants the whole payload": {Body: []byte(`{"ref":"refs/heads/main"}`), Fields: []string{"ref"}},
		"a body that is not JSON":                                    {Body: []byte(`{"ref": "refs/heads/main"`)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := newRawDeliverHarness(t)
			ep := d.endpoint("/hook")
			sub := d.subscribeRaw("github.push", ep)
			d.advance(time.Second)
			ingested, err := d.store.IngestRaw(d.ctx, faucet.RawDelivery{DeliveryID: "unsendable", Event: "push", RepoID: repoID}, d.clock.Now(),
				func(interests []faucet.RawInterest) faucet.RawPlan {
					require.Len(t, interests, 1)
					plan := stored
					plan.Deliveries = []faucet.RawOutbox{{Key: interests[0].Subscription, Incarnation: interests[0].Incarnation}}
					return plan
				})
			require.NoError(t, err)
			d.rawQueued++

			first := d.deliverDue()
			require.Equal(t, map[faucet.GiveUpReason]int{faucet.GiveUpEventMalformed: 1}, first.GaveUp)
			sent := d.pushRaw(nil)
			second := d.deliverDue()
			require.Equal(t, 1, second.Delivered, "the next event is delivered")
			require.Equal(t, 1, first.GapsSent+second.GapsSent)
			gaps := d.rcv.envelopesOf("/hook", "gap")
			require.Len(t, gaps, 1, "the subscription learns that it missed the event")
			members := d.requireEnvelope(gaps[0], "gap", ep.secret, sub.ID)
			require.JSONEq(t, fmt.Sprintf(`"r1.%d"`, ingested.ID), string(members["cursor"]), "the cursor is the event that the subscription missed")
			events := d.rcv.envelopesOf("/hook", "")
			require.Len(t, events, 1, "nothing goes out for the event that cannot be sent")
			require.Equal(t, fmt.Sprintf("r1.%d", sent-1), events[0].body(t).Cursor)
		})
	}
}

// A purge takes the payload of an event whose delivery waits: a run that
// waits over an hour, while the subscriber renews and /cron/purge runs, finds
// it cleared, and a retention under an hour deletes the event. Either way the
// subscription learns that it missed the event, and the cursor of a later
// event passes it.
func TestDeliverDueTellsARawSubscriptionOfAnEventThatAPurgeTookBeforeItWasSent(t *testing.T) {
	t.Parallel()

	for name, purge := range map[string]func(d *deliverHarness, ep hookEndpoint) faucet.RawPurgeResult{
		"a purge cleared its payload": func(d *deliverHarness, ep hookEndpoint) faucet.RawPurgeResult {
			var sum faucet.RawPurgeResult
			for range 13 {
				d.advance(5 * time.Minute)
				d.subscribeRaw("github.push", ep)
				res, err := d.svc.Purge(d.ctx, d.clock.Now().Add(-30*24*time.Hour))
				require.NoError(d.t, err)
				sum.Cleared += res.Raw.Cleared
			}
			return sum
		},
		"a purge deleted it": func(d *deliverHarness, _ hookEndpoint) faucet.RawPurgeResult {
			res, err := d.svc.Purge(d.ctx, d.clock.Now())
			require.NoError(d.t, err)
			return faucet.RawPurgeResult{Deleted: res.Raw.Deleted}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := newRawDeliverHarness(t)
			ep := d.endpoint("/hook")
			sub := d.subscribeRaw("github.push", ep)
			d.advance(time.Second)
			missed := d.pushRaw(nil)
			d.advance(time.Second)
			require.NotZero(t, purge(d, ep), "the purge takes the payload while its delivery waits")
			next := d.pushRaw(nil)

			res := d.deliverDue()
			require.Equal(t, map[faucet.GiveUpReason]int{faucet.GiveUpEventPurged: 1}, res.GaveUp)
			require.Equal(t, 1, res.Delivered)
			require.Equal(t, 1, res.GapsSent)
			gaps := d.rcv.envelopesOf("/hook", "gap")
			require.Len(t, gaps, 1, "the subscription learns that it missed the event")
			members := d.requireEnvelope(gaps[0], "gap", ep.secret, sub.ID)
			require.JSONEq(t, fmt.Sprintf(`"r1.%d"`, missed), string(members["cursor"]), "the gap runs through the event that the purge took")
			// The requests without a type are the events.
			events := d.rcv.envelopesOf("/hook", "")
			require.Len(t, events, 1)
			// The run's workers claim the two deliveries at once, so the
			// next event's cursor is the watermark at its claim: below the
			// missed event while that delivery is queued, and past it once
			// that delivery ended.
			require.Contains(t, []string{fmt.Sprintf("r1.%d", missed-1), fmt.Sprintf("r1.%d", next-1)}, events[0].body(t).Cursor)

			later := d.pushRaw(nil)
			res = d.deliverDue()
			require.Equal(t, 1, res.Delivered)
			require.Zero(t, res.GapsSent, "the gap is sent once")
			events = d.rcv.envelopesOf("/hook", "")
			require.Len(t, events, 2)
			require.Equal(t, fmt.Sprintf("r1.%d", later-1), events[1].body(t).Cursor, "the cursor of a later event passes the missed one")
		})
	}
}

func TestDeliverDueDoesNotSendARawGapASecondTimeWhenTheEndpointRefusesIt(t *testing.T) {
	t.Parallel()

	d := newRawDeliverHarness(t)
	ep := d.endpoint("/hook")
	d.subscribeRaw("github.push", ep)
	d.advance(time.Second)
	for range 101 {
		d.pushRaw(nil)
	}
	d.rcv.answerEnvelope("/hook", "gap", func() int { return http.StatusInternalServerError })

	res := d.deliverDue()
	require.Equal(t, 1, res.GapsFailed)
	require.Zero(t, res.GapsSent)
	require.Equal(t, 100, res.Delivered, "the deliveries go out whatever became of the gap")
	require.Contains(t, logLine(t, d.logs.String(), "the gap envelope was not delivered"), "http_5xx")

	d.advance(time.Minute)
	res = d.deliverDue()
	require.Zero(t, res.GapsFailed+res.GapsSent, "a gap gets one attempt")
	require.Len(t, d.rcv.envelopesOf("/hook", "gap"), 1)
}

func TestDeliverDueSendsTypedAndRawDeliveriesInOneRun(t *testing.T) {
	t.Parallel()

	d := newRawDeliverHarness(t)
	d.subscribeEndpoint(d.endpoint("/typed"))
	d.subscribeRaw("github.push", d.endpoint("/raw"))
	d.advance(time.Second)
	d.newRunEvent()
	d.pushRaw(nil)

	res := d.deliverDue()
	require.Equal(t, 1, res.Enqueued, "the run queues the typed delivery, and the raw one was queued before")
	require.Equal(t, 2, res.Claimed)
	require.Equal(t, 2, res.Delivered)
	typed, raw := d.rcv.requests("/typed"), d.rcv.requests("/raw")
	require.Len(t, typed, 1)
	require.Len(t, raw, 1)
	require.Equal(t, "ci.run_completed", typed[0].body(t).Name)
	require.Regexp(t, `^v2\.\d+$`, typed[0].body(t).Cursor)
	require.Equal(t, "github.push", raw[0].body(t).Name)
	require.Regexp(t, `^r1\.\d+$`, raw[0].body(t).Cursor)
}

func TestDeliverDueStarvesNeitherKindWhenMoreIsDueThanARunTakes(t *testing.T) {
	t.Parallel()

	d := newRawDeliverHarness(t, func(deps *faucet.Deps) { deps.Webhook.MaxWorkersPerOrigin = -1 })
	for i := range 2 {
		d.subscribeEndpoint(d.endpoint(fmt.Sprintf("/typed/%d", i)))
		d.subscribeRaw("github.push", d.endpoint(fmt.Sprintf("/raw/%d", i)))
	}
	d.advance(time.Second)
	for range 100 {
		d.newRunEvent()
		d.pushRaw(nil)
	}
	count := func(kind string) int {
		return len(d.rcv.requests("/"+kind+"/0")) + len(d.rcv.requests("/"+kind+"/1"))
	}

	first := d.deliverDue()
	require.Equal(t, 200, first.Delivered, "400 deliveries are due, and a run takes 200")
	require.True(t, first.Incomplete)
	require.GreaterOrEqual(t, count("typed"), 80, "the typed deliveries get about half of the run")
	require.GreaterOrEqual(t, count("raw"), 80, "the raw deliveries get about half of the run")

	second := d.deliverDue()
	require.Equal(t, 200, second.Delivered)
	require.Equal(t, 200, count("typed"))
	require.Equal(t, 200, count("raw"))
}

func TestDeliverDueHoldsTheLimitPerOriginAcrossTypedAndRawDeliveries(t *testing.T) {
	t.Parallel()

	d := newRawDeliverHarness(t)
	d.subscribeEndpoint(d.endpoint("/typed"))
	d.subscribeRaw("github.push", d.endpoint("/raw"))
	stalled := stall(t, d.rcv, "/typed", "/raw")
	d.advance(time.Second)
	for range 8 {
		d.newRunEvent()
		d.pushRaw(nil)
	}

	wait := d.runDeliverDue()
	stalled.requireHeldAtMost(t, 2)
	stalled.release()
	res := wait()
	d.record(res)
	require.Equal(t, 16, res.Delivered)
}

func TestDeliverDueEndsTheRawDeliveriesOfASubscriptionThatIsGone(t *testing.T) {
	t.Parallel()

	for name, end := range map[string]func(d *deliverHarness, ep hookEndpoint){
		"it expired": func(d *deliverHarness, _ hookEndpoint) { d.advance(11 * time.Minute) },
		"its subscriber unsubscribed": func(d *deliverHarness, ep hookEndpoint) {
			require.NoError(d.t, d.unsubscribeRaw("github.push", ep.args, d.rcv.url(ep.path)))
		},
		"it was made again": func(d *deliverHarness, ep hookEndpoint) {
			require.NoError(d.t, d.unsubscribeRaw("github.push", ep.args, d.rcv.url(ep.path)))
			d.advance(time.Second)
			d.subscribeRaw("github.push", ep)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := newRawDeliverHarness(t)
			ep := d.endpoint("/hook")
			d.subscribeRaw("github.push", ep)
			d.advance(time.Second)
			d.pushRaw(nil)
			d.pushRaw(nil)

			end(d, ep)
			res := d.deliverDue()
			require.Equal(t, map[faucet.GiveUpReason]int{faucet.GiveUpSubscriptionExpired: 2}, res.GaveUp,
				"the deliveries end with the subscription that held them, counted")
			require.Zero(t, res.Claimed)
			require.Zero(t, d.rcv.total(), "an event from before a subscription existed is never sent to it")
		})
	}
}

// rawDeliveryDown is a RawStore whose method named down fails.
type rawDeliveryDown struct {
	faucet.RawStore
	down string
}

func (s *rawDeliveryDown) ExpireRawDeliveries(ctx context.Context, now time.Time) (int64, error) {
	if s.down == "ExpireRawDeliveries" {
		return 0, errRawStoreDown
	}
	return s.RawStore.ExpireRawDeliveries(ctx, now)
}

func (s *rawDeliveryDown) ClaimRawDelivery(ctx context.Context, now time.Time, lease time.Duration, skip []string) (faucet.RawDeliveryClaim, bool, error) {
	if s.down == "ClaimRawDelivery" {
		return faucet.RawDeliveryClaim{}, false, errRawStoreDown
	}
	return s.RawStore.ClaimRawDelivery(ctx, now, lease, skip)
}

func (s *rawDeliveryDown) FinishRawDelivery(ctx context.Context, ref faucet.DeliveryRef, now time.Time, f faucet.Finish) (faucet.FinishResult, error) {
	if s.down == "FinishRawDelivery" {
		return faucet.FinishResult{}, errRawStoreDown
	}
	return s.RawStore.FinishRawDelivery(ctx, ref, now, f)
}

func (s *rawDeliveryDown) ClaimRawGap(ctx context.Context, now time.Time, lease time.Duration, skip []string) (faucet.GapClaim, bool, error) {
	if s.down == "ClaimRawGap" {
		return faucet.GapClaim{}, false, errRawStoreDown
	}
	return s.RawStore.ClaimRawGap(ctx, now, lease, skip)
}

func (s *rawDeliveryDown) FinishRawGap(ctx context.Context, ref faucet.GapRef) error {
	if s.down == "FinishRawGap" {
		return errRawStoreDown
	}
	return s.RawStore.FinishRawGap(ctx, ref)
}

func TestDeliverDueSendsTheTypedDeliveriesWhenTheRawStoreFails(t *testing.T) {
	t.Parallel()

	for _, method := range []string{"ExpireRawDeliveries", "ClaimRawDelivery", "FinishRawDelivery", "ClaimRawGap", "FinishRawGap"} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			store := &rawDeliveryDown{}
			// No limit per origin, so that a worker that sends to the typed
			// endpoint never makes another pass over the raw work.
			d := newRawDeliverHarness(t, func(deps *faucet.Deps) {
				store.RawStore, deps.RawStore = deps.RawStore, store
				deps.Webhook.MaxWorkersPerOrigin = -1
			})
			d.subscribeEndpoint(d.endpoint("/typed"))
			d.subscribeRaw("github.push", d.endpoint("/raw"))
			d.advance(time.Second)
			d.pushRaw(map[string]any{"blob": strings.Repeat("x", 300<<10)})
			d.pushRaw(nil)
			for range 3 {
				d.newRunEvent()
			}
			store.down = method

			res := d.deliverDue()
			require.Len(t, d.rcv.requests("/typed"), 3, "every typed delivery goes out")
			require.Equal(t, 1, res.RawFailures)
			logLine(t, d.logs.String(), "a raw delivery step failed, the run goes on with the typed deliveries")
			require.Contains(t, d.logs.String(), errRawStoreDown.Error())
		})
	}
}
