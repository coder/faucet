package faucet_test

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	mrand "math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
)

const (
	callbackURL = "https://hooks.example.test/mcp/hook"

	codeResourceExhausted = -32025
	codeUnsupported       = -32026
	codeCallbackEndpoint  = -32027
)

var testWebhookKey = func() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(0xA0 + i%16)
	}
	return key
}()

type fakeVerifier struct {
	mu       sync.Mutex
	calls    []string
	requests []faucet.CallbackRequest
	err      error
}

func (v *fakeVerifier) VerifyCallback(_ context.Context, req faucet.CallbackRequest) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls = append(v.calls, req.Principal.Login+" "+req.URL)
	v.requests = append(v.requests, req)
	return v.err
}

func (v *fakeVerifier) callCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.calls)
}

func (v *fakeVerifier) setErr(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.err = err
}

type webhookHarness struct {
	*eventsHarness
	verifier *fakeVerifier
	login    *atomic.Value
}

func (h *webhookHarness) as(login string) { h.login.Store(login) }

func newWebhookHarness(t *testing.T, mutate ...func(*faucet.Deps)) *webhookHarness {
	t.Helper()
	key, err := faucet.NewWebhookKey(testWebhookKey)
	require.NoError(t, err)
	verifier := &fakeVerifier{}
	login := &atomic.Value{}
	login.Store("octocat")
	h := newEventsHarness(t, append([]func(*faucet.Deps){func(d *faucet.Deps) {
		d.Webhook = &faucet.WebhookDeps{Key: key, Callbacks: verifier}
		d.PrincipalFrom = func(context.Context) (faucet.Principal, error) {
			l, ok := login.Load().(string)
			if !ok {
				return faucet.Principal{}, xerrors.New("no login")
			}
			return faucet.NewPrincipal(l, testToken)
		}
	}}, mutate...)...)
	return &webhookHarness{eventsHarness: h, verifier: verifier, login: login}
}

func newSecret(t *testing.T, size int) string {
	t.Helper()
	b := make([]byte, size)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return "whsec_" + base64.StdEncoding.EncodeToString(b)
}

func deliveryTo(url, secret string) json.RawMessage {
	return raw(map[string]any{"mode": "webhook", "url": url, "secret": secret})
}

func subscribeTo(arguments map[string]any, url, secret string) *rawParams {
	return &rawParams{Name: "ci.run_completed", Arguments: raw(arguments), Delivery: deliveryTo(url, secret)}
}

type subscribed struct {
	ResultType    string `json:"resultType"`
	ID            string `json:"id"`
	RefreshBefore string `json:"refreshBefore"`
	Cursor        string `json:"cursor"`
	Truncated     bool   `json:"truncated"`
}

func (h *webhookHarness) subscribe(params *rawParams) subscribed {
	h.t.Helper()
	res, err := h.call(methodSubscribe, params)
	require.NoError(h.t, err)
	for _, field := range []string{"resultType", "id", "refreshBefore", "cursor", "truncated"} {
		require.Contains(h.t, res.fields, field, "every subscribe result carries %s", field)
	}
	return decode[subscribed](h.t, res)
}

func (h *webhookHarness) callError(method string, params *rawParams) *jsonrpc.Error {
	h.t.Helper()
	res, err := h.call(method, params)
	require.Error(h.t, err, "result: %v", res)
	var rpcErr *jsonrpc.Error
	require.ErrorAs(h.t, err, &rpcErr)
	return rpcErr
}

func (h *webhookHarness) subscribeError(params *rawParams) *jsonrpc.Error {
	h.t.Helper()
	return h.callError(methodSubscribe, params)
}

func (h *webhookHarness) unsubscribe(args map[string]any, url string) *jsonrpc.Error {
	h.t.Helper()
	res, err := h.call(methodUnsubscribe, &rawParams{
		Name: "ci.run_completed", Arguments: raw(args), Delivery: raw(map[string]any{"url": url}),
	})
	if err == nil {
		require.JSONEq(h.t, `"complete"`, string(res.fields["resultType"]))
		return nil
	}
	var rpcErr *jsonrpc.Error
	require.ErrorAs(h.t, err, &rpcErr)
	return rpcErr
}

func (h *webhookHarness) rows() []faucet.Subscription { return h.store.subscriptions() }

func requireNothingStored(t *testing.T, h *webhookHarness, why string) {
	t.Helper()
	require.Empty(t, h.rows(), why)
}

// openSealed decrypts a stored secret with code that is independent of the
// service: the layout is version, nonce, then AES-256-GCM with the row's key
// hash as additional data.
func openSealed(t *testing.T, key []byte, row faucet.Subscription) (string, error) {
	t.Helper()
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	sealed := row.SecretEnc
	require.Greater(t, len(sealed), 1+gcm.NonceSize(), "version, nonce and ciphertext")
	require.EqualValues(t, 1, sealed[0], "layout version")
	plain, err := gcm.Open(nil, sealed[1:1+gcm.NonceSize()], sealed[1+gcm.NonceSize():], row.KeyHash[:])
	return string(plain), err
}

func TestSubscribeStoresASubscriptionForAReadableRepository(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	secret := newSecret(t, 32)
	head := h.insert(repoID, "ci.run_completed", "").ID
	h.clock.Advance(time.Second)

	got := h.subscribe(subscribeTo(repoArgs("pr", 7), callbackURL, secret))
	require.Equal(t, "complete", got.ResultType)
	require.False(t, got.Truncated)
	require.Equal(t, fmt.Sprintf("v1.%d", head), got.Cursor, "delivery starts after the events that exist now")
	require.Equal(t, h.clock.Now().Add(10*time.Minute).UTC().Format(time.RFC3339), got.RefreshBefore)

	rows := h.rows()
	require.Len(t, rows, 1)
	row := rows[0]
	require.Equal(t, "octocat", row.Principal)
	require.Equal(t, callbackURL, row.CallbackURL)
	require.Equal(t, "ci.run_completed", row.EventType)
	require.JSONEq(t, `{"repo":"octo/repo","pr":7}`, string(row.Arguments))
	require.EqualValues(t, repoID, row.RepoID, "GitHub's repository ID, not the name")
	require.Equal(t, got.ID, row.KeyHash.ID())
	require.Equal(t, head, row.Cursor)
	require.Equal(t, 1, h.verifier.callCount())
}

