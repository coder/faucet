package faucet_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

func (g hookRequest) signatureCount() int {
	return len(strings.Fields(g.Header.Get("webhook-signature")))
}

func (d *deliverHarness) rotate(ep hookEndpoint, secret string) hookEndpoint {
	d.t.Helper()
	d.subscribe(subscribeTo(ep.args, d.rcv.url(ep.path), secret))
	ep.secret = secret
	return ep
}

func (d *deliverHarness) requireNoSecretInLogs(secrets ...string) {
	d.t.Helper()
	for _, secret := range secrets {
		require.NotContains(d.t, d.logs.String(), secret)
		require.NotContains(d.t, d.logs.String(), strings.TrimPrefix(secret, "whsec_"))
	}
}

type unsweptStore struct{ faucet.Store }

func (unsweptStore) ClearExpiredPreviousSecrets(context.Context, time.Time) (int64, error) {
	return 0, nil
}

func TestRotationSignsDeliveriesWithBothSecretsUntilTheWindowEnds(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	oldSecret := ep.secret
	d.subscribeEndpoint(ep)
	d.advance(time.Minute)
	latest := d.rotate(ep, newSecret(t, 32)).secret
	d.advance(time.Minute)

	d.newRunEvent()
	require.Equal(t, 1, d.deliverDue().Delivered)
	during := d.rcv.requests("/hook")[0]
	require.True(t, during.signatureValid(oldSecret), "a receiver that holds only the old secret verifies")
	require.True(t, during.signatureValid(latest), "a receiver that holds only the new secret verifies")
	require.Equal(t, 2, during.signatureCount())

	d.advance(8 * time.Minute)
	d.rotate(ep, latest)
	d.advance(2 * time.Minute)
	d.newRunEvent()
	require.Equal(t, 1, d.deliverDue().Delivered)
	after := d.rcv.requests("/hook")[1]
	require.False(t, after.signatureValid(oldSecret), "a refresh with the same secret did not extend the window")
	require.True(t, after.signatureValid(latest))
	require.Equal(t, 1, after.signatureCount())
	d.requireNoSecretInLogs(oldSecret, latest)
}

func TestRefreshWithTheSameSecretStartsNoWindow(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	d.subscribeEndpoint(ep)
	d.advance(time.Minute)
	d.subscribeEndpoint(ep)

	d.newRunEvent()
	require.Equal(t, 1, d.deliverDue().Delivered)
	got := d.rcv.requests("/hook")[0]
	require.True(t, got.signatureValid(ep.secret))
	require.Equal(t, 1, got.signatureCount())
}

func TestSecondRotationKeepsOnlyTheSecretItReplaced(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	first := ep.secret
	d.subscribeEndpoint(ep)
	d.advance(time.Minute)
	second := d.rotate(ep, newSecret(t, 32)).secret
	d.advance(time.Minute)
	third := d.rotate(ep, newSecret(t, 32)).secret

	d.newRunEvent()
	require.Equal(t, 1, d.deliverDue().Delivered)
	got := d.rcv.requests("/hook")[0]
	require.True(t, got.signatureValid(third))
	require.True(t, got.signatureValid(second))
	require.False(t, got.signatureValid(first), "one previous secret is kept, and it is the one the last rotation replaced")
	require.Equal(t, 2, got.signatureCount())
}

func TestDeliveryAfterTheWindowIsSignedWithTheNewSecretEvenWhenNoSweepRan(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t, func(deps *faucet.Deps) { deps.Store = unsweptStore{Store: deps.Store} })
	ep := d.endpoint("/hook")
	oldSecret := ep.secret
	d.subscribeEndpoint(ep)
	d.advance(time.Minute)
	latest := d.rotate(ep, newSecret(t, 32)).secret
	d.advance(9 * time.Minute)
	d.rotate(ep, latest)
	d.advance(2 * time.Minute)
	require.NotNil(t, d.rows()[0].PreviousSecretEnc, "the sweep did not run, so the row still holds the previous secret")

	d.newRunEvent()
	require.Equal(t, 1, d.deliverDue().Delivered)
	got := d.rcv.requests("/hook")[0]
	require.False(t, got.signatureValid(oldSecret), "a window that ended signs nothing, swept or not")
	require.True(t, got.signatureValid(latest))
	require.Equal(t, 1, got.signatureCount())
}

func TestDeliverDueClearsThePreviousSecretWhenItsWindowEnds(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	oldSecret := ep.secret
	d.subscribeEndpoint(ep)
	d.advance(time.Minute)
	latest := d.rotate(ep, newSecret(t, 32)).secret

	row := d.rows()[0]
	require.NotEmpty(t, row.PreviousSecretEnc)
	for _, secret := range []string{oldSecret, latest} {
		for _, form := range [][]byte{[]byte(secret), []byte(strings.TrimPrefix(secret, "whsec_")), decodedSecret(t, secret)} {
			require.False(t, bytes.Contains(row.PreviousSecretEnc, form), "the previous secret is sealed")
			require.False(t, bytes.Contains(row.SecretEnc, form), "the current secret is sealed")
		}
	}
	previous := row
	previous.SecretEnc = row.PreviousSecretEnc
	opened, err := openSealed(t, testWebhookKey, previous)
	require.NoError(t, err)
	require.Equal(t, oldSecret, opened, "the previous secret is the old one, sealed with the row's key hash")

	require.Zero(t, d.deliverDue().PreviousSecretsCleared, "the window still runs")
	require.NotNil(t, d.rows()[0].PreviousSecretEnc)

	d.advance(10 * time.Minute)
	require.Equal(t, 1, d.deliverDue().PreviousSecretsCleared)
	cleared := d.rows()
	require.Len(t, cleared, 1, "the subscription is untouched")
	require.Nil(t, cleared[0].PreviousSecretEnc)
	require.True(t, cleared[0].PreviousSecretUntil.IsZero())
	require.Zero(t, d.deliverDue().PreviousSecretsCleared, "a second run has nothing to clear")
}

func TestGapEnvelopeIsSignedWithBothSecretsDuringTheWindow(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	oldSecret := ep.secret
	sub, _, _ := d.purgedBeforeRead(ep)
	latest := d.rotate(ep, newSecret(t, 32)).secret

	require.Equal(t, 1, d.deliverDue().GapsSent)
	requests := d.rcv.requests("/hook")
	require.Len(t, requests, 2, "one gap envelope and one event")
	for _, secret := range []string{oldSecret, latest} {
		d.requireEnvelope(requests[0], "gap", secret, sub.ID)
		require.True(t, requests[1].signatureValid(secret))
	}
	require.Equal(t, 2, requests[0].signatureCount())
	d.requireNoSecretInLogs(oldSecret, latest)
}

func TestTerminatedEnvelopeIsSignedWithBothStoredSecretsAndNotTheRenewals(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	oldSecret := ep.secret
	sub := d.subscribeEndpoint(ep)
	d.advance(time.Minute)
	latest := d.rotate(ep, newSecret(t, 32)).secret
	d.revoke()
	renewalSecret := newSecret(t, 32)

	d.subscribeError(d.renewalParams(ep, renewalSecret))
	requests := d.rcv.requests("/hook")
	require.Len(t, requests, 1)
	for _, secret := range []string{oldSecret, latest} {
		d.requireEnvelope(requests[0], "terminated", secret, sub.ID)
	}
	require.False(t, requests[0].signatureValid(renewalSecret))
	require.Equal(t, 2, requests[0].signatureCount())
	d.requireNoSecretInLogs(oldSecret, latest, renewalSecret)
}
