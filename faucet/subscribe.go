package faucet

import (
	"bytes"
	"context"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/quartz"
)

const (
	methodEventsSubscribe   = "events/subscribe"
	methodEventsUnsubscribe = "events/unsubscribe"

	deliveryWebhook = "webhook"

	codeResourceExhausted = -32025
	codeUnsupported       = -32026
	codeCallbackEndpoint  = -32027

	// maxSubscriptionTTL is the longest grant. Every renewal checks repository
	// access again, so it also bounds how long a revoked reader keeps receiving.
	maxSubscriptionTTL = 10 * time.Minute
	// minSubscriptionTTL protects the server from refresh storms. SEP-3415
	// allows a floor.
	minSubscriptionTTL = 30 * time.Second
	// maxLiveSubscriptions caps one principal's live subscriptions, so a token
	// cannot fill the table.
	maxLiveSubscriptions = 100

	secretPrefix             = "whsec_"
	minSubscriberSecretBytes = 24
	maxSubscriberSecretBytes = 64
	// maxSecretChars is len(secretPrefix) plus the padded base64 of 64 bytes.
	maxSecretChars = 6 + 88
	maxModeBytes   = 64
)

var errSubscriptionStore = xerrors.New("could not store the subscription, try again")

// subscriberSecret is the plaintext signing secret of a subscription. It lives
// for one request. No fmt verb prints it.
type subscriberSecret string

func (subscriberSecret) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprint(f, "[redacted]")
}

type webhookState struct {
	aead                  cipher.AEAD
	callbacks             CallbackVerifier
	allowInsecureLoopback bool
	verified              *callbackCache
}

func newWebhookState(d WebhookDeps, clock quartz.Clock) (*webhookState, error) {
	switch {
	case d.Key == (WebhookKey{}):
		return nil, xerrors.New("webhook key is required")
	case d.Callbacks == nil:
		return nil, xerrors.New("callback verifier is required")
	}
	aead, err := d.Key.aead()
	if err != nil {
		return nil, err
	}
	return &webhookState{aead: aead, callbacks: d.Callbacks, allowInsecureLoopback: d.AllowInsecureLoopback, verified: newCallbackCache(clock)}, nil
}

type eventsSubscribeParams struct {
	mcp.ParamsBase
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	// Delivery is parsed by parseDelivery, not by the SDK, because an SDK decode
	// error could quote the secret.
	Delivery json.RawMessage `json:"delivery,omitempty"`
	Cursor   *string         `json:"cursor,omitempty"`
	MaxAgeMs *int64          `json:"maxAgeMs,omitempty"`
	TTLMs    json.RawMessage `json:"ttlMs,omitempty"`
}

type eventsSubscribeResult struct {
	mcp.ResultBase
	ResultType    string `json:"resultType"`
	ID            string `json:"id"`
	RefreshBefore string `json:"refreshBefore"`
	Cursor        string `json:"cursor"`
	Truncated     bool   `json:"truncated"`
}

type eventsUnsubscribeParams struct {
	mcp.ParamsBase
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Delivery  json.RawMessage `json:"delivery,omitempty"`
}

type eventsUnsubscribeResult struct {
	mcp.ResultBase
	ResultType string `json:"resultType"`
}

type subscribeRequest struct {
	eventType   eventType
	arguments   pollArguments
	canonical   json.RawMessage
	callbackURL string
	secret      subscriberSecret
	cursor      *int64
	maxAge      time.Duration
	ttl         time.Duration
}

func (s *Service) requireWebhook() (*webhookState, error) {
	if s.webhook == nil {
		return nil, rpcError(codeUnsupported, "webhook delivery is not supported by this server",
			map[string]string{"feature": "deliveryMode", "value": deliveryWebhook})
	}
	return s.webhook, nil
}

