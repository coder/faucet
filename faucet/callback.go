package faucet

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/quartz"
)

const (
	maxCallbackURLBytes     = 2048
	callbackVerificationTTL = 10 * time.Minute
	maxVerifiedCallbacks    = 10000
)

// CallbackReason says why a callback endpoint failed verification. The values
// are the lastError categories of SEP-3415. They never carry anything the
// endpoint sent.
type CallbackReason string

const (
	CallbackConnectionRefused CallbackReason = "connection_refused"
	CallbackTimeout           CallbackReason = "timeout"
	CallbackTLSError          CallbackReason = "tls_error"
	CallbackHTTP4xx           CallbackReason = "http_4xx"
	CallbackHTTP5xx           CallbackReason = "http_5xx"
	CallbackChallengeFailed   CallbackReason = "challenge_failed"
)

func (r CallbackReason) valid() bool {
	return slices.Contains([]CallbackReason{
		CallbackConnectionRefused, CallbackTimeout, CallbackTLSError, CallbackHTTP4xx, CallbackHTTP5xx, CallbackChallengeFailed,
	}, r)
}

// CallbackError is what a CallbackVerifier returns when the endpoint does not
// agree to receive deliveries. Subscribe turns it into CallbackEndpointError.
type CallbackError struct {
	Reason CallbackReason
	// Message is shown to the subscriber. It must not quote the endpoint.
	Message string
}

func (e *CallbackError) Error() string {
	return "callback endpoint failed verification: " + string(e.Reason)
}

// ErrCallbackRateLimited is what a CallbackVerifier returns when it refuses to
// contact an endpoint because it already sent too many requests there.
// events/subscribe answers ResourceExhausted and stores nothing.
var ErrCallbackRateLimited = xerrors.New("callback verification rate limit reached")

// CallbackRequest is what a CallbackVerifier gets for one callback URL.
type CallbackRequest struct {
	Principal Principal
	// URL is the callback URL in the canonical form that subscribe stores.
	URL string
	// SubscriptionID is the id that events/subscribe returns, which a request
	// to the endpoint carries in X-MCP-Subscription-Id.
	SubscriptionID string
	secret         subscriberSecret
}

// Sign returns the Standard Webhooks headers for a message to the endpoint,
// signed with the subscriber's secret. The verifier never holds the secret
// itself, so it cannot log it.
func (r CallbackRequest) Sign(id string, timestamp time.Time, body []byte) (http.Header, error) {
	return SignWebhook(string(r.secret), id, timestamp, body)
}

// Format implements fmt.Formatter so that no verb can print the secret.
func (r CallbackRequest) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "CallbackRequest{Login:%q}", r.Principal.Login)
}

// CallbackVerifier proves that the endpoint behind a callback URL agrees to
// receive a principal's deliveries. SEP-3415 requires this before delivery
// starts, because a client-supplied secret stops forged deliveries and does not
// stop a victim from being flooded.
type CallbackVerifier interface {
	// VerifyCallback returns nil when the endpoint agrees. A *CallbackError
	// says why it does not, and ErrCallbackRateLimited that it was not asked.
	VerifyCallback(ctx context.Context, req CallbackRequest) error
}

type callbackAllowlist struct {
	origins []string
}

// NewCallbackAllowlist returns the verifier for SEP-3415's server-configured
// allowlist. It accepts a callback URL whose scheme, host and port match one
// of origins, each written scheme://host[:port]. It makes no request.
func NewCallbackAllowlist(origins []string, allowInsecureLoopback bool) (CallbackVerifier, error) {
	if len(origins) == 0 {
		return nil, xerrors.New("at least one allowed origin is required")
	}
	list := callbackAllowlist{origins: make([]string, len(origins))}
	for i, raw := range origins {
		u, err := parseCallbackURL(raw, allowInsecureLoopback)
		if err != nil {
			return nil, xerrors.Errorf("allowed origin %d: %w", i+1, err)
		}
		if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.ForceQuery {
			return nil, xerrors.Errorf("allowed origin %d must be scheme://host[:port] only", i+1)
		}
		list.origins[i] = originOf(u)
	}
	return list, nil
}

func (a callbackAllowlist) VerifyCallback(_ context.Context, req CallbackRequest) error {
	u, err := url.Parse(req.URL)
	if err != nil {
		return xerrors.Errorf("parse callback URL: %w", err)
	}
	if !slices.Contains(a.origins, originOf(u)) {
		return &CallbackError{Reason: CallbackChallengeFailed, Message: "the callback URL's origin is not on this server's allowlist"}
	}
	return nil
}

