package faucet_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	standardwebhooks "github.com/standard-webhooks/standard-webhooks/libraries/go"
	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

const pathToken = "tok-7c1f4e9a" //nolint:gosec // A marker that the tests look for, not a credential.

type received struct {
	method string
	path   string
	header http.Header
	body   []byte
}

func (r received) challenge() string {
	var msg struct {
		Challenge string `json:"challenge"`
	}
	_ = json.Unmarshal(r.body, &msg)
	return msg.Challenge
}

// endpoint is a callback receiver. Its behavior decides what it answers, and it
// records every request.
type endpoint struct {
	*target
	mu   sync.Mutex
	got  []received
	path string
}

func newEndpoint(t *testing.T, behave func(w http.ResponseWriter, r *http.Request, got received)) *endpoint {
	t.Helper()
	ep := &endpoint{}
	ep.target = newTarget(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got := received{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body}
		ep.mu.Lock()
		ep.got = append(ep.got, got)
		ep.mu.Unlock()
		behave(w, r, got)
	})
	ep.path = "/hook/" + pathToken
	return ep
}

func (e *endpoint) requests() []received {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]received(nil), e.got...)
}

func (e *endpoint) callback() string { return e.url + e.path }

func echo(w http.ResponseWriter, got received) {
	_, _ = fmt.Fprintf(w, "{\n  \"challenge\": %q\n}\n", got.challenge())
}