func (s *Service) subscribeMethod(ctx context.Context, _ *mcp.ServerSession, p *eventsSubscribeParams) (*eventsSubscribeResult, error) {
	wh, err := s.requireWebhook()
	if err != nil {
		return nil, err
	}
	req, err := parseSubscribe(p, wh.allowInsecureLoopback)
	if err != nil {
		return nil, err
	}
	logger := s.logger.With(slog.F("repo", req.arguments.Repo), slog.F("event", req.eventType.name))

	principal, err := s.subscriber(ctx, logger, "subscribe")
	if err != nil {
		return nil, err
	}
	repoID, err := s.authorizeRepo(ctx, logger, principal, "subscribe", req.arguments.Repo)
	if err != nil {
		return nil, accessError(err)
	}
	if err := wh.verifyCallback(ctx, logger, principal, req.callbackURL); err != nil {
		return nil, err
	}

	now := s.clock.Now()
	start, truncated, err := s.subscriptionStart(ctx, now, req)
	if err != nil {
		return nil, subscriptionStoreError(ctx, logger, err)
	}
	key := NewSubscriptionKeyHash(principal.Login, req.callbackURL, req.eventType.name, req.canonical)
	sealed, err := sealSecret(wh.aead, req.secret, key[:])
	if err != nil {
		logger.Error(ctx, "seal subscriber secret", slog.Error(err))
		return nil, rpcError(jsonrpc.CodeInternalError, errSubscriptionStore.Error(), nil)
	}
	row, err := s.store.UpsertSubscription(ctx, Subscription{
		KeyHash:     key,
		Principal:   principal.Login,
		CallbackURL: req.callbackURL,
		EventType:   req.eventType.name,
		Arguments:   req.canonical,
		RepoID:      repoID,
		SecretEnc:   sealed,
		Cursor:      start,
		ExpiresAt:   now.Add(req.ttl).Truncate(time.Second),
	}, now, maxLiveSubscriptions)
	switch {
	case xerrors.Is(err, ErrSubscriptionLimit):
		logger.Info(ctx, "subscribe refused, limit reached", slog.F("login", principal.Login))
		return nil, rpcError(codeResourceExhausted, "subscription limit reached",
			map[string]any{"limit": "subscriptions", "max": maxLiveSubscriptions})
	case err != nil:
		return nil, subscriptionStoreError(ctx, logger, err)
	}
	logger.Info(ctx, "subscribed",
		slog.F("login", principal.Login), slog.F("subscription", key.ID()), slog.F("expires_at", row.ExpiresAt))
	return &eventsSubscribeResult{
		ResultType:    resultComplete,
		ID:            key.ID(),
		RefreshBefore: row.ExpiresAt.UTC().Format(time.RFC3339),
		Cursor:        encodeCursor(row.Cursor),
		// A row that kept its own cursor did not restart, so nothing was skipped.
		Truncated: truncated && row.Cursor == start,
	}, nil
}

func (s *Service) unsubscribeMethod(ctx context.Context, _ *mcp.ServerSession, p *eventsUnsubscribeParams) (*eventsUnsubscribeResult, error) {
	wh, err := s.requireWebhook()
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, invalidParams("name is required")
	}
	t, args, err := parseSubscriptionName(p.Name, p.Arguments)
	if err != nil {
		return nil, err
	}
	d, err := decodeDelivery(p.Delivery)
	if err != nil {
		return nil, err
	}
	callbackURL, err := d.callbackURL(wh.allowInsecureLoopback)
	if err != nil {
		return nil, err
	}
	logger := s.logger.With(slog.F("repo", args.Repo), slog.F("event", t.name))

	principal, err := s.subscriber(ctx, logger, "unsubscribe")
	if err != nil {
		return nil, err
	}
	key := NewSubscriptionKeyHash(principal.Login, callbackURL, t.name, canonicalArguments(args))
	deleted, err := s.store.DeleteSubscription(ctx, principal.Login, key)
	if err != nil {
		return nil, subscriptionStoreError(ctx, logger, err)
	}
	if !deleted {
		return nil, rpcError(codeNotFound, "no such subscription", map[string]string{"kind": "subscription"})
	}
	logger.Info(ctx, "unsubscribed", slog.F("login", principal.Login), slog.F("subscription", key.ID()))
	return &eventsUnsubscribeResult{ResultType: resultComplete}, nil
}

