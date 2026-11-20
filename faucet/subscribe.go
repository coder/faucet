package faucet

import (
	"bytes"
	"context"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
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
	client                *http.Client
	idKey                 []byte
	nonceKey              []byte
	workersPerOrigin      int
	logger                slog.Logger
	terminated            struct{ sent, failed atomic.Uint64 }
	warned                windowWarnings
}

func newWebhookState(d WebhookDeps, clock quartz.Clock, logger slog.Logger) (*webhookState, error) {
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
	client := d.Client
	if client == nil {
		client = NewHTTPClient(HTTPClientOptions{AllowInsecureLoopback: d.AllowInsecureLoopback})
	}
	workersPerOrigin := d.MaxWorkersPerOrigin
	if workersPerOrigin == 0 {
		workersPerOrigin = defaultMaxWorkersPerOrigin
	}
	return &webhookState{aead: aead, callbacks: d.Callbacks, allowInsecureLoopback: d.AllowInsecureLoopback, verified: newCallbackCache(clock), client: client, idKey: d.Key.messageIDKey(), nonceKey: d.Key.sealNonceKey(), workersPerOrigin: workersPerOrigin, logger: logger}, nil
}

// subscriptionParams holds the params of events/subscribe and
// events/unsubscribe as raw JSON. When the SDK fails to decode params into a
// typed struct, its error quotes all of them, and these params carry the
// subscriber secret. The decode here never fails: the handler checks every
// field and answers with fixed text.
type subscriptionParams struct {
	mcp.ParamsBase
	fields    subscriptionFields
	malformed bool
}

type subscriptionFields struct {
	Name, Arguments, Delivery, Cursor, MaxAgeMs, TTLMs json.RawMessage
}

func (p *subscriptionParams) UnmarshalJSON(b []byte) error {
	members, ok := decodeMembers(b)
	p.malformed = !ok
	p.fields = subscriptionFields{
		Name:      members["name"],
		Arguments: members["arguments"],
		Delivery:  members["delivery"],
		Cursor:    members["cursor"],
		MaxAgeMs:  members["maxAgeMs"],
		TTLMs:     members["ttlMs"],
	}
	return nil
}

// decodeMembers reads a JSON object by exact member name. encoding/json
// matches a name in any letter case and keeps the last of a repeated name, so
// one body could say one thing to RequireEventName and another to a handler.
// Here a repeated name fails and a name in another case is another member,
// which the handlers ignore, as go-sdk does for the params of events/poll.
func decodeMembers(raw []byte) (map[string]json.RawMessage, bool) {
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil || hasRepeatedKey(raw) {
		return nil, false
	}
	return members, true
}

type eventsSubscribeResult struct {
	mcp.ResultBase
	ResultType    string `json:"resultType"`
	ID            string `json:"id"`
	RefreshBefore string `json:"refreshBefore"`
	Cursor        string `json:"cursor"`
	Truncated     bool   `json:"truncated"`
	// DeliveryStatus is set when the call refreshed a subscription that
	// existed.
	DeliveryStatus *deliveryStatusResult `json:"deliveryStatus,omitempty"`
}

// deliveryStatusResult is the deliveryStatus object of SEP-3415. LastError is
// one of its categories and never anything the endpoint sent. Active is
// always true, because faucet never suspends a subscription.
type deliveryStatusResult struct {
	Active         bool    `json:"active"`
	LastDeliveryAt string  `json:"lastDeliveryAt,omitempty"`
	LastError      *string `json:"lastError"`
	FailedSince    string  `json:"failedSince,omitempty"`
}

func newDeliveryStatusResult(st DeliveryStatus) *deliveryStatusResult {
	res := &deliveryStatusResult{Active: true}
	if !st.LastDeliveryAt.IsZero() {
		res.LastDeliveryAt = st.LastDeliveryAt.UTC().Format(time.RFC3339)
	}
	if st.LastError != "" {
		lastError := string(st.LastError)
		res.LastError = &lastError
	}
	if !st.FailedSince.IsZero() {
		res.FailedSince = st.FailedSince.UTC().Format(time.RFC3339)
	}
	return res
}

type eventsUnsubscribeResult struct {
	mcp.ResultBase
	ResultType string `json:"resultType"`
}

