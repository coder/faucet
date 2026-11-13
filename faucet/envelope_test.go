package faucet_test

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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
