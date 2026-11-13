package faucet_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
)

const (
	targetHost = "example.com"
	sizeLimit  = 64 << 10
)

type lookup struct {
	addrs []netip.Addr
	err   error
}

func answer(addrs ...string) lookup {
	out := make([]netip.Addr, len(addrs))
	for i, a := range addrs {
		out[i] = netip.MustParseAddr(a)
	}
	return lookup{addrs: out}
}

// sequenceResolver answers the nth lookup with the nth entry, and every later
// lookup with the last one.
type sequenceResolver struct {
	mu      sync.Mutex
	answers []lookup
	calls   int
}

func (r *sequenceResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.answers[min(r.calls, len(r.answers)-1)]
	r.calls++
	return a.addrs, a.err
}

func (r *sequenceResolver) lookups() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// target is a TLS server on loopback that answers as example.com, which the
// certificate of httptest names. It counts the requests it receives.
type target struct {
	url  string
	port string
	pool *x509.CertPool
	pem  []byte
	hits atomic.Int32
	srv  *httptest.Server
}

func newTarget(t *testing.T, handler http.HandlerFunc) *target {
	t.Helper()
	tg := &target{}
	tg.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tg.hits.Add(1)
		handler(w, r)
	}))
	tg.srv.StartTLS()
	t.Cleanup(tg.srv.Close)
	tg.adopt(t)
	return tg
}

func (tg *target) adopt(t *testing.T) {
	t.Helper()
	_, tg.port, _ = net.SplitHostPort(tg.srv.Listener.Addr().String())
	tg.url = "https://" + targetHost + ":" + tg.port
	tg.pool = x509.NewCertPool()
	tg.pool.AddCert(tg.srv.Certificate())
	tg.pem = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tg.srv.Certificate().Raw})
}

// sibling is a second server that listens on the same port at another loopback
// address, so a client that dials 127.0.0.2 reaches it and not the target.
func (tg *target) sibling(t *testing.T, handler http.HandlerFunc) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	l, err := net.Listen("tcp", "127.0.0.2:"+tg.port)
	require.NoError(t, err)
	require.NoError(t, srv.Listener.Close())
	srv.Listener = l
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return &hits
}

// refusingPort returns a loopback port that refuses every connection until the test ends. The socket is bound and never
// listens, so the kernel answers each connect with a refusal, and no other test can be given the port.
func refusingPort(t *testing.T) string {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Close(fd) })
	require.NoError(t, syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}))
	bound, err := syscall.Getsockname(fd)
	require.NoError(t, err)
	inet, isInet := bound.(*syscall.SockaddrInet4)
	require.True(t, isInet)
	return strconv.Itoa(inet.Port)
}

func ok(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }

func (tg *target) client(resolver faucet.Resolver) *http.Client {
	return faucet.NewHTTPClient(faucet.HTTPClientOptions{Resolver: resolver, AllowInsecureLoopback: true, RootCAs: tg.pool})
}

func get(t *testing.T, client *http.Client, url string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)
	return client.Do(req) //nolint:bodyclose // The callers close it, or get none.
}

func getErr(t *testing.T, client *http.Client, url string) error {
	t.Helper()
	resp, err := get(t, client, url)
	if err == nil {
		_ = resp.Body.Close()
	}
	return err
}