// subscriber returns the authenticated caller. Subscriptions are keyed by the
// login, so a principal without one is no principal.
func (s *Service) subscriber(ctx context.Context, logger slog.Logger, op string) (Principal, error) {
	p, err := s.principal(ctx, logger, op)
	if err == nil && p.Login == "" {
		logger.Info(ctx, op+" by a principal without a login")
		err = errAuthRequired
	}
	if err != nil {
		return Principal{}, accessError(err)
	}
	return p, nil
}

// verifyCallback asks the host's verifier unless it accepted this pair in the
// last callbackVerificationTTL.
func (wh *webhookState) verifyCallback(ctx context.Context, logger slog.Logger, p Principal, callbackURL string) error {
	if wh.verified.verified(p.Login, callbackURL) {
		return nil
	}
	err := wh.callbacks.VerifyCallback(ctx, p, callbackURL)
	var cbErr *CallbackError
	switch {
	case err == nil:
		wh.verified.remember(p.Login, callbackURL)
		return nil
	case xerrors.As(err, &cbErr) && cbErr.Reason.valid():
		logger.Info(ctx, "callback not verified", slog.F("login", p.Login), slog.F("reason", string(cbErr.Reason)))
		message := cbErr.Message
		if message == "" {
			message = "callback endpoint failed verification"
		}
		return rpcError(codeCallbackEndpoint, message, map[string]string{"reason": string(cbErr.Reason)})
	default:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logger.Warn(ctx, "could not verify the callback", slog.F("login", p.Login), slog.Error(err))
		return rpcError(jsonrpc.CodeInternalError, "could not verify the callback endpoint, try again", nil)
	}
}

// subscriptionStart picks where delivery begins and whether that skips events
// the caller asked for. No cursor means now. A cursor below the oldest event
// the store can still serve, which is the purge mark or the maxAgeMs floor, is
// moved up to it and reported as truncated.
func (s *Service) subscriptionStart(ctx context.Context, now time.Time, req subscribeRequest) (start int64, truncated bool, err error) {
	if req.cursor == nil {
		head, err := s.store.CursorAt(ctx, now)
		return head, false, err
	}
	var floorTime time.Time
	if req.maxAge > 0 {
		floorTime = now.Add(-req.maxAge)
	}
	floor, err := s.store.CursorAt(ctx, floorTime)
	if err != nil {
		return 0, false, err
	}
	if *req.cursor < floor {
		return floor, true, nil
	}
	return *req.cursor, false, nil
}

func subscriptionStoreError(ctx context.Context, logger slog.Logger, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	logger.Error(ctx, "store subscription", slog.Error(err))
	return rpcError(jsonrpc.CodeInternalError, errSubscriptionStore.Error(), nil)
}

func parseSubscribe(p *eventsSubscribeParams, allowInsecureLoopback bool) (subscribeRequest, error) {
	if p == nil {
		return subscribeRequest{}, invalidParams("name is required")
	}
	t, args, err := parseSubscriptionName(p.Name, p.Arguments)
	if err != nil {
		return subscribeRequest{}, err
	}
	d, err := decodeDelivery(p.Delivery)
	if err != nil {
		return subscribeRequest{}, err
	}
	callbackURL, err := d.callbackURL(allowInsecureLoopback)
	if err != nil {
		return subscribeRequest{}, err
	}
	secret, err := d.subscriberSecret()
	if err != nil {
		return subscribeRequest{}, err
	}
	req := subscribeRequest{eventType: t, arguments: args, canonical: canonicalArguments(args), callbackURL: callbackURL, secret: secret}
	if req.ttl, err = parseTTL(p.TTLMs); err != nil {
		return subscribeRequest{}, err
	}
	if p.Cursor != nil {
		id, ok := decodeCursor(*p.Cursor)
		if !ok {
			return subscribeRequest{}, invalidParams("cursor is not one this server returned")
		}
		req.cursor = &id
	}
	if p.MaxAgeMs != nil {
		if *p.MaxAgeMs < 1 {
			return subscribeRequest{}, invalidParams("maxAgeMs must be positive")
		}
		req.maxAge = time.Duration(min(*p.MaxAgeMs, maxAgeLimitMs)) * time.Millisecond
	}
	return req, nil
}

