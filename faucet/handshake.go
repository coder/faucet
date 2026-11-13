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
	handshakeBurst       = 5
	handshakeRefill      = 12 * time.Second
	maxHandshakeHosts    = 10000
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
// Each destination host has a token bucket: 5 handshakes at once, then one every
// 12 seconds. A handshake spends a token, and an endpoint that answers it
// correctly gives the token back, because an endpoint that echoes the nonce has
// agreed to the traffic. When the bucket is empty the verifier returns
// ErrCallbackRateLimited and sends nothing.
func NewCallbackHandshake(opts HandshakeOptions) (CallbackVerifier, error) {
	if opts.Client == nil {
		return nil, xerrors.New("handshake client is required")
	}
	clock := opts.Clock
	if clock == nil {
		clock = quartz.NewReal()
	}
	return &handshake{client: opts.Client, clock: clock, hosts: &hostLimiter{clock: clock, buckets: map[string]bucket{}}}, nil
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
	if !h.hosts.take(host) {
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
	h.hosts.refund(host)
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

func (b bucket) refilled(now time.Time) bucket {
	if n := int(now.Sub(b.last) / handshakeRefill); n > 0 {
		b.tokens = min(handshakeBurst, b.tokens+n)
		b.last = b.last.Add(time.Duration(n) * handshakeRefill)
	}
	if b.tokens == handshakeBurst {
		b.last = now
	}
	return b
}

// hostLimiter keeps one token bucket per destination host. At the size limit it
// forgets the buckets that are full again, which a new bucket equals, and when
// none is left it refuses new hosts.
type hostLimiter struct {
	clock quartz.Clock

	mu      sync.Mutex
	buckets map[string]bucket
}

func (l *hostLimiter) take(host string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	b, known := l.buckets[host]
	if !known {
		if len(l.buckets) >= maxHandshakeHosts {
			l.forgetFull(now)
		}
		if len(l.buckets) >= maxHandshakeHosts {
			return false
		}
		b = bucket{tokens: handshakeBurst, last: now}
	}
	b = b.refilled(now)
	granted := b.tokens > 0
	if granted {
		b.tokens--
	}
	l.buckets[host] = b
	return granted
}

func (l *hostLimiter) refund(host string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[host]; ok {
		b.tokens = min(handshakeBurst, b.tokens+1)
		l.buckets[host] = b
	}
}

func (l *hostLimiter) forgetFull(now time.Time) {
	for host, b := range l.buckets {
		if b.refilled(now).tokens == handshakeBurst {
			delete(l.buckets, host)
		}
	}
}