type subscribeRequest struct {
	entry       *catalogEntry
	arguments   pollArguments
	canonical   json.RawMessage
	callbackURL string
	secret      subscriberSecret
	cursor      *cursor
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

func (s *Service) subscribeMethod(ctx context.Context, _ *mcp.ServerSession, p *subscriptionParams) (*eventsSubscribeResult, error) {
	wh, err := s.requireWebhook()
	if err != nil {
		return nil, err
	}
	if entry, raw, err := s.rawSubscriptionEntry(p); err != nil {
		return nil, err
	} else if raw {
		return s.subscribeRaw(ctx, wh, entry, p)
	}
	req, err := parseSubscribe(p, wh.allowInsecureLoopback)
	if err != nil {
		return nil, err
	}
	logger := s.logger.With(req.arguments.logField(), slog.F("event", req.entry.name))

	principal, err := s.subscriber(ctx, logger, "subscribe")
	if err != nil {
		return nil, err
	}
	key := NewSubscriptionKeyHash(principal.Login, req.callbackURL, req.entry.name, req.canonical)
	target, err := s.authorizeFreshTarget(ctx, logger, principal, "subscribe", req.arguments.targetArguments)
	if err != nil {
		if xerrors.Is(err, ErrNoAccess) {
			s.endRevokedSubscription(ctx, logger, wh, principal, key)
		}
		return nil, accessError(err)
	}
	secret := req.secret
	verification := CallbackRequest{
		Login: principal.Login, URL: req.callbackURL, SubscriptionID: key.ID(),
		sign: func(id string, timestamp time.Time, body []byte) (http.Header, error) {
			return SignWebhook(string(secret), id, timestamp, body)
		},
	}
	if err := wh.verifyCallback(ctx, logger, verification); err != nil {
		return nil, err
	}

	now := s.clock.Now()
	start, err := s.store.StartSubscription(ctx, startQuery(now, target, req))
	if err != nil {
		return nil, subscriptionStoreError(ctx, logger, err)
	}
	sealed := sealSecret(wh.aead, wh.nonceKey, req.secret, key[:])
	expiresAt := now.Add(req.ttl).Truncate(time.Second)
	row, err := s.store.UpsertSubscription(ctx, Subscription{
		KeyHash:     key,
		Principal:   principal.Login,
		CallbackURL: req.callbackURL,
		EventType:   req.entry.name,
		EventTypes:  req.entry.types(req.arguments.Actions),
		Arguments:   req.canonical,
		Target:      target,
		SecretEnc:   sealed,
		Cursor:      start.After,
		Position:    start.Position,
		ExpiresAt:   expiresAt,
		// The window never outlasts the grant, so a previous secret cannot
		// outlive its subscription.
		RotationGrace: min(rotationGrace, expiresAt.Sub(now)),
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
	renewed := row.UpdatedAt.After(row.CreatedAt)
	result := &eventsSubscribeResult{
		ResultType:    resultComplete,
		ID:            key.ID(),
		RefreshBefore: row.ExpiresAt.UTC().Format(time.RFC3339),
		Cursor:        encodeCursor(row.Position),
		// A renewal kept its own start, so it skipped nothing. A legacy
		// cursor is no place on the line, so the start skipped what it named.
		Truncated: (start.Truncated || req.cursor != nil && req.cursor.legacy) && !renewed,
	}
	if renewed {
		result.DeliveryStatus = newDeliveryStatusResult(row.Status)
	}
	return result, nil
}

func (s *Service) unsubscribeMethod(ctx context.Context, _ *mcp.ServerSession, p *subscriptionParams) (*eventsUnsubscribeResult, error) {
	wh, err := s.requireWebhook()
	if err != nil {
		return nil, err
	}
	if p == nil || p.malformed {
		return nil, invalidParams("params must be an object")
	}
	if entry, raw, err := s.rawSubscriptionEntry(p); err != nil {
		return nil, err
	} else if raw {
		return s.unsubscribeRaw(ctx, wh, entry, p)
	}
	entry, args, err := parseSubscriptionName(p.fields.Name, p.fields.Arguments, func(e *catalogEntry) *jsonschema.Resolved { return e.endInput })
	if err != nil {
		return nil, err
	}
	d, err := decodeDelivery(p.fields.Delivery)
	if err != nil {
		return nil, err
	}
	callbackURL, err := d.callbackURL(wh.allowInsecureLoopback)
	if err != nil {
		return nil, err
	}
	logger := s.logger.With(args.logField(), slog.F("event", entry.name))

	principal, err := s.subscriber(ctx, logger, "unsubscribe")
	if err != nil {
		return nil, err
	}
	key := NewSubscriptionKeyHash(principal.Login, callbackURL, entry.name, canonicalArguments(args))
	return s.endSubscription(ctx, logger, principal, key)
}

// endSubscription deletes the caller's live subscription of key.
func (s *Service) endSubscription(ctx context.Context, logger slog.Logger, principal Principal, key SubscriptionKeyHash) (*eventsUnsubscribeResult, error) {
	_, found, err := s.store.DeleteLiveSubscription(ctx, principal.Login, key, s.clock.Now())
	if err != nil {
		return nil, subscriptionStoreError(ctx, logger, err)
	}
	if !found {
		return nil, rpcError(codeNotFound, "no such subscription", map[string]string{"kind": "subscription"})
	}
	logger.Info(ctx, "unsubscribed", slog.F("login", principal.Login), slog.F("subscription", key.ID()))
	return &eventsUnsubscribeResult{ResultType: resultComplete}, nil
}

// subscriber needs the events:subscribe scope. Every principal that the
// constructors build has a login, which keys its subscriptions.
func (s *Service) subscriber(ctx context.Context, logger slog.Logger, op string) (Principal, error) {
	p, err := s.principal(ctx, logger, op, ScopeEventsSubscribe)
	if err != nil {
		return Principal{}, accessError(err)
	}
	return p, nil
}

func (wh *webhookState) verifyCallback(ctx context.Context, logger slog.Logger, req CallbackRequest) error {
	login := req.Login
	if wh.verified.verified(login, req.URL) {
		return nil
	}
	err := wh.callbacks.VerifyCallback(ctx, req)
	var cbErr *CallbackError
	switch {
	case err == nil:
		wh.verified.remember(login, req.URL)
		return nil
	case xerrors.As(err, &cbErr) && cbErr.Reason.valid():
		logger.Info(ctx, "callback not verified", slog.F("login", login), slog.F("reason", string(cbErr.Reason)))
		message := cbErr.Message
		if message == "" {
			message = "callback endpoint failed verification"
		}
		return rpcError(codeCallbackEndpoint, message, map[string]string{"reason": string(cbErr.Reason)})
	case xerrors.Is(err, ErrCallbackRateLimited):
		logger.Info(ctx, "callback verification rate limited", slog.F("login", login))
		return rpcError(codeResourceExhausted, "too many verification requests for this callback host, try again later",
			map[string]string{"limit": "callbackVerifications"})
	default:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logger.Warn(ctx, "could not verify the callback", slog.F("login", login), slog.Error(err))
		return rpcError(jsonrpc.CodeInternalError, "could not verify the callback endpoint, try again", nil)
	}
}

// startQuery asks for the start of a subscription. No cursor means now, and
// so does a legacy cursor. A cursor below the oldest event the store can
// still serve, which is the purge mark or the maxAgeMs floor, is moved up to
// it and reported as truncated.
func startQuery(now time.Time, target Target, req subscribeRequest) StartQuery {
	if req.cursor == nil || req.cursor.legacy {
		return StartQuery{Target: target, Since: now}
	}
	q := StartQuery{Target: target, Cursor: &req.cursor.position}
	if req.maxAge > 0 {
		q.Since = now.Add(-req.maxAge)
	}
	return q
}

func subscriptionStoreError(ctx context.Context, logger slog.Logger, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	logger.Error(ctx, "store subscription", slog.Error(err))
	return rpcError(jsonrpc.CodeInternalError, errSubscriptionStore.Error(), nil)
}

func parseSubscribe(p *subscriptionParams, allowInsecureLoopback bool) (subscribeRequest, error) {
	if p == nil || p.malformed {
		return subscribeRequest{}, invalidParams("params must be an object")
	}
	entry, args, err := parseSubscriptionName(p.fields.Name, p.fields.Arguments, func(e *catalogEntry) *jsonschema.Resolved { return e.input })
	if err != nil {
		return subscribeRequest{}, err
	}
	d, err := decodeDelivery(p.fields.Delivery)
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
	req := subscribeRequest{entry: entry, arguments: args, canonical: canonicalArguments(args), callbackURL: callbackURL, secret: secret}
	if req.ttl, err = parseTTL(p.fields.TTLMs); err != nil {
		return subscribeRequest{}, err
	}
	if !isAbsent(p.fields.Cursor) {
		var cursor string
		if json.Unmarshal(p.fields.Cursor, &cursor) != nil {
			return subscribeRequest{}, invalidParams("cursor must be a string")
		}
		c, ok := decodeCursor(cursor)
		if !ok {
			return subscribeRequest{}, invalidParams("cursor is not one this server returned")
		}
		req.cursor = &c
	}
	if req.maxAge, err = parseMaxAge(p.fields.MaxAgeMs); err != nil {
		return subscribeRequest{}, err
	}
	return req, nil
}

// parseMaxAge reads maxAgeMs. An absent one is zero, which means no limit.
func parseMaxAge(raw json.RawMessage) (time.Duration, error) {
	if isAbsent(raw) {
		return 0, nil
	}
	ms, err := parseInteger(raw)
	if err != nil {
		return 0, invalidParams("maxAgeMs must be an integer")
	}
	if ms < 1 {
		return 0, invalidParams("maxAgeMs must be positive")
	}
	return time.Duration(min(ms, maxAgeLimitMs)) * time.Millisecond, nil
}

func isAbsent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// parseInteger reads a JSON number whose value is an integer, so 600000,
// 6e5 and 600000.0 all read as 600000. One beyond the int64 range reads as the
// nearest end of it. A fraction and anything that is not a number is an error.
func parseInteger(raw json.RawMessage) (int64, error) {
	trimmed := string(bytes.TrimSpace(raw))
	n, err := strconv.ParseInt(trimmed, 10, 64)
	if err == nil {
		return n, nil
	}
	if !strings.ContainsAny(trimmed, ".eE") && !xerrors.Is(err, strconv.ErrRange) {
		return 0, err
	}
	f, err := strconv.ParseFloat(trimmed, 64)
	switch {
	case err != nil && !xerrors.Is(err, strconv.ErrRange), math.IsNaN(f), f != math.Trunc(f):
		return 0, xerrors.New("not an integer")
	case f >= math.MaxInt64:
		return math.MaxInt64, nil
	case f <= math.MinInt64:
		return math.MinInt64, nil
	}
	return int64(f), nil
}

func parseSubscriptionName(rawName, arguments json.RawMessage, input func(*catalogEntry) *jsonschema.Resolved) (*catalogEntry, pollArguments, error) {
	var name string
	if !isAbsent(rawName) && json.Unmarshal(rawName, &name) != nil {
		return nil, pollArguments{}, invalidParams("name must be a string")
	}
	if name == "" {
		return nil, pollArguments{}, invalidParams("name is required")
	}
	entry, err := eventByName(name)
	if err != nil {
		return nil, pollArguments{}, err
	}
	args, err := parsePollArguments(input(entry), arguments)
	if err != nil {
		return nil, pollArguments{}, err
	}
	return entry, args, nil
}

// canonicalArguments is the one JSON spelling of args. Fields come in struct
// order without spaces, and each list of actions or field values sorted once
// each, so two requests that differ only in key order, whitespace or the order
// and repeats of a list address the same subscription. Arguments without
// actions and field filters keep the spelling they had before those existed,
// and so keep their key, and so do arguments that name a repository, from
// before owner and all targets existed. repos is in lower case, which a store
// that reads it compares.
func canonicalArguments(args pollArguments) json.RawMessage {
	return marshalRaw(struct {
		targetArguments
		SHA     string   `json:"sha,omitempty"`
		PR      int32    `json:"pr,omitempty"`
		Actions []string `json:"actions,omitempty"`
		fieldArguments
	}{args.normalized(), args.SHA, args.PR, sortedSet(args.Actions), args.canonical()})
}

type deliveryParams struct {
	Mode, URL, Secret *string
}

// decodeDelivery reads the delivery object. Every error is fixed text, so none
// can quote the secret or the URL.
func decodeDelivery(raw json.RawMessage) (deliveryParams, error) {
	bad := invalidParams("delivery must be an object with string fields")
	members, ok := decodeMembers(raw)
	if !ok || members == nil {
		return deliveryParams{}, bad
	}
	var d deliveryParams
	for name, member := range map[string]**string{"mode": &d.Mode, "url": &d.URL, "secret": &d.Secret} {
		if isAbsent(members[name]) {
			continue
		}
		var value string
		if json.Unmarshal(members[name], &value) != nil {
			return deliveryParams{}, bad
		}
		*member = &value
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
	if _, ok := decodeSecret(s); !ok {
		return "", invalidParams("delivery.secret must be %s followed by the base64 of %d to %d random bytes", secretPrefix, minSubscriberSecretBytes, maxSubscriberSecretBytes)
	}
	return subscriberSecret(s), nil
}

// decodeSecret returns the HMAC key bytes of a secret that parseSecret accepts.
func decodeSecret(s string) ([]byte, bool) {
	encoded, ok := strings.CutPrefix(s, secretPrefix)
	if !ok || len(s) > maxSecretChars || strings.IndexFunc(encoded, func(r rune) bool { return !isBase64Char(r) }) >= 0 {
		return nil, false
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(decoded) < minSubscriberSecretBytes || len(decoded) > maxSubscriberSecretBytes {
		return nil, false
	}
	return decoded, true
}

func isBase64Char(r rune) bool {
	return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '+' || r == '/' || r == '='
}

// parseTTL turns the suggestion into a grant. Absent, null and anything above
// the cap give the cap, and anything below the floor gives the floor. SEP-3415
// has no rejection path for TTL values.
func parseTTL(raw json.RawMessage) (time.Duration, error) {
	if isAbsent(raw) {
		return maxSubscriptionTTL, nil
	}
	ms, err := parseInteger(raw)
	if err != nil {
		return 0, invalidParams("ttlMs must be an integer or null")
	}
	ms = max(minSubscriptionTTL.Milliseconds(), min(ms, maxSubscriptionTTL.Milliseconds()))
	return time.Duration(ms) * time.Millisecond, nil
}