func parseSubscriptionName(name string, arguments json.RawMessage) (eventType, pollArguments, error) {
	if name == "" {
		return eventType{}, pollArguments{}, invalidParams("name is required")
	}
	t, err := eventByName(name)
	if err != nil {
		return eventType{}, pollArguments{}, err
	}
	args, err := parsePollArguments(arguments)
	if err != nil {
		return eventType{}, pollArguments{}, err
	}
	return t, args, nil
}

// canonicalArguments is the one JSON spelling of args. Fields come in struct
// order without spaces, so two requests that differ only in key order or
// whitespace address the same subscription.
func canonicalArguments(args pollArguments) json.RawMessage {
	return marshalRaw(struct {
		Repo string `json:"repo"`
		SHA  string `json:"sha,omitempty"`
		PR   int32  `json:"pr,omitempty"`
	}{args.Repo, args.SHA, args.PR})
}

type deliveryParams struct {
	Mode   *string `json:"mode"`
	URL    *string `json:"url"`
	Secret *string `json:"secret"`
}

// decodeDelivery reads the delivery object. Every error is fixed text, so none
// can quote the secret or the URL.
func decodeDelivery(raw json.RawMessage) (deliveryParams, error) {
	var d deliveryParams
	if len(raw) == 0 || json.Unmarshal(raw, &d) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return deliveryParams{}, invalidParams("delivery must be an object with string fields")
	}
	if d.Mode != nil && *d.Mode != deliveryWebhook {
		if len(*d.Mode) > maxModeBytes {
			return deliveryParams{}, invalidParams("delivery.mode is not a delivery mode")
		}
		return deliveryParams{}, rpcError(codeUnsupported, "events/subscribe supports only the webhook delivery mode",
			map[string]string{"feature": "deliveryMode", "value": *d.Mode})
	}
	return d, nil
}

func (d deliveryParams) callbackURL(allowInsecureLoopback bool) (string, error) {
	if d.URL == nil {
		return "", invalidParams("delivery.url is required")
	}
	u, err := parseCallbackURL(*d.URL, allowInsecureLoopback)
	if err != nil {
		return "", invalidParams("delivery.url %v", err)
	}
	return u.String(), nil
}

func (d deliveryParams) subscriberSecret() (subscriberSecret, error) {
	if d.Secret == nil {
		return "", invalidParams("delivery.secret is required")
	}
	return parseSecret(*d.Secret)
}

// parseSecret accepts the Standard Webhooks form: whsec_ and the standard,
// padded base64 of 24 to 64 bytes. The error names the rule and never the
// value.
func parseSecret(s string) (subscriberSecret, error) {
	bad := invalidParams("delivery.secret must be %s followed by the base64 of %d to %d random bytes", secretPrefix, minSubscriberSecretBytes, maxSubscriberSecretBytes)
	encoded, ok := strings.CutPrefix(s, secretPrefix)
	if !ok || len(s) > maxSecretChars || strings.IndexFunc(encoded, func(r rune) bool { return !isBase64Char(r) }) >= 0 {
		return "", bad
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(decoded) < minSubscriberSecretBytes || len(decoded) > maxSubscriberSecretBytes {
		return "", bad
	}
	return subscriberSecret(s), nil
}

func isBase64Char(r rune) bool {
	return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '+' || r == '/' || r == '='
}

// parseTTL turns the suggestion into a grant. Absent, null and anything above
// the cap give the cap, and anything below the floor gives the floor. SEP-3415
// has no rejection path for TTL values.
func parseTTL(raw json.RawMessage) (time.Duration, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return maxSubscriptionTTL, nil
	}
	ms, err := strconv.ParseInt(string(trimmed), 10, 64)
	switch {
	case xerrors.Is(err, strconv.ErrRange):
		ms = math.MaxInt64
		if trimmed[0] == '-' {
			ms = math.MinInt64
		}
	case err != nil:
		return 0, invalidParams("ttlMs must be an integer or null")
	}
	return max(minSubscriptionTTL, time.Duration(min(ms, maxSubscriptionTTL.Milliseconds()))*time.Millisecond), nil
}