func TestSubscribeDeniedAndMissingRepositoriesLookTheSameAndStoreNothing(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	secret := newSecret(t, 32)

	denied := h.subscribeError(subscribeTo(args("repo", deniedRepo), callbackURL, secret))
	missing := h.subscribeError(subscribeTo(args("repo", "octo/does-not-exist"), callbackURL, secret))
	require.EqualValues(t, codeForbidden, denied.Code)
	require.Equal(t, denied, missing, "a repository that exists and one that does not give one answer")
	requireNothingStored(t, h, "a denied subscribe")
	require.Zero(t, h.verifier.callCount(), "an unreadable repository is never checked against the callback")

	h.authz.mu.Lock()
	h.authz.err = faucet.ErrInvalidToken
	h.authz.mu.Unlock()
	rejected := h.subscribeError(subscribeTo(repoArgs(), callbackURL, secret))
	require.EqualValues(t, codeForbidden, rejected.Code, "a rejected token")

	h.authz.mu.Lock()
	h.authz.err = xerrors.New("GitHub is down")
	h.authz.mu.Unlock()
	down := h.subscribeError(subscribeTo(repoArgs(), callbackURL, secret))
	require.EqualValues(t, codeInternal, down.Code, "an access check that could not run is not a denial")
	requireNothingStored(t, h, "a failed access check")
}

func TestSubscribeRequiresAnAuthenticatedPrincipal(t *testing.T) {
	t.Parallel()

	for name, principal := range map[string]func(context.Context) (faucet.Principal, error){
		"no principal":        func(context.Context) (faucet.Principal, error) { return faucet.Principal{}, xerrors.New("none") },
		"principal no login":  func(context.Context) (faucet.Principal, error) { return faucet.Principal{}, nil },
		"principal and login": nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newWebhookHarness(t, func(d *faucet.Deps) {
				if principal != nil {
					d.PrincipalFrom = principal
				}
			})
			params := subscribeTo(repoArgs(), callbackURL, newSecret(t, 32))
			if principal == nil {
				h.subscribe(params)
				return
			}
			require.EqualValues(t, codeForbidden, h.subscribeError(params).Code)
			requireNothingStored(t, h, name)
			unsub := h.unsubscribe(repoArgs(), callbackURL)
			require.NotNil(t, unsub)
			require.EqualValues(t, codeForbidden, unsub.Code)
		})
	}
}

func TestSubscribeGrantsAtMostTenMinutes(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		ttl  json.RawMessage
		want time.Duration
	}{
		"omitted":                          {nil, 10 * time.Minute},
		"null asks for none":               {json.RawMessage("null"), 10 * time.Minute},
		"an hour":                          {json.RawMessage("3600000"), 10 * time.Minute},
		"just above the cap":               {json.RawMessage("600001"), 10 * time.Minute},
		"beyond int64":                     {json.RawMessage("99999999999999999999"), 10 * time.Minute},
		"exactly the cap":                  {json.RawMessage("600000"), 10 * time.Minute},
		"a minute":                         {json.RawMessage("60000"), time.Minute},
		"below the floor":                  {json.RawMessage("1"), 30 * time.Second},
		"zero":                             {json.RawMessage("0"), 30 * time.Second},
		"negative":                         {json.RawMessage("-5"), 30 * time.Second},
		"very negative":                    {json.RawMessage("-99999999999999999999"), 30 * time.Second},
		"just above the floor":             {json.RawMessage("31000"), 31 * time.Second},
		"max int64":                        {json.RawMessage("9223372036854775807"), 10 * time.Minute},
		"min int64":                        {json.RawMessage("-9223372036854775808"), 30 * time.Second},
		"largest that fits in nanoseconds": {json.RawMessage("-9223372036854"), 30 * time.Second},
		"first that wraps to 292 years":    {json.RawMessage("-9223372036855"), 30 * time.Second},
		"wraps to 291 years":               {json.RawMessage("-9300000000000"), 30 * time.Second},
		"wraps to 14 years":                {json.RawMessage("-18000000000000"), 30 * time.Second},
		"wraps to a few minutes":           {json.RawMessage("-18446744073710"), 30 * time.Second},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newWebhookHarness(t)
			params := subscribeTo(repoArgs(), callbackURL, newSecret(t, 32))
			params.TTLMs = tc.ttl
			got := h.subscribe(params)
			want := h.clock.Now().Add(tc.want)
			require.Equal(t, want.UTC().Format(time.RFC3339), got.RefreshBefore)
			require.Equal(t, want.Truncate(time.Second), h.rows()[0].ExpiresAt, "the stored expiry is the grant")
		})
	}

	for name, ttl := range map[string]string{"string": `"600000"`, "fraction": `1.5`, "object": `{}`, "bool": `true`} {
		t.Run("refuses "+name, func(t *testing.T) {
			t.Parallel()

			h := newWebhookHarness(t)
			params := subscribeTo(repoArgs(), callbackURL, newSecret(t, 32))
			params.TTLMs = json.RawMessage(ttl)
			require.EqualValues(t, codeInvalidParams, h.subscribeError(params).Code)
			requireNothingStored(t, h, "a bad ttlMs")
		})
	}
}

func TestSubscribeRefusesBadSecretsWithoutEchoingThem(t *testing.T) {
	t.Parallel()

	good := newSecret(t, 32)
	encoded := strings.TrimPrefix(good, "whsec_")
	bad := map[string]json.RawMessage{
		"missing":             nil,
		"null":                json.RawMessage("null"),
		"a number":            json.RawMessage("123456789012"),
		"an object":           json.RawMessage(`{"v":"x"}`),
		"empty":               raw(""),
		"prefix only":         raw("whsec_"),
		"no prefix":           raw(encoded),
		"wrong prefix":        raw("whsec-" + encoded),
		"upper-case prefix":   raw("WHSEC_" + encoded),
		"not base64":          raw("whsec_" + strings.Repeat("!", 40)),
		"url-safe base64":     raw("whsec_" + strings.NewReplacer("+", "-", "/", "_").Replace(base64.StdEncoding.EncodeToString(append([]byte{0xfb, 0xff, 0xfe}, make([]byte, 29)...)))),
		"unpadded":            raw("whsec_" + base64.RawStdEncoding.EncodeToString(make([]byte, 25))),
		"newline inside":      raw("whsec_" + encoded[:20] + "\n" + encoded[20:]),
		"space inside":        raw("whsec_" + encoded[:20] + " " + encoded[20:]),
		"trailing newline":    raw(good + "\n"),
		"23 bytes":            raw("whsec_" + base64.StdEncoding.EncodeToString(make([]byte, 23))),
		"65 bytes":            raw("whsec_" + base64.StdEncoding.EncodeToString(make([]byte, 65))),
		"far too long":        raw("whsec_" + strings.Repeat("A", 100000)),
		"non-zero pad bits":   raw("whsec_" + strings.TrimSuffix(base64.StdEncoding.EncodeToString(make([]byte, 25)), "AA==") + "AB=="),
		"secret in the array": json.RawMessage(`["` + good + `"]`),
	}
	for name, secret := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newWebhookHarness(t)
			delivery := map[string]json.RawMessage{"mode": raw("webhook"), "url": raw(callbackURL)}
			if secret != nil {
				delivery["secret"] = secret
			}
			rpcErr := h.subscribeError(&rawParams{Name: "ci.run_completed", Arguments: raw(repoArgs()), Delivery: raw(delivery)})
			require.EqualValues(t, codeInvalidParams, rpcErr.Code)
			requireNothingStored(t, h, "a bad secret")
			require.Zero(t, h.authz.callCount(), "a bad secret is refused before GitHub is asked")

			text := rpcErr.Message + string(rpcErr.Data)
			for i := 0; i+8 <= len(secret) && i < 2000; i++ {
				chunk := strings.Trim(string(secret[i:i+8]), `"`)
				if len(chunk) == 8 && chunk != "whsec_" && !strings.ContainsAny(chunk, "{}[]:\\") {
					require.NotContains(t, text, chunk, "the error quotes the secret")
				}
			}
		})
	}

	for _, size := range []int{24, 25, 32, 64} {
		t.Run(fmt.Sprintf("accepts %d bytes", size), func(t *testing.T) {
			t.Parallel()

			h := newWebhookHarness(t)
			h.subscribe(subscribeTo(repoArgs(), callbackURL, newSecret(t, size)))
			require.Len(t, h.rows(), 1)
		})
	}
}

