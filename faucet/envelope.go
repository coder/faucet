package faucet

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"cdr.dev/slog/v3"
)

const (
	envelopeGap        = "gap"
	envelopeTerminated = "terminated"
	envelopeEntropy    = 16

	failureSecretUnreadable = "secret_unreadable"
	failureEnvelopeEncoding = "envelope_malformed"
)

// terminatedEnvelope tells a subscription that it has ended and no longer
// exists. The error is the SEP's example for revoked access: fixed text that
// says nothing about the repository.
type terminatedEnvelope struct {
	Type  string          `json:"type"`
	Error terminatedError `json:"error"`
}

type terminatedError struct {
	Code    int64             `json:"code"`
	Message string            `json:"message"`
	Data    map[string]string `json:"data"`
}

// Terminations counts the terminated envelopes that a Service has sent since it
// was built. Failed are the ones the endpoint did not answer with 2xx: the
// subscription is gone either way, and a failed envelope is not retried.
type Terminations struct {
	Sent, Failed uint64
}

// gapEnvelope tells a subscription that events were lost between refreshes.
// The client persists cursor and treats it as truncated.
type gapEnvelope struct {
	Type   string `json:"type"`
	Cursor string `json:"cursor"`
}

// sendEnvelope signs one control envelope of SEP-3415 with the subscription's
// stored secrets, which are the current one and, during a rotation window, the
// previous one, and posts it once through the guarded client. It returns ""
// when the endpoint answered 2xx, and else the category of the failure. It
// never retries: the caller counts and logs the failure. The secrets are opened
// here, to sign, and go nowhere else.
func (wh *webhookState) sendEnvelope(ctx context.Context, now time.Time, kind string, key SubscriptionKeyHash, callbackURL string, sealed sealedSecrets, envelope any) string {
	body, err := json.Marshal(envelope)
	if err != nil {
		return failureEnvelopeEncoding
	}
	secrets, err := wh.signingSecrets(now, key, sealed)
	if err != nil {
		return failureSecretUnreadable
	}
	headers, err := signWebhook(secrets, newEnvelopeID(kind), now, body)
	if err != nil {
		return failureSecretUnreadable
	}
	status, category := wh.post(ctx, callbackURL, key.ID(), headers, body)
	switch {
	case status >= 200 && status <= 299:
		return ""
	case status >= 500:
		return string(CallbackHTTP5xx)
	case status > 0:
		return string(CallbackHTTP4xx)
	}
	return string(category)
}

// newEnvelopeID is the webhook-id of a control envelope, msg_<type>_<random>.
// It is fresh per message and shares nothing with the subscription's secret.
func newEnvelopeID(kind string) string {
	var random [envelopeEntropy]byte
	_, _ = rand.Read(random[:])
	return messageIDPrefix + kind + "_" + base64.RawURLEncoding.EncodeToString(random[:])
}

// post sends one request and returns the status, or the category of the
// failure. Nothing the endpoint sent is kept, and the error of the client,
// which quotes the URL, is dropped.
func (wh *webhookState) post(ctx context.Context, callbackURL, subscriptionID string, headers http.Header, body []byte) (status int, category CallbackReason) {
	reqCtx, cancel := context.WithTimeout(ctx, callbackRequestTimeout+time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, callbackURL, bytes.NewReader(body))
	if err != nil {
		return 0, CallbackConnectionRefused
	}
	req.Header = headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", deliveryAgent)
	req.Header.Set(subscriptionIDHeader, subscriptionID)
	resp, err := wh.client.Do(req)
	if err != nil {
		return 0, deliveryFailureOf(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, ""
}

// endRevokedSubscription ends the subscription that a renewal of the same
// principal, callback URL, event type and arguments names, after the access
// check of that renewal said the principal can no longer read the repository.
// The conditional delete is the send token: the one caller whose delete removes
// the live row sends the terminated envelope, signed with the stored secret
// and posted once, and any other caller finds no row and sends nothing. A
// failure is logged and counted. The send does not follow the request's
// cancellation, because the row is already gone and the envelope is the
// subscription's only notice.
func (s *Service) endRevokedSubscription(ctx context.Context, logger slog.Logger, wh *webhookState, principal Principal, req subscribeRequest) {
	key := NewSubscriptionKeyHash(principal.Login, req.callbackURL, req.eventType.name, req.canonical)
	row, found, err := s.store.DeleteLiveSubscription(ctx, principal.Login, key, s.clock.Now())
	if err != nil {
		logger.Error(ctx, "end a subscription whose access was revoked", slog.Error(err))
		return
	}
	if !found {
		return
	}
	logger = logger.With(slog.F("subscription", key.ID()), slog.F("login", principal.Login))
	failure := wh.sendEnvelope(context.WithoutCancel(ctx), s.clock.Now(), envelopeTerminated, key, row.CallbackURL, row.sealedSecrets(),
		terminatedEnvelope{Type: envelopeTerminated, Error: terminatedError{Code: codeForbidden, Message: "Forbidden", Data: map[string]string{"reason": "Access revoked"}}})
	if failure == "" {
		wh.terminated.sent.Add(1)
		logger.Info(ctx, "the subscription ended, access revoked, the terminated envelope was sent")
		return
	}
	wh.terminated.failed.Add(1)
	logger.Warn(ctx, "the subscription ended, access revoked, the terminated envelope was not delivered", slog.F("reason", failure))
}

// Terminations returns the counts of the terminated envelopes that this Service
// has sent. A Service without webhook delivery sent none.
func (s *Service) Terminations() Terminations {
	if s.webhook == nil {
		return Terminations{}
	}
	return Terminations{Sent: s.webhook.terminated.sent.Load(), Failed: s.webhook.terminated.failed.Load()}
}