func originOf(u *url.URL) string {
	return u.Scheme + "://" + u.Host
}

// parseCallbackURL checks a callback URL without any network access and
// returns it in canonical form: lower-case host without a trailing dot and
// without the default port. It never puts raw in an error, because a URL can
// carry a token in its path or query.
func parseCallbackURL(raw string, allowInsecureLoopback bool) (*url.URL, error) {
	if raw == "" || len(raw) > maxCallbackURLBytes {
		return nil, xerrors.Errorf("must be 1 to %d bytes", maxCallbackURLBytes)
	}
	if strings.ContainsAny(raw, "\\#") || strings.IndexFunc(raw, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
		return nil, xerrors.New("must not contain whitespace, control characters, a backslash or a fragment")
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil || !u.IsAbs() || u.Opaque != "" || u.Hostname() == "":
		return nil, xerrors.New("must be an absolute URL with a host")
	case u.User != nil:
		return nil, xerrors.New("must not contain user info")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if err := checkCallbackHost(host, u.Scheme, allowInsecureLoopback); err != nil {
		return nil, err
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, xerrors.New("port must be from 1 to 65535")
		}
	}
	if u.Scheme == "https" && port == "443" {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	u.Host = host
	return u, nil
}

func checkCallbackHost(host, scheme string, allowInsecureLoopback bool) error {
	if host == "" || strings.IndexFunc(host, func(r rune) bool { return r >= 0x80 || r == '%' }) >= 0 {
		return xerrors.New("host must be ASCII")
	}
	var loopback bool
	if addr, err := netip.ParseAddr(host); err == nil {
		loopback = addr.Unmap().IsLoopback()
		if !loopback && !isGlobalAddr(addr) {
			return xerrors.New("host must be a public address")
		}
	} else {
		loopback = host == "localhost" || strings.HasSuffix(host, ".localhost")
		if !loopback && !isNamedHost(host) {
			return xerrors.New("host must be a public name or address")
		}
	}
	switch {
	case loopback && !allowInsecureLoopback:
		return xerrors.New("host must not be a loopback address")
	case loopback && scheme != "http" && scheme != "https", !loopback && scheme != "https":
		return xerrors.New("scheme must be https")
	}
	return nil
}

// isNamedHost refuses names that a C resolver reads as an IPv4 address in
// decimal, octal or hex (2130706433, 0x7f.1) and single-label names, which
// resolve through the host's search domains.
func isNamedHost(host string) bool {
	labels := strings.Split(host, ".")
	if len(labels) < 2 || strings.Contains(host, "..") {
		return false
	}
	if _, err := strconv.ParseUint(labels[len(labels)-1], 10, 64); err == nil {
		return false
	}
	return !slices.ContainsFunc(labels, func(l string) bool { return l == "" || strings.HasPrefix(l, "0x") })
}

var nonGlobalPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/96", "::ffff:0:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "100:0:0:1::/64", "2001::/23",
		"2001:db8::/32", "2002::/16", "3fff::/20", "5f00::/16", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// isGlobalAddr reports whether addr is publicly routable. The standard library
// predicates cross-check the table of prefixes, so a prefix left out of it
// still cannot let loopback, private, link-local or multicast space through.
func isGlobalAddr(addr netip.Addr) bool {
	addr = addr.WithZone("").Unmap()
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsMulticast() {
		return false
	}
	return !slices.ContainsFunc(nonGlobalPrefixes, func(p netip.Prefix) bool { return p.Contains(addr) })
}

// callbackCache does not remember a failure, so a retry can succeed. A restart
// verifies again.
type callbackCache struct {
	clock quartz.Clock

	mu      sync.Mutex
	expires map[verifiedCallback]time.Time
}

type verifiedCallback struct {
	login string
	url   string
}

func newCallbackCache(clock quartz.Clock) *callbackCache {
	return &callbackCache{clock: clock, expires: map[verifiedCallback]time.Time{}}
}

func (c *callbackCache) verified(login, callbackURL string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.clock.Now().Before(c.expires[verifiedCallback{login, callbackURL}])
}

func (c *callbackCache) remember(login, callbackURL string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	if len(c.expires) >= maxVerifiedCallbacks {
		for k, t := range c.expires {
			if !now.Before(t) {
				delete(c.expires, k)
			}
		}
		if len(c.expires) >= maxVerifiedCallbacks {
			clear(c.expires)
		}
	}
	c.expires[verifiedCallback{login, callbackURL}] = now.Add(callbackVerificationTTL)
}