func TestSubscribeSealsTheSecretAtRest(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	secret := newSecret(t, 32)
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	require.NoError(t, err)

	h.subscribe(subscribeTo(repoArgs(), callbackURL, secret))
	h.subscribe(subscribeTo(repoArgs(), "https://hooks.example.test/other", secret))
	rows := h.rows()
	require.Len(t, rows, 2)

	for _, row := range rows {
		require.NotContains(t, string(row.SecretEnc), secret, "the stored secret is not the plaintext")
		require.NotContains(t, string(row.SecretEnc), strings.TrimPrefix(secret, "whsec_"))
		require.NotContains(t, string(row.SecretEnc), string(decoded))
		plain, err := openSealed(t, testWebhookKey, row)
		require.NoError(t, err, "the injected key opens the row")
		require.Equal(t, secret, plain)

		wrongKey := append([]byte(nil), testWebhookKey...)
		wrongKey[0] ^= 1
		_, err = openSealed(t, wrongKey, row)
		require.Error(t, err, "another key does not")

		moved := row
		moved.KeyHash = rows[0].KeyHash
		if row.KeyHash == rows[0].KeyHash {
			moved.KeyHash = rows[1].KeyHash
		}
		_, err = openSealed(t, testWebhookKey, moved)
		require.Error(t, err, "a ciphertext moved to another row does not open")
	}
	require.NotEqual(t, rows[0].SecretEnc, rows[1].SecretEnc, "one secret seals differently on two rows")
}

func TestSubscribeNeverWritesTheSecretAnywhere(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	secret := newSecret(t, 32)
	body := strings.TrimPrefix(secret, "whsec_")
	var seen []string
	record := func(what string, v any) {
		b, err := json.Marshal(v)
		require.NoError(t, err, what)
		seen = append(seen, what+": "+string(b))
	}

	ok := subscribeTo(repoArgs(), callbackURL, secret)
	res, err := h.call(methodSubscribe, ok)
	require.NoError(t, err)
	record("result", res.fields)

	for name, params := range map[string]*rawParams{
		"denied repository": subscribeTo(args("repo", deniedRepo), callbackURL, secret),
		"bad URL":           subscribeTo(repoArgs(), "http://hooks.example.test/x", secret),
		"bad arguments":     subscribeTo(args("repo", "no-slash"), callbackURL, secret),
		"unknown event":     {Name: "nope", Arguments: raw(repoArgs()), Delivery: deliveryTo(callbackURL, secret)},
		"bad cursor":        {Name: "ci.run_completed", Arguments: raw(repoArgs()), Delivery: deliveryTo(callbackURL, secret), Cursor: raw("garbage")},
		"bad ttl":           {Name: "ci.run_completed", Arguments: raw(repoArgs()), Delivery: deliveryTo(callbackURL, secret), TTLMs: raw("x")},
		"bad mode":          {Name: "ci.run_completed", Arguments: raw(repoArgs()), Delivery: raw(map[string]any{"mode": "push", "url": callbackURL, "secret": secret})},
		"delivery array":    {Name: "ci.run_completed", Arguments: raw(repoArgs()), Delivery: raw([]string{secret})},
		"delivery string":   {Name: "ci.run_completed", Arguments: raw(repoArgs()), Delivery: raw(secret)},
	} {
		_, err := h.call(methodSubscribe, params)
		require.Error(t, err, name)
		seen = append(seen, name+": "+err.Error())
	}

	h.verifier.setErr(&faucet.CallbackError{Reason: faucet.CallbackChallengeFailed})
	_, err = h.call(methodSubscribe, subscribeTo(repoArgs(), "https://hooks.example.test/refused", secret))
	require.Error(t, err)
	seen = append(seen, "verifier refusal: "+err.Error())
	h.verifier.setErr(xerrors.New("verifier broke"))
	_, err = h.call(methodSubscribe, subscribeTo(repoArgs(), "https://hooks.example.test/broken", secret))
	require.Error(t, err)
	seen = append(seen, "verifier failure: "+err.Error())
	h.verifier.setErr(nil)

	h.store.mu.Lock()
	h.store.err = xerrors.New("database is down")
	h.store.mu.Unlock()
	_, err = h.call(methodSubscribe, subscribeTo(repoArgs(), "https://hooks.example.test/db", secret))
	require.Error(t, err)
	seen = append(seen, "store failure: "+err.Error())
	h.store.mu.Lock()
	h.store.err = nil
	h.store.mu.Unlock()

	seen = append(seen, "logs: "+h.logs.String())
	for _, row := range h.rows() {
		record("row", row)
		seen = append(seen, "row secret_enc: "+string(row.SecretEnc))
	}
	text := strings.Join(seen, "\n")
	require.NotEmpty(t, h.logs.String(), "the service logged the subscribe")
	require.Zero(t, strings.Count(text, secret), "the secret, whole")
	require.Zero(t, strings.Count(text, body), "the secret without its prefix")
	for i := 0; i+12 <= len(body); i += 4 {
		require.Zero(t, strings.Count(text, body[i:i+12]), "a 12 character piece of the secret at %d", i)
	}
}

