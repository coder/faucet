package faucet

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/quartz"
)

const (
	handshakeLoginBurst  = 5
	handshakeLoginRefill = 12 * time.Second
	handshakeHostBurst   = 60
	handshakeHostRefill  = time.Second
	maxHandshakeBuckets  = 10000
	maxHandshakeAnswer   = 1024
	handshakeNonceBytes  = 32
	handshakeIDBytes     = 16
	handshakeUserAgent   = "faucet-webhook-verification"
	subscriptionIDHeader = "X-MCP-Subscription-Id"
)

type HandshakeOptions struct {
	// Client sends the verification request. Build it with NewHTTPClient: a
	// client without its checks lets a subscriber aim the request at an
	// address inside your network.
	Client *http.Client
	// Clock times the rate limit and the webhook-timestamp. Nil uses the real
	// clock.
	Clock quartz.Clock
}

type handshake struct {
	client *http.Client
	clock  quartz.Clock
	hosts  *hostLimiter
}

// NewCallbackHandshake returns the verifier for the first option of SEP-3415's
// endpoint verification. It POSTs {"type":"verification","challenge":"<nonce>"},
// signed like a delivery with the subscriber's secret, and accepts an endpoint
// that answers 2xx with a JSON object whose only member is that challenge. The
// nonce is random, single-use and never stored or logged. Nothing the endpoint
// sent reaches a log or an error.
//
// Two token buckets limit the requests. Each login has one per destination: 5
// handshakes at once, then one every 12 seconds. Each destination has one for
// all logins: 60 at once, then one a second. A login that fails again and again
// empties only its own bucket and cannot starve another login, and it takes
// many logins to empty the bucket of a destination. A handshake spends a token
// of each, and an endpoint that answers it correctly gives them back, because
// an endpoint that echoes the nonce has agreed to the traffic. When a bucket is
// empty the verifier returns ErrCallbackRateLimited and sends nothing.
func NewCallbackHandshake(opts HandshakeOptions) (CallbackVerifier, error) {
	if opts.Client == nil {
		return nil, xerrors.New("handshake client is required")
	}
	clock := opts.Clock
	if clock == nil {
		clock = quartz.NewReal()
	}
	return &handshake{client: opts.Client, clock: clock, hosts: newHostLimiter(clock)}, nil
}

type verificationMessage struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
}

func (h *handshake) VerifyCallback(ctx context.Context, req CallbackRequest) error {
	u, err := url.Parse(req.URL)
	if err != nil || u.Hostname() == "" {
		return xerrors.New("callback URL is not an absolute URL")
	}
	host := limiterKey(u.Hostname())
	if !h.hosts.take(req.Login, host) {
		return ErrCallbackRateLimited
	}
	nonce, id, err := randomToken(handshakeNonceBytes)
	if err != nil {
		return err
	}
	body, err := json.Marshal(verificationMessage{Type: "verification", Challenge: nonce})
	if err != nil {
		return xerrors.Errorf("encode verification: %w", err)
	}
	httpReq, err := h.request(ctx, req, id, body)
	if err != nil {
		return err
	}
	resp, err := h.client.Do(httpReq) //nolint:bodyclose // Closed in checkAnswer.
	if err != nil {
		return transportFailure(ctx, err)
	}
	if err := checkAnswer(ctx, resp, nonce); err != nil {
		return err
	}
	h.hosts.refund(req.Login, host)
	return nil
}

func (h *handshake) request(ctx context.Context, req CallbackRequest, token string, body []byte) (*http.Request, error) {
	headers, err := req.Sign("msg_verification_"+token, h.clock.Now(), body)
	if err != nil {
		return nil, xerrors.Errorf("sign verification: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.URL, bytes.NewReader(body))
	if err != nil {
		return nil, xerrors.New("callback URL is not an absolute URL")
	}
	httpReq.Header = headers
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", handshakeUserAgent)
	httpReq.Header.Set(subscriptionIDHeader, req.SubscriptionID)
	return httpReq, nil
}

func checkAnswer(ctx context.Context, resp *http.Response, nonce string) error {
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return callbackFailure(CallbackHTTP4xx, "the callback endpoint answered with a 4xx status")
	case resp.StatusCode >= 500:
		return callbackFailure(CallbackHTTP5xx, "the callback endpoint answered with a 5xx status")
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return callbackFailure(CallbackChallengeFailed, "the callback endpoint did not answer the challenge")
	}
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxHandshakeAnswer+1))
	if err != nil {
		return transportFailure(ctx, err)
	}
	if len(answer) > maxHandshakeAnswer || !echoesOnly(answer, nonce) {
		return callbackFailure(CallbackChallengeFailed, "the callback endpoint did not answer the challenge")
	}
	return nil
}

// transportFailure turns a transport error into a category. It never keeps err,
// which can quote the callback URL.
func transportFailure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return callbackFailure(callbackReasonOf(err), "the callback endpoint could not be reached")
}

func callbackFailure(reason CallbackReason, message string) *CallbackError {
	return &CallbackError{Reason: reason, Message: message}
}