// subscriberEchoes is a correct subscriber: it checks the signature with its
// secret, and only then answers the challenge.
func subscriberEchoes(secret string) func(http.ResponseWriter, *http.Request, received) {
	return func(w http.ResponseWriter, _ *http.Request, got received) {
		verifier, err := standardwebhooks.NewWebhook(secret)
		if err != nil || verifier.VerifyIgnoringTimestamp(got.body, got.header) != nil {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		var msg struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(got.body, &msg) != nil || msg.Type != "verification" {
			http.Error(w, "not a verification", http.StatusBadRequest)
			return
		}
		echo(w, got)
	}
}

func resolveToLoopback() *sequenceResolver {
	return &sequenceResolver{answers: []lookup{answer("127.0.0.1")}}
}

func newHandshakeHarness(t *testing.T, tg *target, resolver faucet.Resolver) *webhookHarness {
	t.Helper()
	return newWebhookHarness(t, func(d *faucet.Deps) {
		verifier, err := faucet.NewCallbackHandshake(faucet.HandshakeOptions{Client: tg.client(resolver), Clock: d.Clock})
		require.NoError(t, err)
		d.Webhook.Callbacks = verifier
	})
}

var challengeShape = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

func TestHandshakeAcceptsASubscriberThatEchoesTheChallenge(t *testing.T) {
	t.Parallel()

	secret := newSecret(t, 32)
	ep := newEndpoint(t, subscriberEchoes(secret))
	h := newHandshakeHarness(t, ep.target, resolveToLoopback())

	res := h.subscribe(subscribeTo(repoArgs(), ep.callback(), secret))
	reqs := ep.requests()
	require.Len(t, reqs, 1)
	got := reqs[0]
	require.Equal(t, http.MethodPost, got.method)
	require.Equal(t, ep.path, got.path)
	require.Equal(t, "application/json", got.header.Get("Content-Type"))
	require.True(t, strings.HasPrefix(got.header.Get("webhook-id"), "msg_verification_"))
	require.Equal(t, res.ID, got.header.Get("X-MCP-Subscription-Id"), "the receiver can route by the id that subscribe returns")
	require.Equal(t, strconv.FormatInt(h.clock.Now().Unix(), 10), got.header.Get("webhook-timestamp"))
	verifier, err := standardwebhooks.NewWebhook(secret)
	require.NoError(t, err)
	require.NoError(t, verifier.VerifyIgnoringTimestamp(got.body, got.header), "signed with the subscriber's secret")

	var msg map[string]string
	require.NoError(t, json.Unmarshal(got.body, &msg))
	require.Len(t, msg, 2)
	require.Equal(t, "verification", msg["type"])
	require.Regexp(t, challengeShape, msg["challenge"], "32 random bytes, URL-safe")
	require.Empty(t, got.header.Get("Authorization"), "no credential travels to the endpoint")
	require.Empty(t, got.header.Get("Cookie"))
	require.NotContains(t, string(got.body), testToken)
	require.Len(t, h.rows(), 1)

	h.subscribe(subscribeTo(repoArgs(), ep.callback()+"?second", secret))
	second := ep.requests()
	require.Len(t, second, 2)
	require.NotEqual(t, msg["challenge"], second[1].challenge(), "a nonce is used once")
	require.NotEqual(t, got.header.Get("webhook-id"), second[1].header.Get("webhook-id"))
}

func TestHandshakeRefusesAnEndpointThatDoesNotEchoTheChallenge(t *testing.T) {
	t.Parallel()

	elsewhere := newTarget(t, ok)
	for name, tc := range map[string]struct {
		reason string
		answer func(w http.ResponseWriter, r *http.Request, got received)
	}{
		"a wrong challenge": {"challenge_failed", func(w http.ResponseWriter, _ *http.Request, _ received) {
			_, _ = io.WriteString(w, `{"challenge":"`+strings.Repeat("A", 43)+`"}`)
		}},
		"an empty body": {"challenge_failed", func(http.ResponseWriter, *http.Request, received) {}},
		"no content":    {"challenge_failed", func(w http.ResponseWriter, _ *http.Request, _ received) { w.WriteHeader(http.StatusNoContent) }},
		"the request body reflected": {"challenge_failed", func(w http.ResponseWriter, _ *http.Request, got received) {
			_, _ = w.Write(got.body)
		}},
		"an extra member": {"challenge_failed", func(w http.ResponseWriter, _ *http.Request, got received) {
			_, _ = fmt.Fprintf(w, `{"challenge":%q,"ok":true}`, got.challenge())
		}},
		"the challenge twice, the wrong one first": {"challenge_failed", func(w http.ResponseWriter, _ *http.Request, got received) {
			_, _ = fmt.Fprintf(w, `{"challenge":"x","challenge":%q}`, got.challenge())
		}},
		"a challenge that is not a string": {"challenge_failed", func(w http.ResponseWriter, _ *http.Request, _ received) {
			_, _ = io.WriteString(w, `{"challenge":123}`)
		}},
		"text that is not JSON": {"challenge_failed", func(w http.ResponseWriter, _ *http.Request, got received) {
			_, _ = io.WriteString(w, got.challenge())
		}},
		"the challenge in an array": {"challenge_failed", func(w http.ResponseWriter, _ *http.Request, got received) {
			_, _ = fmt.Fprintf(w, `[{"challenge":%q}]`, got.challenge())
		}},
		"the right object and then more": {"challenge_failed", func(w http.ResponseWriter, _ *http.Request, got received) {
			_, _ = fmt.Fprintf(w, `{"challenge":%q} {}`, got.challenge())
		}},
		"the right answer padded past 1 KiB": {"challenge_failed", func(w http.ResponseWriter, _ *http.Request, got received) {
			_, _ = fmt.Fprintf(w, `{"challenge":%q}%s`, got.challenge(), strings.Repeat(" ", 2048))
		}},
		"the right answer with a 3xx status": {"challenge_failed", func(w http.ResponseWriter, r *http.Request, got received) {
			w.Header().Set("Location", elsewhere.srv.URL+"/internal")
			w.WriteHeader(http.StatusFound)
			echo(w, got)
		}},
		"a 401": {"http_4xx", func(w http.ResponseWriter, _ *http.Request, got received) {
			w.WriteHeader(http.StatusUnauthorized)
			echo(w, got)
		}},
		"a 404": {"http_4xx", func(w http.ResponseWriter, _ *http.Request, _ received) { http.NotFound(w, nil) }},
		"a 500": {"http_5xx", func(w http.ResponseWriter, _ *http.Request, _ received) {
			w.WriteHeader(http.StatusInternalServerError)
		}},
		"a 503 with the right answer": {"http_5xx", func(w http.ResponseWriter, _ *http.Request, got received) {
			w.WriteHeader(http.StatusServiceUnavailable)
			echo(w, got)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			secret := newSecret(t, 32)
			ep := newEndpoint(t, tc.answer)
			h := newHandshakeHarness(t, ep.target, resolveToLoopback())

			rpcErr := h.subscribeError(subscribeTo(repoArgs(), ep.callback(), secret))
			requireRefused(t, h, rpcErr, tc.reason, secret, ep)
		})
	}
	require.Zero(t, elsewhere.hits.Load(), "no redirect was followed")
}

func TestHandshakeNamesOnlyTheFailureCategory(t *testing.T) {
	t.Parallel()

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	_, closedPort, _ := net.SplitHostPort(closed.Addr().String())
	require.NoError(t, closed.Close())
	plain := httptest.NewServer(http.HandlerFunc(ok))
	t.Cleanup(plain.Close)
	_, plainPort, _ := net.SplitHostPort(plain.Listener.Addr().String())

	for name, tc := range map[string]struct {
		host    func(tg *target) string
		resolve lookup
		reason  string
	}{
		"nothing listens":                            {func(*target) string { return "https://" + targetHost + ":" + closedPort }, answer("127.0.0.1"), "connection_refused"},
		"a certificate for another host":             {func(tg *target) string { return "https://wrong.example.test:" + tg.port }, answer("127.0.0.1"), "tls_error"},
		"a server that does not speak TLS":           {func(*target) string { return "https://" + targetHost + ":" + plainPort }, answer("127.0.0.1"), "tls_error"},
		"a host name that points inside the network": {func(tg *target) string { return tg.url }, answer("10.0.0.5"), "connection_refused"},
		"a host name that does not resolve":          {func(tg *target) string { return tg.url }, lookup{err: fmt.Errorf("no such host")}, "connection_refused"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			secret := newSecret(t, 32)
			ep := newEndpoint(t, subscriberEchoes(secret))
			h := newHandshakeHarness(t, ep.target, &sequenceResolver{answers: []lookup{tc.resolve}})
			ep.path = "/hook/" + pathToken
			rpcErr := h.subscribeError(subscribeTo(repoArgs(), tc.host(ep.target)+ep.path, secret))
			requireRefused(t, h, rpcErr, tc.reason, secret, ep)
			require.Zero(t, ep.hits.Load(), "the endpoint on loopback was not reached")
		})
	}
}