func TestSubscribeRefusesBadCallbackURLsAndWritesNothing(t *testing.T) {
	t.Parallel()

	bad := map[string]string{
		"empty":                "",
		"not a URL":            "not a url",
		"scheme relative":      "//hooks.example.test/x",
		"http":                 "http://hooks.example.test/x",
		"upper-case http":      "HTTP://hooks.example.test/x",
		"ftp":                  "ftp://hooks.example.test/x",
		"javascript":           "javascript:alert(1)",
		"user info":            "https://user:pw@hooks.example.test/x",
		"user only":            "https://user@hooks.example.test/x",
		"fragment":             "https://hooks.example.test/x#frag",
		"empty fragment":       "https://hooks.example.test/x#",
		"space":                "https://hooks.example.test/a b",
		"newline":              "https://hooks.example.test/a\nb",
		"backslash":            "https://hooks.example.test\\@evil.test/",
		"tab in host":          "https://hooks.example\t.test/",
		"too long":             "https://hooks.example.test/" + strings.Repeat("a", 2100),
		"no host":              "https:///x",
		"port zero":            "https://hooks.example.test:0/x",
		"port too large":       "https://hooks.example.test:99999/x",
		"non-ASCII host":       "https://h\u00f6oks.example.test/x",
		"percent-encoded host": "https://hooks%2eexample.test/x",
		"single label host":    "https://intranet/x",
		"decimal IPv4":         "https://2130706433/x",
		"hex IPv4":             "https://0x7f000001/x",
		"dotted hex IPv4":      "https://0x7f.0.0.1/x",
		"numeric TLD":          "https://example.123/x",
		"localhost":            "https://localhost/x",
		"localhost dot":        "https://localhost./x",
		"sub localhost":        "https://api.localhost/x",
		"loopback IPv4":        "https://127.0.0.1/x",
		"loopback IPv4 range":  "https://127.9.9.9:8443/x",
		"loopback IPv6":        "https://[::1]/x",
		"unspecified":          "https://0.0.0.0/x",
		"private 10":           "https://10.1.2.3/x",
		"private 172":          "https://172.16.0.1/x",
		"private 192":          "https://192.168.1.1/x",
		"CGNAT":                "https://100.64.0.1/x",
		"link-local metadata":  "https://169.254.169.254/latest/meta-data",
		"documentation":        "https://203.0.113.9/x",
		"multicast":            "https://224.0.0.1/x",
		"reserved":             "https://240.0.0.1/x",
		"IPv6 unique local":    "https://[fd00::1]/x",
		"IPv6 link-local":      "https://[fe80::1]/x",
		"IPv6 zone":            "https://[fe80::1%25eth0]/x",
		"IPv4-mapped loopback": "https://[::ffff:127.0.0.1]/x",
		"IPv4-mapped private":  "https://[::ffff:10.0.0.1]/x",
		"NAT64":                "https://[64:ff9b::7f00:1]/x",
		"6to4":                 "https://[2002:7f00:1::1]/x",
	}
	for name, url := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newWebhookHarness(t)
			rpcErr := h.subscribeError(subscribeTo(repoArgs(), url, newSecret(t, 32)))
			require.EqualValues(t, codeInvalidParams, rpcErr.Code)
			require.NotContains(t, rpcErr.Message, "pw", "no part of the URL is quoted")
			requireNothingStored(t, h, name)
			require.Zero(t, h.authz.callCount(), "a bad URL is refused before GitHub is asked")
			require.Zero(t, h.verifier.callCount())
		})
	}

	t.Run("unsubscribe checks the URL the same way", func(t *testing.T) {
		t.Parallel()

		h := newWebhookHarness(t)
		for _, url := range []string{"http://hooks.example.test/x", "https://127.0.0.1/x", "", "https://u:p@hooks.example.test/"} {
			rpcErr := h.unsubscribe(repoArgs(), url)
			require.NotNil(t, rpcErr)
			require.EqualValues(t, codeInvalidParams, rpcErr.Code, url)
		}
	})
}

func TestSubscribeAcceptsPublicAddressesAndSpellingsOfOneURL(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	secret := newSecret(t, 32)
	first := h.subscribe(subscribeTo(repoArgs(), "https://hooks.example.test/mcp/hook?token=abc", secret))
	for _, spelling := range []string{
		"HTTPS://HOOKS.Example.Test/mcp/hook?token=abc",
		"https://hooks.example.test:443/mcp/hook?token=abc",
		"https://hooks.example.test./mcp/hook?token=abc",
	} {
		require.Equal(t, first.ID, h.subscribe(subscribeTo(repoArgs(), spelling, secret)).ID, spelling)
	}
	require.Len(t, h.rows(), 1, "spellings of one URL are one subscription")
	require.Equal(t, "https://hooks.example.test/mcp/hook?token=abc", h.rows()[0].CallbackURL, "path and query are kept")

	for _, url := range []string{"https://8.8.8.8/x", "https://[2606:4700:4700::1111]/x", "https://hooks.example.test:8443/x", "https://a.b.example.test/x"} {
		h.subscribe(subscribeTo(repoArgs(), url, secret))
	}
	require.Len(t, h.rows(), 5)

	require.NotContains(t, h.logs.String(), "token=abc", "a query can hold a receiver token and is never logged")
	require.NotContains(t, h.logs.String(), "/mcp/hook", "the path is never logged either")
}

func TestSubscribeAcceptsHTTPOnLoopbackOnlyWhenTheHostAllowsIt(t *testing.T) {
	t.Parallel()

	allow := func(d *faucet.Deps) { d.Webhook.AllowInsecureLoopback = true }
	h := newWebhookHarness(t, allow)
	secret := newSecret(t, 32)
	for _, url := range []string{"http://127.0.0.1:8080/hook", "http://localhost:3000/hook", "http://[::1]:9/hook", "https://127.0.0.1:8443/hook"} {
		h.subscribe(subscribeTo(repoArgs(), url, secret))
	}
	require.Len(t, h.rows(), 4)

	for _, url := range []string{"http://hooks.example.test/hook", "http://10.0.0.1/hook", "http://169.254.169.254/", "ftp://127.0.0.1/hook", "https://10.0.0.1/hook"} {
		rpcErr := h.subscribeError(subscribeTo(repoArgs(), url, secret))
		require.EqualValues(t, codeInvalidParams, rpcErr.Code, "%s stays refused: the option opens loopback and nothing else", url)
	}
	require.Len(t, h.rows(), 4)
}