func readAll(t *testing.T, resp *http.Response) ([]byte, error) {
	t.Helper()
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func TestGuardedClientDialsTheCheckedAddressAndKeepsTheHostName(t *testing.T) {
	t.Parallel()

	var serverName, host string
	tg := newTarget(t, func(w http.ResponseWriter, r *http.Request) {
		serverName, host = r.TLS.ServerName, r.Host
		ok(w, r)
	})
	resp, err := get(t, tg.client(&sequenceResolver{answers: []lookup{answer("127.0.0.1")}}), tg.url)
	require.NoError(t, err)
	body, err := readAll(t, resp)
	require.NoError(t, err)
	require.Equal(t, "ok", string(body))
	require.Equal(t, targetHost, serverName, "TLS SNI is the host name, not the address")
	require.Equal(t, targetHost+":"+tg.port, host)

	err = getErr(t, tg.client(&sequenceResolver{answers: []lookup{answer("127.0.0.1")}}),
		"https://other.example.test:"+tg.port)
	var hostErr x509.HostnameError
	require.ErrorAs(t, err, &hostErr, "the certificate is checked against the host name, not the address")
}

func TestGuardedClientRefusesEveryNonPublicAddressForm(t *testing.T) {
	t.Parallel()

	for _, host := range []string{
		"0.0.0.0", "127.0.0.1", "127.255.255.254", "10.0.0.1", "172.16.0.1", "172.31.255.255", "192.168.1.1",
		"100.64.0.1", "100.127.255.254", "169.254.169.254", "224.0.0.1", "239.255.255.255", "240.0.0.1",
		"255.255.255.255", "192.0.0.8", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "192.88.99.1",
		"[::]", "[::1]", "[fe80::1]", "[fe80::1%25eth0]", "[fc00::1]", "[fd12:3456::1]", "[fec0::1]", "[ff02::1]",
		"[2001:db8::1]", "[2001::1]", "[2001:2::1]", "[2002:a00:1::]", "[64:ff9b::a00:1]", "[64:ff9b:1::1]",
		"[100::1]", "[3fff::1]", "[5f00::1]",
		"[::ffff:10.0.0.1]", "[::ffff:a00:1]", "[::ffff:127.0.0.1]", "[::ffff:169.254.169.254]", "[::ffff:0.0.0.0]",
		"[::10.0.0.1]", "[::127.0.0.1]", "[::ffff:0:10.0.0.1]",
		"2130706433", "0x7f.1", "127.1", "0177.0.0.1", "0x7f000001", "localhost", "localhost.", "foo.localhost", "intranet",
	} {
		resolver := &sequenceResolver{answers: []lookup{answer("8.8.8.8")}}
		client := faucet.NewHTTPClient(faucet.HTTPClientOptions{Resolver: resolver})
		for _, scheme := range []string{"https://", "http://"} {
			err := getErr(t, client, scheme+host+"/hook")
			require.ErrorIs(t, err, faucet.ErrDestinationRefused, scheme+host)
		}
		require.Zero(t, resolver.lookups(), "%s is refused before any lookup", host)
	}
}

func TestGuardedClientRefusesANameThatResolvesToANonPublicAddress(t *testing.T) {
	t.Parallel()

	tg := newTarget(t, ok)
	for name, answers := range map[string]lookup{
		"loopback":                           answer("127.0.0.1"),
		"private":                            answer("10.0.0.5"),
		"IPv4-mapped private":                answer("::ffff:10.0.0.5"),
		"link-local metadata address":        answer("169.254.169.254"),
		"CGNAT":                              answer("100.64.0.1"),
		"NAT64 of a private address":         answer("64:ff9b::a00:5"),
		"unique local":                       answer("fd00::1"),
		"unspecified":                        answer("0.0.0.0"),
		"a public address and a private one": answer("8.8.8.8", "10.0.0.5"),
		"no address":                         answer(),
		"a failed lookup":                    {err: xerrors.New("no such host")},
	} {
		client := faucet.NewHTTPClient(faucet.HTTPClientOptions{Resolver: &sequenceResolver{answers: []lookup{answers}}, RootCAs: tg.pool})
		err := getErr(t, client, tg.url)
		require.Error(t, err, name)
		if answers.err == nil {
			require.ErrorIs(t, err, faucet.ErrDestinationRefused, name)
		}
	}
	require.Zero(t, tg.hits.Load(), "the loopback server was never reached")

	mixed := tg.client(&sequenceResolver{answers: []lookup{answer("127.0.0.1", "10.0.0.5")}})
	err := getErr(t, mixed, tg.url)
	require.ErrorIs(t, err, faucet.ErrDestinationRefused, "one address that is not allowed refuses the whole answer")
	require.Zero(t, tg.hits.Load(), "an allowed address in a mixed answer is not used")

	err = getErr(t, tg.client(&sequenceResolver{answers: []lookup{answer("127.0.0.1")}}), tg.url)
	require.NoError(t, err, "the same request reaches the server once loopback is allowed")
	require.EqualValues(t, 1, tg.hits.Load())
}

func TestGuardedClientIgnoresADNSAnswerThatChangesAfterTheCheck(t *testing.T) {
	t.Parallel()

	tg := newTarget(t, ok)
	swapped := tg.sibling(t, ok)
	resolver := &sequenceResolver{answers: []lookup{answer("127.0.0.1"), answer("127.0.0.2")}}

	resp, err := get(t, tg.client(resolver), tg.url)
	require.NoError(t, err)
	_, err = readAll(t, resp)
	require.NoError(t, err)
	require.EqualValues(t, 1, tg.hits.Load(), "the checked address got the request")
	require.Zero(t, swapped.Load(), "the address of a later answer got nothing")
}

func TestGuardedClientFollowsNoRedirect(t *testing.T) {
	t.Parallel()

	elsewhere := newTarget(t, ok)
	tg := newTarget(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.srv.URL+"/internal", http.StatusFound)
	})
	resp, err := get(t, tg.client(&sequenceResolver{answers: []lookup{answer("127.0.0.1")}}), tg.url)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusFound, resp.StatusCode, "the redirect is the answer")
	require.Zero(t, elsewhere.hits.Load(), "the target of the redirect got nothing")
}