func TestHandshakeTimesOutOnAnEndpointThatNeverAnswers(t *testing.T) {
	t.Parallel()

	secret := newSecret(t, 32)
	ep := newEndpoint(t, func(_ http.ResponseWriter, r *http.Request, _ received) { <-r.Context().Done() })
	h := newHandshakeHarness(t, ep.target, resolveToLoopback())

	start := time.Now()
	rpcErr := h.subscribeError(subscribeTo(repoArgs(), ep.callback(), secret))
	requireRefused(t, h, rpcErr, "timeout", secret, ep)
	require.Greater(t, time.Since(start), 4*time.Second)
	require.Less(t, time.Since(start), 9*time.Second)
}

func TestHandshakeAcceptsWhateverFormattingTheEndpointUses(t *testing.T) {
	t.Parallel()

	for name, format := range map[string]string{
		"compact":                 `{"challenge":%q}`,
		"spaced like Python":      `{"challenge": %q}`,
		"indented with a newline": "{\n\t\"challenge\" :\t%q\n}\n",
		"a 201 status":            `{"challenge":%q}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			secret := newSecret(t, 32)
			ep := newEndpoint(t, func(w http.ResponseWriter, _ *http.Request, got received) {
				if name == "a 201 status" {
					w.WriteHeader(http.StatusCreated)
				}
				_, _ = fmt.Fprintf(w, format, got.challenge())
			})
			h := newHandshakeHarness(t, ep.target, resolveToLoopback())
			h.subscribe(subscribeTo(repoArgs(), ep.callback(), secret))
			require.Len(t, h.rows(), 1)
		})
	}
}

func TestHandshakeIsRateLimitedPerHost(t *testing.T) {
	t.Parallel()

	secret := newSecret(t, 32)
	var answering atomic.Bool
	ep := newEndpoint(t, func(w http.ResponseWriter, _ *http.Request, got received) {
		if answering.Load() {
			echo(w, got)
			return
		}
		http.NotFound(w, nil)
	})
	h := newHandshakeHarness(t, ep.target, resolveToLoopback())
	url := func(i int) string { return fmt.Sprintf("%s/hook/%s-%d", ep.url, pathToken, i) }

	for i := range 5 {
		rpcErr := h.subscribeError(subscribeTo(repoArgs(), url(i), secret))
		require.EqualValues(t, codeCallbackEndpoint, rpcErr.Code, "attempt %d", i)
	}
	rpcErr := h.subscribeError(subscribeTo(repoArgs(), url(5), secret))
	require.EqualValues(t, codeResourceExhausted, rpcErr.Code, "the sixth handshake in a row to one host")
	require.JSONEq(t, `{"limit":"callbackVerifications"}`, string(rpcErr.Data))
	require.EqualValues(t, 5, ep.hits.Load(), "a limited handshake sends nothing")
	requireNothingStored(t, h, "a limited subscribe")

	otherHost := h.subscribeError(subscribeTo(repoArgs(), "https://other.example.test:"+ep.port+"/hook/"+pathToken, secret))
	require.EqualValues(t, codeCallbackEndpoint, otherHost.Code, "another host has its own bucket")

	h.clock.Advance(12 * time.Second)
	rpcErr = h.subscribeError(subscribeTo(repoArgs(), url(6), secret))
	require.EqualValues(t, codeCallbackEndpoint, rpcErr.Code, "a token is back after 12 seconds")
	require.EqualValues(t, 6, ep.hits.Load())
	rpcErr = h.subscribeError(subscribeTo(repoArgs(), url(7), secret))
	require.EqualValues(t, codeResourceExhausted, rpcErr.Code, "and only one")

	h.clock.Advance(time.Minute)
	answering.Store(true)
	for i := 100; i < 130; i++ {
		h.subscribe(subscribeTo(repoArgs(), url(i), secret))
	}
	require.Len(t, h.rows(), 30, "an endpoint that echoes gives each token back, so it is never limited")
}

func TestHandshakeIsCachedPerLoginAndURL(t *testing.T) {
	t.Parallel()

	secret := newSecret(t, 32)
	ep := newEndpoint(t, subscriberEchoes(secret))
	h := newHandshakeHarness(t, ep.target, resolveToLoopback())

	h.subscribe(subscribeTo(repoArgs(), ep.callback(), secret))
	h.subscribe(subscribeTo(repoArgs(), ep.callback(), secret))
	h.subscribe(subscribeTo(repoArgs("pr", 7), ep.callback(), secret))
	require.Len(t, ep.requests(), 1, "a refresh and another PR on the same URL send nothing")

	h.as("hubot")
	h.subscribe(subscribeTo(repoArgs(), ep.callback(), secret))
	require.Len(t, ep.requests(), 2, "another login is verified again")

	h.as("octocat")
	h.clock.Advance(11 * time.Minute)
	h.subscribe(subscribeTo(repoArgs(), ep.callback(), secret))
	require.Len(t, ep.requests(), 3, "the verification expires with the 10 minutes of a subscription")
}

func TestVerifierGetsTheSubscriptionAndASignerButNotTheSecret(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	secret := newSecret(t, 32)
	res := h.subscribe(subscribeTo(repoArgs(), callbackURL, secret))

	require.Len(t, h.verifier.requests, 1)
	req := h.verifier.requests[0]
	require.Equal(t, "octocat", req.Principal.Login)
	require.Equal(t, callbackURL, req.URL)
	require.Equal(t, res.ID, req.SubscriptionID)

	body := []byte(`{"type":"verification"}`)
	headers, err := req.Sign("msg_1", h.clock.Now(), body)
	require.NoError(t, err)
	verifier, err := standardwebhooks.NewWebhook(secret)
	require.NoError(t, err)
	require.NoError(t, verifier.VerifyIgnoringTimestamp(body, headers), "the request signs with the subscriber's secret")

	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%q"} {
		out := fmt.Sprintf(verb, req)
		require.NotContains(t, out, strings.TrimPrefix(secret, "whsec_"), verb)
		require.NotContains(t, out, testToken, verb)
	}
}

func TestNewCallbackHandshakeNeedsAClient(t *testing.T) {
	t.Parallel()

	_, err := faucet.NewCallbackHandshake(faucet.HandshakeOptions{})
	require.Error(t, err)
}

// requireRefused checks everything that a refused handshake owes the caller:
// the SEP category, no row, and no secret, callback path or nonce in the
// answer or the log.
func requireRefused(t *testing.T, h *webhookHarness, rpcErr *jsonrpc.Error, reason, secret string, ep *endpoint) {
	t.Helper()
	require.EqualValues(t, codeCallbackEndpoint, rpcErr.Code)
	require.JSONEq(t, fmt.Sprintf(`{"reason":%q}`, reason), string(rpcErr.Data))
	requireNothingStored(t, h, "a refused handshake")

	needles := []string{secret, strings.TrimPrefix(secret, "whsec_"), pathToken, ep.url}
	for _, got := range ep.requests() {
		needles = append(needles, got.challenge())
	}
	answer := rpcErr.Message + string(rpcErr.Data)
	for _, needle := range needles {
		if needle == "" {
			continue
		}
		require.NotContains(t, answer, needle, "the answer to the caller")
		require.NotContains(t, h.logs.String(), needle, "the log")
	}
}