func TestSubscribeVerifiesTheCallbackOncePerPrincipalAndURLForTenMinutes(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	secret := newSecret(t, 32)
	subscribe := func(url string) { h.subscribe(subscribeTo(repoArgs(), url, secret)) }

	subscribe(callbackURL)
	require.Equal(t, 1, h.verifier.callCount())
	subscribe(callbackURL)
	h.subscribe(&rawParams{Name: "pr.closed", Arguments: raw(repoArgs()), Delivery: deliveryTo(callbackURL, secret)})
	h.subscribe(subscribeTo(repoArgs("pr", 7), callbackURL, secret))
	require.Equal(t, 1, h.verifier.callCount(), "refreshes and other arguments reuse the verification")

	h.clock.Advance(10*time.Minute - time.Second)
	subscribe(callbackURL)
	require.Equal(t, 1, h.verifier.callCount(), "the verification holds for ten minutes")
	h.clock.Advance(time.Second)
	subscribe(callbackURL)
	require.Equal(t, 2, h.verifier.callCount(), "and then it is asked again")

	subscribe("https://hooks.example.test/another")
	require.Equal(t, 3, h.verifier.callCount(), "another URL is verified on its own")
	h.as("hubot")
	subscribe(callbackURL)
	require.Equal(t, 4, h.verifier.callCount(), "one principal's verification never covers another's")
	require.Equal(t, []string{
		"octocat " + callbackURL, "octocat " + callbackURL, "octocat https://hooks.example.test/another", "hubot " + callbackURL,
	}, h.verifier.calls)
}

func TestSubscribeRefusesAnUnverifiedCallbackAndWritesNothing(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	secret := newSecret(t, 32)
	h.verifier.setErr(&faucet.CallbackError{Reason: faucet.CallbackChallengeFailed, Message: "not allowed here"})

	rpcErr := h.subscribeError(subscribeTo(repoArgs(), callbackURL, secret))
	require.EqualValues(t, codeCallbackEndpoint, rpcErr.Code)
	require.Equal(t, "not allowed here", rpcErr.Message)
	require.JSONEq(t, `{"reason":"challenge_failed"}`, string(rpcErr.Data))
	requireNothingStored(t, h, "a refused callback")

	h.verifier.setErr(nil)
	h.subscribe(subscribeTo(repoArgs(), callbackURL, secret))
	require.Equal(t, 2, h.verifier.callCount(), "a refusal is not cached, so the retry asks again")
	require.Len(t, h.rows(), 1)

	for _, reason := range []faucet.CallbackReason{
		faucet.CallbackConnectionRefused, faucet.CallbackTimeout, faucet.CallbackTLSError, faucet.CallbackHTTP4xx, faucet.CallbackHTTP5xx,
	} {
		h.verifier.setErr(&faucet.CallbackError{Reason: reason})
		rpcErr := h.subscribeError(subscribeTo(repoArgs(), "https://hooks.example.test/"+string(reason), secret))
		require.EqualValues(t, codeCallbackEndpoint, rpcErr.Code)
		require.JSONEq(t, fmt.Sprintf(`{"reason":%q}`, reason), string(rpcErr.Data))
	}
	require.Len(t, h.rows(), 1)

	for name, err := range map[string]error{
		"a verifier error":         xerrors.New("dial tcp 10.0.0.9: connection refused"),
		"a reason the SEP lacks":   &faucet.CallbackError{Reason: "the body said hi"},
		"an empty reason":          &faucet.CallbackError{},
		"a wrapped callback error": xerrors.Errorf("verify: %w", &faucet.CallbackError{Reason: faucet.CallbackTimeout}),
	} {
		h.verifier.setErr(err)
		rpcErr := h.subscribeError(subscribeTo(repoArgs(), "https://hooks.example.test/"+strings.ReplaceAll(name, " ", "-"), secret))
		if name == "a wrapped callback error" {
			require.EqualValues(t, codeCallbackEndpoint, rpcErr.Code, name)
			continue
		}
		require.EqualValues(t, codeInternal, rpcErr.Code, name)
		require.NotContains(t, rpcErr.Message, "10.0.0.9", "a verifier's error text is not shown to the subscriber")
		require.NotContains(t, rpcErr.Message, "the body said hi")
	}
	require.Len(t, h.rows(), 1)
}

func TestSubscribeIdentityAndUpsert(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	secretA, secretB := newSecret(t, 32), newSecret(t, 40)

	first := h.subscribe(subscribeTo(repoArgs("pr", 7), callbackURL, secretA))
	require.Regexp(t, `^sub_[0-9a-f]{16}$`, first.ID)
	require.Equal(t, first.ID, h.subscribe(&rawParams{
		Name: "ci.run_completed", Arguments: json.RawMessage(`{ "pr" : 7 , "repo" : "octo/repo" }`), Delivery: deliveryTo(callbackURL, secretA),
	}).ID, "key order and spaces in the arguments do not change the subscription")
	require.Len(t, h.rows(), 1)

	h.clock.Advance(3 * time.Minute)
	refreshed := h.subscribe(subscribeTo(repoArgs("pr", 7), callbackURL, secretB))
	require.Equal(t, first.ID, refreshed.ID, "the id is stable across refreshes")
	rows := h.rows()
	require.Len(t, rows, 1, "a refresh updates in place")
	plain, err := openSealed(t, testWebhookKey, rows[0])
	require.NoError(t, err)
	require.Equal(t, secretB, plain, "a refresh replaces the secret")
	require.Equal(t, h.clock.Now().Add(10*time.Minute).UTC().Format(time.RFC3339), refreshed.RefreshBefore, "and grants again")
	require.True(t, rows[0].CreatedAt.Before(rows[0].UpdatedAt))

	distinct := map[string]*rawParams{
		"callback URL":   subscribeTo(repoArgs("pr", 7), "https://hooks.example.test/second", secretA),
		"event name":     {Name: "pr.closed", Arguments: raw(repoArgs("pr", 7)), Delivery: deliveryTo(callbackURL, secretA)},
		"repository":     subscribeTo(args("repo", otherRepo, "pr", 7), callbackURL, secretA),
		"pull request":   subscribeTo(repoArgs("pr", 8), callbackURL, secretA),
		"sha":            subscribeTo(repoArgs("pr", 7, "sha", testShaA), callbackURL, secretA),
		"no pr filter":   subscribeTo(repoArgs(), callbackURL, secretA),
		"upper-case sha": subscribeTo(repoArgs("pr", 7, "sha", strings.ToUpper(testShaA)), callbackURL, secretA),
	}
	ids := map[string]string{first.ID: "the first"}
	for name, params := range distinct {
		id := h.subscribe(params).ID
		require.NotContains(t, ids, id, "%s gives a different subscription from %s", name, ids[id])
		ids[id] = name
	}

	h.as("hubot")
	hubot := h.subscribe(subscribeTo(repoArgs("pr", 7), callbackURL, secretA))
	require.NotContains(t, ids, hubot.ID, "another principal subscribing to the same thing gets its own subscription")
	require.Len(t, h.rows(), len(distinct)+2)
}