// callbackReasonOf maps an error from the client of NewHTTPClient to a SEP-3415
// category. An address that the client refuses, a name that does not resolve
// and a refused connection all give connection_refused, so the category does
// not tell a caller whether a host name points inside the network.
func callbackReasonOf(err error) CallbackReason {
	var (
		netErr      net.Error
		certErr     *tls.CertificateVerificationError
		recordErr   tls.RecordHeaderError
		alertErr    tls.AlertError
		unknownAuth x509.UnknownAuthorityError
		hostErr     x509.HostnameError
		invalidCert x509.CertificateInvalidError
	)
	switch {
	case errors.Is(err, ErrResponseTooLarge):
		return CallbackChallengeFailed
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		return CallbackTimeout
	case errors.Is(err, http.ErrSchemeMismatch), errors.As(err, &certErr), errors.As(err, &recordErr), errors.As(err, &alertErr),
		errors.As(err, &unknownAuth), errors.As(err, &hostErr), errors.As(err, &invalidCert):
		return CallbackTLSError
	default:
		return CallbackConnectionRefused
	}
}

// echoesOnly reports whether answer is a JSON object with one member,
// "challenge", whose value is nonce. Whitespace and a trailing newline do not
// matter. A body that holds more, such as the request reflected back, does not
// count.
func echoesOnly(answer []byte, nonce string) bool {
	dec := json.NewDecoder(bytes.NewReader(answer))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return false
	}
	members := 0
	var got string
	var isString bool
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return false
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return false
		}
		members++
		if key == "challenge" {
			isString = json.Unmarshal(value, &got) == nil
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return false
	}
	return members == 1 && isString && subtle.ConstantTimeCompare([]byte(got), []byte(nonce)) == 1
}

// randomToken returns two unrelated URL-safe random strings: a challenge of
// size bytes, and a message ID.
func randomToken(size int) (challenge, id string, err error) {
	buf := make([]byte, size+handshakeIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", xerrors.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf[:size]), base64.RawURLEncoding.EncodeToString(buf[size:]), nil
}

// limiterKey names the destination of a request: an IP address by its canonical
// form, so that no spelling of it gets a bucket of its own, and a host name in
// lower case without a trailing dot.
func limiterKey(host string) string {
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.WithZone("").Unmap().String()
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

type bucket struct {
	tokens int
	last   time.Time
}

func (b bucket) refilled(now time.Time, burst int, every time.Duration) bucket {
	if n := int(now.Sub(b.last) / every); n > 0 {
		b.tokens = min(burst, b.tokens+n)
		b.last = b.last.Add(time.Duration(n) * every)
	}
	if b.tokens == burst {
		b.last = now
	}
	return b
}

// tokenBuckets keeps one bucket per key. At the size limit it forgets the
// buckets that are full again, which a new bucket equals, and when none is left
// it refuses new keys.
type tokenBuckets struct {
	burst   int
	every   time.Duration
	buckets map[string]bucket
}

func (t *tokenBuckets) take(key string, now time.Time) bool {
	b, known := t.buckets[key]
	if !known {
		if len(t.buckets) >= maxHandshakeBuckets {
			t.forgetFull(now)
		}
		if len(t.buckets) >= maxHandshakeBuckets {
			return false
		}
		b = bucket{tokens: t.burst, last: now}
	}
	b = b.refilled(now, t.burst, t.every)
	granted := b.tokens > 0
	if granted {
		b.tokens--
	}
	t.buckets[key] = b
	return granted
}

func (t *tokenBuckets) refund(key string) {
	if b, ok := t.buckets[key]; ok {
		b.tokens = min(t.burst, b.tokens+1)
		t.buckets[key] = b
	}
}

func (t *tokenBuckets) forgetFull(now time.Time) {
	for key, b := range t.buckets {
		if b.refilled(now, t.burst, t.every).tokens == t.burst {
			delete(t.buckets, key)
		}
	}
}

// hostLimiter limits the handshakes to one destination twice: per login, so that
// one login cannot use up what another needs, and for all logins together, so
// that many logins cannot flood a host.
type hostLimiter struct {
	clock quartz.Clock

	mu       sync.Mutex
	perLogin tokenBuckets
	perHost  tokenBuckets
}

func newHostLimiter(clock quartz.Clock) *hostLimiter {
	return &hostLimiter{
		clock:    clock,
		perLogin: tokenBuckets{burst: handshakeLoginBurst, every: handshakeLoginRefill, buckets: map[string]bucket{}},
		perHost:  tokenBuckets{burst: handshakeHostBurst, every: handshakeHostRefill, buckets: map[string]bucket{}},
	}
}

func loginHostKey(login, host string) string { return login + "\x00" + host }

func (l *hostLimiter) take(login, host string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	if !l.perLogin.take(loginHostKey(login, host), now) {
		return false
	}
	if !l.perHost.take(host, now) {
		l.perLogin.refund(loginHostKey(login, host))
		return false
	}
	return true
}

func (l *hostLimiter) refund(login, host string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.perLogin.refund(loginHostKey(login, host))
	l.perHost.refund(host)
}
