package faucet

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"golang.org/x/xerrors"
)

const (
	callbackRequestTimeout   = 5 * time.Second
	maxCallbackResponseBytes = 64 << 10
	maxCallbackHeaderBytes   = 16 << 10
	maxDialAddresses         = 4
)

var (
	// ErrDestinationRefused is what a client from NewHTTPClient returns for a
	// URL that is not a public https address. An error that wraps it can hold
	// the URL, so do not log it.
	ErrDestinationRefused = xerrors.New("the destination is not a public https address")
	// ErrResponseTooLarge says that a response was longer than the client
	// reads.
	ErrResponseTooLarge = xerrors.New("the response is too large")
)

// Resolver looks up the addresses of a host name. *net.Resolver implements it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

type HTTPClientOptions struct {
	// Resolver looks names up. Nil uses net.DefaultResolver.
	Resolver Resolver
	// AllowInsecureLoopback lets the client reach loopback addresses, and
	// reach them over http. It exists for tests. Production hosts leave it
	// false.
	AllowInsecureLoopback bool
	// RootCAs verifies server certificates. Nil uses the system roots.
	RootCAs *x509.CertPool
}

// NewHTTPClient returns a client for requests to a URL that a caller chose. It
// speaks https only, to public addresses only. It looks the host name up,
// refuses the request when any answer is not public, and dials the address it
// checked, so a DNS answer that changes later has no effect. Certificates are
// still verified against the host name. It follows no redirect, ignores the
// proxy environment variables, gives up after 5 seconds, and fails a read of a
// response body past 64 KiB with ErrResponseTooLarge.
//
// Errors that it returns can quote the URL, which can hold a token in its path
// or query. Do not log them or show them to a subscriber.
func NewHTTPClient(opts HTTPClientOptions) *http.Client {
	resolver := opts.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	g := &guard{resolver: resolver, allowLoopback: opts.AllowInsecureLoopback}
	g.dialer = &net.Dialer{Timeout: callbackRequestTimeout, Control: g.control}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            g.dialContext,
		TLSClientConfig:        &tls.Config{RootCAs: opts.RootCAs, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    callbackRequestTimeout,
		MaxResponseHeaderBytes: maxCallbackHeaderBytes,
		DisableKeepAlives:      true,
		DisableCompression:     true,
	}
	return &http.Client{
		Transport:     guardedTransport{next: transport, allowLoopback: opts.AllowInsecureLoopback},
		Timeout:       callbackRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type guard struct {
	resolver      Resolver
	allowLoopback bool
	dialer        *net.Dialer
}

func (g *guard) permits(addr netip.Addr) bool {
	addr = addr.WithZone("").Unmap()
	return (g.allowLoopback && addr.IsLoopback()) || isGlobalAddr(addr)
}

// resolve returns the addresses that the client may dial for host. One answer
// that is not public refuses all of them, so a mixed answer cannot pick the
// private one.
func (g *guard) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		if !g.permits(addr) {
			return nil, ErrDestinationRefused
		}
		return []netip.Addr{addr.WithZone("").Unmap()}, nil
	}
	answers, err := g.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, xerrors.Errorf("look up host: %w", err)
	}
	if len(answers) == 0 {
		return nil, ErrDestinationRefused
	}
	addrs := make([]netip.Addr, 0, min(len(answers), maxDialAddresses))
	for _, answer := range answers {
		if !g.permits(answer) {
			return nil, ErrDestinationRefused
		}
		if len(addrs) < maxDialAddresses {
			addrs = append(addrs, answer.WithZone("").Unmap())
		}
	}
	return addrs, nil
}

func (g *guard) dialContext(ctx context.Context, _, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, ErrDestinationRefused
	}
	addrs, err := g.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	var dialErr error
	for _, addr := range addrs {
		conn, err := g.dialer.DialContext(ctx, "tcp", net.JoinHostPort(addr.String(), port))
		if err == nil {
			return conn, nil
		}
		dialErr = err
	}
	return nil, dialErr
}

// control runs on the socket, just before it connects. It checks the address
// that the connection really uses, so a change that dialed a name would fail
// here instead of reaching a private address.
func (g *guard) control(_, address string, _ syscall.RawConn) error {
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil || !g.permits(addrPort.Addr()) {
		return ErrDestinationRefused
	}
	return nil
}

type guardedTransport struct {
	next          http.RoundTripper
	allowLoopback bool
}

func (t guardedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := checkRequestURL(req.URL, t.allowLoopback); err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.ContentLength > maxCallbackResponseBytes {
		_ = resp.Body.Close()
		return nil, ErrResponseTooLarge
	}
	resp.Body = &cappedBody{body: resp.Body, left: maxCallbackResponseBytes}
	return resp, nil
}

// checkRequestURL refuses what needs no lookup: another scheme than https, user
// info (which the client would send as a Basic credential), and hosts that are
// not public names or addresses. http is allowed only for a loopback address
// written as a number, and only when the caller asked for it.
func checkRequestURL(u *url.URL, allowLoopback bool) error {
	if u == nil || u.User != nil || u.Opaque != "" {
		return ErrDestinationRefused
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if err := checkCallbackHost(host, u.Scheme, allowLoopback); err != nil {
		return ErrDestinationRefused
	}
	if u.Scheme == "http" {
		if addr, err := netip.ParseAddr(host); err != nil || !addr.IsLoopback() {
			return ErrDestinationRefused
		}
	}
	return nil
}

// cappedBody fails the read that goes past the limit. It does not cut the body
// short, because a receiver that parses half a body can read a different
// message than the endpoint sent.
type cappedBody struct {
	body io.ReadCloser
	left int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if int64(len(p)) > b.left+1 {
		p = p[:b.left+1]
	}
	n, err := b.body.Read(p)
	if int64(n) > b.left {
		n, b.left = int(b.left), 0
		return n, ErrResponseTooLarge
	}
	b.left -= int64(n)
	return n, err
}

func (b *cappedBody) Close() error { return b.body.Close() }