func TestGuardedClientSpeaksHTTPSOnly(t *testing.T) {
	t.Parallel()

	plain := httptest.NewServer(http.HandlerFunc(ok))
	t.Cleanup(plain.Close)
	resolver := &sequenceResolver{answers: []lookup{answer("8.8.8.8")}}

	err := getErr(t, faucet.NewHTTPClient(faucet.HTTPClientOptions{Resolver: resolver}), "http://"+targetHost+"/hook")
	require.ErrorIs(t, err, faucet.ErrDestinationRefused, "http to a public name")
	loopbackAllowed := faucet.NewHTTPClient(faucet.HTTPClientOptions{Resolver: resolver, AllowInsecureLoopback: true})
	err = getErr(t, loopbackAllowed, "http://"+targetHost+"/hook")
	require.ErrorIs(t, err, faucet.ErrDestinationRefused, "http to a public name even when loopback is allowed")
	err = getErr(t, loopbackAllowed, "http://localhost:1/hook")
	require.ErrorIs(t, err, faucet.ErrDestinationRefused, "http to a loopback name")
	require.Zero(t, resolver.lookups())

	err = getErr(t, faucet.NewHTTPClient(faucet.HTTPClientOptions{}), plain.URL)
	require.ErrorIs(t, err, faucet.ErrDestinationRefused, "http to loopback in the default client")

	resp, err := get(t, loopbackAllowed, plain.URL)
	require.NoError(t, err, "http to a loopback address needs the option")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	for _, raw := range []string{"ftp://example.com/x", "file:///etc/passwd", "gopher://example.com/", "https://user:pass@example.com/x"} {
		err = getErr(t, loopbackAllowed, raw)
		require.ErrorIs(t, err, faucet.ErrDestinationRefused, raw)
	}
}

func TestGuardedClientCapsTheResponseBody(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		size    int
		chunked bool
		tooBig  bool
	}{
		"exactly the limit":                    {size: sizeLimit},
		"one byte more, declared":              {size: sizeLimit + 1, tooBig: true},
		"one byte more, chunked":               {size: sizeLimit + 1, chunked: true, tooBig: true},
		"a chunked body of 10 times the limit": {size: 10 * sizeLimit, chunked: true, tooBig: true},
		"a declared length over the limit":     {size: 10 * sizeLimit, tooBig: true},
	} {
		tg := newTarget(t, func(w http.ResponseWriter, _ *http.Request) {
			if !tc.chunked {
				w.Header().Set("Content-Length", strconv.Itoa(tc.size))
			}
			chunk := bytes.Repeat([]byte("x"), 4096)
			for sent := 0; sent < tc.size; sent += len(chunk) {
				if _, err := w.Write(chunk[:min(len(chunk), tc.size-sent)]); err != nil {
					return
				}
				if tc.chunked {
					w.(http.Flusher).Flush()
				}
			}
		})
		resp, err := get(t, tg.client(&sequenceResolver{answers: []lookup{answer("127.0.0.1")}}), tg.url)
		if err == nil {
			var body []byte
			body, err = readAll(t, resp)
			require.LessOrEqual(t, len(body), sizeLimit, name)
		}
		if tc.tooBig {
			require.ErrorIs(t, err, faucet.ErrResponseTooLarge, name)
		} else {
			require.NoError(t, err, name)
		}
	}
}