func TestSubscribeKeepsTheCursorOfALiveSubscriptionAndRestartsALapsedOne(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	secret := newSecret(t, 32)
	h.insert(repoID, "ci.run_completed", "")
	h.clock.Advance(time.Second)
	first := h.subscribe(subscribeTo(repoArgs(), callbackURL, secret))

	h.insert(repoID, "ci.run_completed", "")
	h.insert(repoID, "ci.run_completed", "")
	h.clock.Advance(time.Second)
	live := h.subscribe(subscribeTo(repoArgs(), callbackURL, secret))
	require.Equal(t, first.Cursor, live.Cursor, "a refresh of a live subscription keeps its position")
	require.False(t, live.Truncated)

	h.clock.Advance(11 * time.Minute)
	lapsed := h.subscribe(subscribeTo(repoArgs(), callbackURL, secret))
	require.Equal(t, "v1.3", lapsed.Cursor, "after the TTL passed it is a fresh subscription that starts at now")
	require.Equal(t, first.ID, lapsed.ID)
}

func TestSubscribeReadsTheSuppliedCursor(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	secret := newSecret(t, 32)
	for range 4 {
		h.insert(repoID, "ci.run_completed", "")
		h.clock.Advance(time.Minute)
	}

	at := func(cursor string, maxAge json.RawMessage, url string) subscribed {
		params := subscribeTo(repoArgs(), url, secret)
		params.Cursor, params.MaxAgeMs = raw(cursor), maxAge
		return h.subscribe(params)
	}
	got := at("v1.2", nil, "https://hooks.example.test/a")
	require.Equal(t, subscribed{ResultType: "complete", ID: got.ID, RefreshBefore: got.RefreshBefore, Cursor: "v1.2"}, got,
		"a cursor inside the stored events starts delivery after it")
	require.Equal(t, int64(2), h.rows()[0].Cursor)

	got = at("v1.1", json.RawMessage("150000"), "https://hooks.example.test/b")
	require.Equal(t, "v1.2", got.Cursor, "maxAgeMs moves the start up to the oldest event it allows")
	require.True(t, got.Truncated, "and says that events were skipped")

	got = at("v1.999", nil, "https://hooks.example.test/c")
	require.Equal(t, "v1.999", got.Cursor, "a cursor ahead of the head is kept, as poll keeps it")
	require.False(t, got.Truncated)

	got = at("v1.1", raw(int64(math.MaxInt64)), "https://hooks.example.test/huge")
	require.Equal(t, "v1.1", got.Cursor, "the largest maxAgeMs is a very old floor, not a wrapped one")
	require.False(t, got.Truncated)

	h.clock.Advance(time.Minute)
	_, err := h.store.Purge(h.ctx, h.clock.Now().Add(-2*time.Minute))
	require.NoError(t, err)
	got = at("v1.0", nil, "https://hooks.example.test/d")
	require.Equal(t, "v1.3", got.Cursor, "events through the purge mark are gone, so delivery starts above it")
	require.True(t, got.Truncated)

	for name, cursor := range map[string]string{"not ours": "page-2", "negative": "v1.-1", "padded": "v1.01", "empty": ""} {
		params := subscribeTo(repoArgs(), callbackURL, secret)
		params.Cursor = raw(cursor)
		require.EqualValues(t, codeInvalidParams, h.subscribeError(params).Code, name)
	}
	params := subscribeTo(repoArgs(), callbackURL, secret)
	for _, ms := range []int64{0, -1, -9223372036855, math.MinInt64} {
		params.MaxAgeMs = raw(ms)
		require.EqualValues(t, codeInvalidParams, h.subscribeError(params).Code, "maxAgeMs of %d", ms)
	}
}

func TestSubscribeLimitsEachPrincipal(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	secret := newSecret(t, 32)
	url := func(n int) string { return fmt.Sprintf("https://hooks.example.test/%d", n) }
	for n := range 100 {
		h.subscribe(subscribeTo(repoArgs(), url(n), secret))
	}
	rpcErr := h.subscribeError(subscribeTo(repoArgs(), url(100), secret))
	require.EqualValues(t, codeResourceExhausted, rpcErr.Code)
	require.JSONEq(t, `{"limit":"subscriptions","max":100}`, string(rpcErr.Data))
	require.Len(t, h.rows(), 100, "the refused subscription is not stored")

	h.subscribe(subscribeTo(repoArgs(), url(5), secret))
	h.as("hubot")
	h.subscribe(subscribeTo(repoArgs(), url(100), secret))
	require.Len(t, h.rows(), 101, "a refresh and another principal are not counted against the limit")

	h.as("octocat")
	h.clock.Advance(11 * time.Minute)
	h.subscribe(subscribeTo(repoArgs(), url(100), secret))
}

func TestUnsubscribeRemovesOnlyTheCallersSubscription(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	secret := newSecret(t, 32)
	h.subscribe(subscribeTo(repoArgs(), callbackURL, secret))
	h.as("hubot")
	h.subscribe(subscribeTo(repoArgs(), callbackURL, secret))
	require.Len(t, h.rows(), 2)

	mallory := func() *jsonrpc.Error {
		h.as("mallory")
		defer h.as("octocat")
		return h.unsubscribe(repoArgs(), callbackURL)
	}
	foreign := mallory()
	require.NotNil(t, foreign)
	require.EqualValues(t, codeNotFound, foreign.Code)
	require.JSONEq(t, `{"kind":"subscription"}`, string(foreign.Data))
	h.as("octocat")
	never := h.unsubscribe(repoArgs(), "https://hooks.example.test/never-subscribed")
	require.Equal(t, never, foreign, "a subscription of another principal and one that never existed give one answer")
	require.Len(t, h.rows(), 2, "neither removed anything")

	require.Nil(t, h.unsubscribe(repoArgs(), callbackURL))
	rows := h.rows()
	require.Len(t, rows, 1, "the caller's own subscription is gone")
	require.Equal(t, "hubot", rows[0].Principal, "and the other principal's is not")

	again := h.unsubscribe(repoArgs(), callbackURL)
	require.NotNil(t, again)
	require.EqualValues(t, codeNotFound, again.Code)

	h.as("hubot")
	h.subscribe(subscribeTo(repoArgs("pr", 3), callbackURL, secret))
	require.NotNil(t, h.unsubscribe(repoArgs("pr", 4), callbackURL), "arguments are part of the key")
	require.NotNil(t, h.unsubscribe(repoArgs(), callbackURL+"2"), "the URL is part of the key")
	require.Len(t, h.rows(), 2)
	require.Nil(t, h.unsubscribe(repoArgs("pr", 3), callbackURL))
	require.Len(t, h.rows(), 1)
}

