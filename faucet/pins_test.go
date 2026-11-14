package faucet_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

func TestTerminatedEnvelopeIsSentWhenTheRenewingClientCancels(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	d.subscribeEndpoint(ep)
	d.advance(time.Second)
	d.revoke()

	ctx, cancel := context.WithCancel(d.ctx)
	defer cancel()
	d.rcv.answerEnvelope("/hook", "terminated", func() int {
		cancel()
		time.Sleep(100 * time.Millisecond)
		return http.StatusOK
	})
	_, err := mcp.CallCustomMethod[*rawParams, *rawResult](ctx, d.session, methodSubscribe, d.renewalParams(ep, ep.secret))
	require.Error(t, err, "the client gave up on the renewal")

	require.Eventually(t, func() bool { return d.svc.Terminations() == faucet.Terminations{Sent: 1} }, 5*time.Second, 10*time.Millisecond,
		"the subscription is already deleted, so its only notice goes out and is counted as sent")
	require.Empty(t, d.rows())
}

func TestEveryEnvelopeGetsAFreshRandomWebhookID(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	d.ledgerSkip = true
	a, b := d.endpoint("/a"), d.endpoint("/b")
	d.subscribeEndpoint(a)
	d.subscribeEndpoint(b)
	for range 2 {
		d.advance(time.Second)
		d.newRunEvent()
		d.advance(2 * time.Minute)
		d.newRunEvent()
		_, err := d.store.Purge(d.ctx, d.clock.Now().Add(-time.Minute))
		require.NoError(t, err)
		require.Equal(t, 2, d.deliverDue().GapsSent)
	}
	d.revoke()
	d.subscribeError(d.renewalParams(a, a.secret))

	gaps := append(d.rcv.envelopesOf("/a", "gap"), d.rcv.envelopesOf("/b", "gap")...)
	terminated := d.rcv.envelopesOf("/a", "terminated")
	require.Len(t, gaps, 4)
	require.Len(t, terminated, 1)
	seen := map[string]bool{}
	for _, envelope := range append(gaps, terminated...) {
		id := envelope.messageID()
		require.False(t, seen[id], "webhook-id %s repeats: a receiver that deduplicates on it would drop the envelope", id)
		seen[id] = true
	}
	require.Regexp(t, regexp.MustCompile(`^msg_terminated_[A-Za-z0-9_-]{22}$`), terminated[0].messageID())
}

func messageIDOf(idKey []byte, subscription faucet.SubscriptionKeyHash, eventID int64) string {
	mac := hmac.New(sha256.New, idKey)
	_, _ = mac.Write(subscription[:])
	_, _ = mac.Write([]byte(strconv.FormatInt(eventID, 10)))
	return "msg_" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
}

func hmacOf(key []byte, label string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(label))
	return mac.Sum(nil)
}

func TestWebhookIDIsKeyedByTheOperatorsKeyApartFromTheSealingKey(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	d.subscribeEndpoint(d.endpoint("/hook"))
	d.advance(time.Second)
	event := d.newRunEvent()
	require.Equal(t, 1, d.deliverDue().Delivered)
	got := d.rcv.requests("/hook")[0].messageID()
	subscription := d.rows()[0].KeyHash

	require.Equal(t, messageIDOf(hmacOf(testWebhookKey, "faucet webhook-id v1"), subscription, event.ID), got,
		"a second faucet that holds the same key, such as a failover, gives the same webhook-id")
	require.NotEqual(t, messageIDOf(testWebhookKey, subscription, event.ID), got, "the key that seals secrets does not make the ids")
	require.NotEqual(t, messageIDOf(hmacOf(testWebhookKey, "faucet secret nonce v1"), subscription, event.ID), got,
		"neither does the key that derives the nonces of sealed secrets")
	otherKey := hmacOf(testWebhookKey, "another key")
	require.NotEqual(t, messageIDOf(hmacOf(otherKey, "faucet webhook-id v1"), subscription, event.ID), got,
		"a receiver or a reader of the table cannot compute the id without the key")
}

func TestRotationWindowEndsWithTheTTLThatTheRotatingCallGotWhenThatIsShorter(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	oldSecret := ep.secret
	d.subscribeEndpoint(ep)
	d.advance(time.Minute)
	rotating := subscribeTo(ep.args, d.rcv.url(ep.path), newSecret(t, 32))
	rotating.TTLMs = raw(2 * 60 * 1000)
	d.subscribe(rotating)
	rotatedAt := d.clock.Now()
	latest := hookEndpoint{path: ep.path, secret: decodeSecretFromParams(t, rotating), args: ep.args}

	row := d.rows()[0]
	require.True(t, row.PreviousSecretUntil.Equal(row.ExpiresAt), "the window ends with the 2 minutes that the call got, not 10: %s", row.PreviousSecretUntil.Sub(rotatedAt))

	d.advance(30 * time.Second)
	d.newRunEvent()
	require.Equal(t, 1, d.deliverDue().Delivered)
	inside := d.rcv.requests("/hook")[0]
	require.True(t, inside.signatureValid(oldSecret) && inside.signatureValid(latest.secret), "inside the window both secrets sign")

	d.advance(time.Minute)
	d.rotate(latest, latest.secret)
	d.advance(31 * time.Second)
	require.Greater(t, d.clock.Now().Sub(rotatedAt), 2*time.Minute)
	d.newRunEvent()
	require.Equal(t, 1, d.deliverDue().Delivered)
	after := d.rcv.requests("/hook")[1]
	require.True(t, after.signatureValid(latest.secret))
	require.False(t, after.signatureValid(oldSecret), "the renewal kept the subscription alive past the window, and the old secret stopped signing at the TTL")
}

func decodeSecretFromParams(t *testing.T, p *rawParams) string {
	t.Helper()
	var delivery struct {
		Secret string `json:"secret"`
	}
	require.NoError(t, json.Unmarshal(p.Delivery, &delivery))
	return delivery.Secret
}

func TestHandshakeOfARenewalThatRotatesIsSignedWithTheNewSecretOnly(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	d.subscribeEndpoint(ep)
	restarted := newDeliverHarness(t, func(deps *faucet.Deps) {
		deps.Store = d.store
		deps.Clock = d.clock
	})
	restarted.ledgerSkip = true
	newer := newSecret(t, 32)
	restarted.subscribe(subscribeTo(ep.args, d.rcv.url(ep.path), newer))

	require.Len(t, restarted.verifier.requests, 1, "a faucet that has not verified the URL yet does it on the renewal")
	body := []byte(`{"type":"verification"}`)
	headers, err := restarted.verifier.requests[0].Sign("msg_probe", restarted.clock.Now(), body)
	require.NoError(t, err)
	probe := hookRequest{Header: headers, Body: body}
	require.True(t, probe.signatureValid(newer))
	require.False(t, probe.signatureValid(ep.secret), "a receiver that holds only the old secret cannot answer the handshake")
	require.Equal(t, 1, probe.signatureCount())
}
