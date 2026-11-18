package faucet_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
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
		"the payload is over 256 KiB when it arrives": {faucet.GiveUpEventTooLarge, func(d *deliverHarness, _ hookEndpoint) faucet.DeliverResult {
			d.pushRaw(map[string]any{"blob": strings.Repeat("x", 300<<10)})
			return d.deliverDue()
		}},
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
			require.LessOrEqual(t, d.rcv.total(), 1, "no request goes out for it but the one that made it wait or that the endpoint refused")
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