func TestUnsubscribeDoesNotNeedRepositoryAccess(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	h.subscribe(subscribeTo(repoArgs(), callbackURL, newSecret(t, 32)))
	h.authz.mu.Lock()
	delete(h.authz.repos, testRepo)
	h.authz.mu.Unlock()
	calls := h.authz.callCount()

	require.Nil(t, h.unsubscribe(repoArgs(), callbackURL), "a caller who lost access can still clean up")
	require.Equal(t, calls, h.authz.callCount(), "unsubscribe asks GitHub nothing")
	requireNothingStored(t, h, "unsubscribe")
}

func TestSubscribeAndUnsubscribeNeedAKnownEventAndValidArguments(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	secret := newSecret(t, 32)
	for method, build := range map[string]func(name string, arguments json.RawMessage) *rawParams{
		methodSubscribe: func(name string, arguments json.RawMessage) *rawParams {
			return &rawParams{Name: name, Arguments: arguments, Delivery: deliveryTo(callbackURL, secret)}
		},
		methodUnsubscribe: func(name string, arguments json.RawMessage) *rawParams {
			return &rawParams{Name: name, Arguments: arguments, Delivery: raw(map[string]any{"url": callbackURL})}
		},
	} {
		unknown := h.callError(method, build("repo.exploded", raw(repoArgs())))
		require.EqualValues(t, codeNotFound, unknown.Code, method)
		require.JSONEq(t, `{"kind":"event"}`, string(unknown.Data))
		require.EqualValues(t, codeInvalidParams, h.callError(method, build("", raw(repoArgs()))).Code, method+" without a name")
		for name, arguments := range map[string]json.RawMessage{
			"missing":          nil,
			"no repo":          raw(args("pr", 3)),
			"bad repo":         raw(args("repo", "../../etc")),
			"extra property":   raw(args("repo", testRepo, "branch", "main")),
			"pr zero":          raw(args("repo", testRepo, "pr", 0)),
			"short sha":        raw(args("repo", testRepo, "sha", "abc")),
			"arguments a list": raw([]string{testRepo}),
		} {
			require.EqualValues(t, codeInvalidParams, h.callError(method, build("ci.run_completed", arguments)).Code, "%s %s", method, name)
		}
	}
	requireNothingStored(t, h, "invalid requests")
}

func TestSubscribeOnlyAcceptsTheWebhookMode(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	secret := newSecret(t, 32)
	for _, mode := range []string{"push", "poll", "carrier-pigeon", ""} {
		rpcErr := h.subscribeError(&rawParams{
			Name: "ci.run_completed", Arguments: raw(repoArgs()),
			Delivery: raw(map[string]any{"mode": mode, "url": callbackURL, "secret": secret}),
		})
		require.EqualValues(t, codeUnsupported, rpcErr.Code, mode)
		require.JSONEq(t, fmt.Sprintf(`{"feature":"deliveryMode","value":%q}`, mode), string(rpcErr.Data))
	}
	long := h.subscribeError(&rawParams{
		Name: "ci.run_completed", Arguments: raw(repoArgs()),
		Delivery: raw(map[string]any{"mode": strings.Repeat("x", 1000), "url": callbackURL, "secret": secret}),
	})
	require.EqualValues(t, codeInvalidParams, long.Code, "a mode name is not echoed at any length")
	requireNothingStored(t, h, "another mode")

	h.subscribe(&rawParams{Name: "ci.run_completed", Arguments: raw(repoArgs()), Delivery: raw(map[string]any{"url": callbackURL, "secret": secret})})
	require.Len(t, h.rows(), 1, "the mode may be left out, because events/subscribe is for webhooks")
}

func TestWebhookModeIsOfferedOnlyWhenTheHostConfiguredIt(t *testing.T) {
	t.Parallel()

	modes := func(h *eventsHarness) [][]string {
		res, err := h.call(methodList, nil)
		require.NoError(t, err)
		var list []struct {
			Delivery []string `json:"delivery"`
		}
		require.NoError(t, json.Unmarshal(res.fields["events"], &list))
		out := make([][]string, len(list))
		for i, e := range list {
			out[i] = e.Delivery
		}
		return out
	}

	on := newWebhookHarness(t)
	for _, delivery := range modes(on.eventsHarness) {
		require.Equal(t, []string{"poll", "webhook"}, delivery)
	}

	off := newEventsHarness(t)
	for _, delivery := range modes(off) {
		require.Equal(t, []string{"poll"}, delivery)
	}
	calls := off.authz.callCount()
	for method, params := range map[string]*rawParams{
		methodSubscribe:   subscribeTo(repoArgs(), callbackURL, newSecret(t, 32)),
		methodUnsubscribe: {Name: "ci.run_completed", Arguments: raw(repoArgs()), Delivery: raw(map[string]any{"url": callbackURL})},
	} {
		res, err := off.call(method, params)
		require.Error(t, err, "result: %v", res)
		var rpcErr *jsonrpc.Error
		require.ErrorAs(t, err, &rpcErr)
		require.EqualValues(t, codeUnsupported, rpcErr.Code, method)
		require.JSONEq(t, `{"feature":"deliveryMode","value":"webhook"}`, string(rpcErr.Data))
	}
	require.Equal(t, calls, off.authz.callCount(), "an unsupported call asks GitHub nothing")
	require.Empty(t, off.store.subscriptions())
}

func TestSubscribeReportsAStoreFailureWithoutDetail(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	h.store.mu.Lock()
	h.store.err = xerrors.New("pq: connection to 10.1.2.3 refused")
	h.store.mu.Unlock()

	rpcErr := h.subscribeError(subscribeTo(repoArgs(), callbackURL, newSecret(t, 32)))
	require.EqualValues(t, codeInternal, rpcErr.Code)
	require.NotContains(t, rpcErr.Message, "10.1.2.3")
	require.Contains(t, h.logs.String(), "10.1.2.3", "the detail goes to the log")
}