func TestGuardedClientGivesUpOnASlowServer(t *testing.T) {
	t.Parallel()

	stallBeforeHeaders := func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
	dripBody := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for {
			if _, err := io.WriteString(w, "x"); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
	for name, tc := range map[string]struct {
		handler http.HandlerFunc
		inBody  bool
	}{
		"before the headers": {stallBeforeHeaders, false},
		"inside the body":    {dripBody, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			tg := newTarget(t, tc.handler)
			ctx, cancel := context.WithTimeout(t.Context(), 9*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, tg.url, nil)
			require.NoError(t, err)
			start := time.Now()
			resp, err := tg.client(&sequenceResolver{answers: []lookup{answer("127.0.0.1")}}).Do(req) //nolint:bodyclose // readAll closes it.
			if err == nil {
				_, err = readAll(t, resp)
			}
			if err != nil {
				var netErr net.Error
				require.ErrorAs(t, err, &netErr)
				require.True(t, netErr.Timeout(), "the failure is a timeout")
			} else {
				// When the client gives up it closes the TLS connection, which sends close_notify first. A server that answers
				// that by ending its response can reach a read that is still waiting, and the read then ends without an error.
				require.True(t, tc.inBody, "a stall before the headers always fails")
			}
			require.Greater(t, time.Since(start), 4*time.Second, "it waited for the limit")
			require.Less(t, time.Since(start), 9*time.Second, "and not much longer")
		})
	}
}

const proxyHelperEnv = "FAUCET_PROXY_HELPER_URL"

func TestGuardedClientIgnoresTheProxyVariables(t *testing.T) {
	t.Parallel()

	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = proxy.Close() })
	var proxied atomic.Int32
	go func() {
		for {
			conn, err := proxy.Accept()
			if err != nil {
				return
			}
			proxied.Add(1)
			_ = conn.Close()
		}
	}()
	tg := newTarget(t, ok)

	env := []string{proxyHelperEnv + "=" + tg.url, "FAUCET_PROXY_HELPER_CA=" + string(tg.pem)}
	for _, name := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY"} {
		env = append(env, name+"=http://"+proxy.Addr().String())
	}
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); !strings.HasSuffix(strings.ToUpper(name), "_PROXY") {
			env = append(env, kv)
		}
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestProxyHelperProcess$", "-test.v") //nolint:gosec // The test binary itself.
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "the helper process: %s", out)
	require.Contains(t, string(out), "--- PASS: TestProxyHelperProcess")
	require.EqualValues(t, 1, tg.hits.Load(), "the request went straight to the server")
	require.Zero(t, proxied.Load(), "and never to the proxy")
}

// TestProxyHelperProcess runs inside the child process of
// TestGuardedClientIgnoresTheProxyVariables. A process reads its proxy
// variables once, so the test needs a process of its own.
func TestProxyHelperProcess(t *testing.T) { //nolint:paralleltest // The child process runs this one test.
	raw := os.Getenv(proxyHelperEnv)
	if raw == "" {
		t.Skip("runs only as the child of TestGuardedClientIgnoresTheProxyVariables")
	}
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM([]byte(os.Getenv("FAUCET_PROXY_HELPER_CA"))))
	client := faucet.NewHTTPClient(faucet.HTTPClientOptions{
		Resolver:              &sequenceResolver{answers: []lookup{answer("127.0.0.1")}},
		AllowInsecureLoopback: true,
		RootCAs:               pool,
	})
	resp, err := get(t, client, raw)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()
}
