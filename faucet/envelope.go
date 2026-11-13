package faucet

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"
)

const (
	envelopeGap        = "gap"
	envelopeTerminated = "terminated"
	envelopeEntropy    = 16

	failureSecretUnreadable = "secret_unreadable"
	failureEnvelopeEncoding = "envelope_malformed"
)

// gapEnvelope tells a subscription that events were lost between refreshes.
// The client persists cursor and treats it as truncated.
type gapEnvelope struct {
	Type   string `json:"type"`
	Cursor string `json:"cursor"`
}

// sendEnvelope signs one control envelope of SEP-3415 with the subscription's
// stored secret and posts it once through the guarded client. It returns ""
// when the endpoint answered 2xx, and else the category of the failure. It
// never retries: the caller counts and logs the failure. The secret is opened
// here, to sign, and goes nowhere else.
func (wh *webhookState) sendEnvelope(ctx context.Context, now time.Time, kind string, key SubscriptionKeyHash, callbackURL string, sealed []byte, envelope any) string {
	body, err := json.Marshal(envelope)
	if err != nil {
		return failureEnvelopeEncoding
	}
	secret, err := openSecret(wh.aead, sealed, key[:])
	if err != nil {
		return failureSecretUnreadable
	}
	headers, err := SignWebhook(string(secret), newEnvelopeID(kind), now, body)
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