func TestPurgeDeletesExpiredSubscriptions(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	secret := newSecret(t, 32)
	soon := subscribeTo(repoArgs(), "https://hooks.example.test/soon", secret)
	soon.TTLMs = raw(60000)
	h.subscribe(soon)
	h.subscribe(subscribeTo(repoArgs(), "https://hooks.example.test/later", secret))

	h.clock.Advance(2 * time.Minute)
	_, err := h.svc.Purge(h.ctx, h.clock.Now())
	require.NoError(t, err)
	rows := h.rows()
	require.Len(t, rows, 1)
	require.Equal(t, "https://hooks.example.test/later", rows[0].CallbackURL)

	h.clock.Advance(10 * time.Minute)
	_, err = h.svc.Purge(h.ctx, h.clock.Now())
	require.NoError(t, err)
	requireNothingStored(t, h, "every subscription has expired")
}

func TestSubscribeAndUnsubscribeRefuseMistypedParamsWithoutQuotingTheSecret(t *testing.T) {
	t.Parallel()

	secret := newSecret(t, 32)
	body := strings.TrimPrefix(secret, "whsec_")
	delivery := fmt.Sprintf(`{"mode":"webhook","url":%q,"secret":%q}`, callbackURL, secret)
	withDelivery := func(field string) json.RawMessage {
		return json.RawMessage(`{"name":"ci.run_completed","arguments":{"repo":"octo/repo"},"delivery":` + delivery + `,` + field + `}`)
	}
	cases := map[string]json.RawMessage{
		"name a number":        json.RawMessage(`{"name":5,"arguments":{"repo":"octo/repo"},"delivery":` + delivery + `}`),
		"name an object":       json.RawMessage(`{"name":{"n":1},"arguments":{"repo":"octo/repo"},"delivery":` + delivery + `}`),
		"cursor a number":      withDelivery(`"cursor":12`),
		"cursor an object":     withDelivery(`"cursor":{"c":1}`),
		"maxAgeMs a string":    withDelivery(`"maxAgeMs":"1000"`),
		"maxAgeMs a fraction":  withDelivery(`"maxAgeMs":1.5`),
		"maxAgeMs a list":      withDelivery(`"maxAgeMs":[1]`),
		"arguments a number":   json.RawMessage(`{"name":"ci.run_completed","arguments":7,"delivery":` + delivery + `}`),
		"params a list":        json.RawMessage(`["` + secret + `"]`),
		"params a string":      json.RawMessage(`"` + secret + `"`),
		"params a number":      json.RawMessage(`12345`),
		"params with a secret": json.RawMessage(`{"secret":"` + secret + `","name":[1]}`),
	}
	for method, expected := range map[string][]int64{
		methodSubscribe:   {codeInvalidParams},
		methodUnsubscribe: {codeInvalidParams, codeNotFound},
	} {
		for name, whole := range cases {
			t.Run(method+" "+name, func(t *testing.T) {
				t.Parallel()

				h := newWebhookHarness(t)
				rpcErr := h.callError(method, &rawParams{Whole: whole})
				require.Contains(t, expected, rpcErr.Code, "unsubscribe has no cursor or maxAgeMs, so it ignores them")
				text := rpcErr.Message + string(rpcErr.Data)
				require.NotContains(t, text, secret, "the error quotes the secret")
				for i := 0; i+12 <= len(body); i += 4 {
					require.NotContains(t, text, body[i:i+12], "the error quotes a piece of the secret")
				}
				requireNothingStored(t, h, "mistyped params")
			})
		}
	}
}

func TestSubscribeReadsParamsOfEveryValidType(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	secret := newSecret(t, 32)
	delivery := fmt.Sprintf(`{"url":%q,"secret":%q}`, callbackURL, secret)
	ok := json.RawMessage(`{"name":"ci.run_completed","arguments":{"repo":"octo/repo"},"delivery":` + delivery + `,"cursor":null,"maxAgeMs":null,"ttlMs":120000,"_meta":{"x":1}}`)
	got := h.subscribe(&rawParams{Whole: ok})
	require.Equal(t, h.clock.Now().Add(2*time.Minute).UTC().Format(time.RFC3339), got.RefreshBefore)
	require.Len(t, h.rows(), 1)
}

func TestSubscribeGrantStaysFromTheFloorToTheCapForAnyInteger(t *testing.T) {
	t.Parallel()

	const wrapStep = 18446744073709
	rng := mrand.New(mrand.NewPCG(1, 2)) //nolint:gosec // A fixed seed gives the same sample every run, which is the point.
	values := []int64{math.MinInt64, math.MaxInt64, 0, 1, -1}
	for k := range 63 {
		values = append(values, 1<<k, -(1 << k), 1<<k+1, -(1<<k)-1)
	}
	for k := int64(-40); k <= 40; k++ {
		for d := int64(-2); d <= 2; d++ {
			values = append(values, k*wrapStep+d)
		}
	}
	for range 300 {
		values = append(values, int64(rng.Uint64())) //nolint:gosec // Any bit pattern is a wanted integer.
	}

	h := newWebhookHarness(t)
	secret := newSecret(t, 32)
	now := h.clock.Now()
	for _, ms := range values {
		params := subscribeTo(repoArgs(), callbackURL, secret)
		params.TTLMs = raw(ms)
		got := h.subscribe(params)
		grant, err := time.Parse(time.RFC3339, got.RefreshBefore)
		require.NoError(t, err)
		require.GreaterOrEqual(t, grant.Sub(now), 29*time.Second, "ttlMs %d", ms)
		require.LessOrEqual(t, grant.Sub(now), 10*time.Minute, "ttlMs %d", ms)
	}
}

func TestSubscribeChecksRepositoryAccessAgainOnEveryRenewal(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	h.subscribe(subscribeTo(repoArgs(), callbackURL, newSecret(t, 32)))
	granted := h.rows()

	h.authz.mu.Lock()
	delete(h.authz.repos, testRepo)
	h.authz.mu.Unlock()
	h.clock.Advance(2 * time.Minute)

	rpcErr := h.subscribeError(subscribeTo(repoArgs(), callbackURL, newSecret(t, 32)))
	require.EqualValues(t, codeForbidden, rpcErr.Code, "a renewal does not reuse the grant of the first subscribe")
	require.Equal(t, granted, h.rows(), "a refused renewal changes nothing: same expiry, same sealed secret")

	h.authz.mu.Lock()
	h.authz.repos[testRepo] = repoID
	h.authz.mu.Unlock()
	h.subscribe(subscribeTo(repoArgs(), callbackURL, newSecret(t, 32)))
	require.Greater(t, h.rows()[0].ExpiresAt, granted[0].ExpiresAt, "with access back, the renewal is granted again")
}
