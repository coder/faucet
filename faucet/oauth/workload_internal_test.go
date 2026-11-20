package oauth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"

	"github.com/coder/quartz"
	"github.com/coder/superblink-events/faucet"
)

const (
	testAudience         = "https://faucet.example.com"
	testVercelIssuer     = "https://oidc.vercel.com/codercom"
	testVercelDiscovery  = "oidc.vercel.com/codercom/.well-known/openid-configuration"
	testVercelKeys       = "oidc.vercel.com/codercom/.well-known/jwks"
	testKubernetesIssuer = "https://kubernetes.default.svc"
	testKID              = "test-key"
	// The IDs of the example token on vercel.com/docs/oidc/reference.
	testVercelTeam    = "team_7Gw5ZMzpQA8h90F832KGp7nwbuh3"
	testVercelProject = "prj_7Gw5ZMBpQA8h9GF832KGp7nwbuh3"
)

// The keys that sign the test tokens. The real keys of Vercel's documents
// sign nothing here.
var (
	testRSAKey   = sync.OnceValue(func() *rsa.PrivateKey { return must(rsa.GenerateKey(rand.Reader, 2048)) })
	otherRSAKey  = sync.OnceValue(func() *rsa.PrivateKey { return must(rsa.GenerateKey(rand.Reader, 2048)) })
	testECDSAKey = sync.OnceValue(func() *ecdsa.PrivateKey { return must(ecdsa.GenerateKey(elliptic.P256(), rand.Reader)) })
)

func must[V any](v V, err error) V {
	if err != nil {
		panic(err)
	}
	return v
}

func publicJWK(key crypto.Signer, kid string) jose.JSONWebKey {
	return jose.JSONWebKey{Key: key.Public(), KeyID: kid, Use: "sig"}
}

func testVercelRule() TrustRule {
	return TrustRule{Issuer: testVercelIssuer, Claims: map[string]string{
		"owner_id": testVercelTeam, "project_id": testVercelProject, "environment": "production",
	}}
}

// vercelClaims are the claims of a token of a production function, as
// vercel.com/docs/oidc/reference shows them, with faucet as the audience.
func vercelClaims(now time.Time) map[string]any {
	return map[string]any{
		"iss": testVercelIssuer, "aud": testAudience,
		"sub": "owner:codercom:project:digest-bot:environment:production",
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(2 * time.Hour).Unix(),
		"owner": "codercom", "owner_id": testVercelTeam,
		"project": "digest-bot", "project_id": testVercelProject,
		"environment": "production",
		// A token for a custom audience also carries these, as the claim names
		// of a real one showed on 2026-11-18. act holds the iat and the aud of
		// the token that Vercel exchanged.
		"deployment_id": "dpl_7Gw5ZMBpQA8h9GF832KGp7nwbuh3", "plan": "enterprise", "scope": "owner:codercom:project:digest-bot:environment:production",
		"act": map[string]any{"iat": now.Add(-time.Minute).Unix(), "aud": "https://vercel.com/codercom"},
	}
}

func testKubernetesRule(t *testing.T, keys ...jose.JSONWebKey) TrustRule {
	t.Helper()
	jwks, err := json.Marshal(jose.JSONWebKeySet{Keys: keys})
	require.NoError(t, err)
	return TrustRule{Issuer: testKubernetesIssuer, Claims: map[string]string{"sub": "system:serviceaccount:kube-system:coredns"}, JWKS: jwks}
}

// kubernetesClaims are the claims of a projected service account token, as
// kubernetes.io's "Configure Service Accounts for Pods" shows them, with
// faucet as the audience.
func kubernetesClaims(now time.Time) map[string]any {
	return map[string]any{
		"aud": []string{testAudience}, "exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "nbf": now.Unix(),
		"iss": testKubernetesIssuer, "jti": "ea28ed49-2e11-4280-9ec5-bc3d1d84661a",
		"kubernetes.io": map[string]any{
			"namespace":      "kube-system",
			"node":           map[string]any{"name": "127.0.0.1", "uid": "58456cb0-dd00-45ed-b797-5578fdceaced"},
			"pod":            map[string]any{"name": "coredns-69cbfb9798-jv9gn", "uid": "778a530c-b3f4-47c0-9cd5-ab018fb64f33"},
			"serviceaccount": map[string]any{"name": "coredns", "uid": "a087d5a0-e1dd-43ec-93ac-f13d89cd13af"},
			"warnafter":      now.Add(48 * time.Minute).Unix(),
		},
		"sub": "system:serviceaccount:kube-system:coredns",
	}
}

func signToken(t *testing.T, alg jose.SignatureAlgorithm, key crypto.Signer, kid string, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid))
	require.NoError(t, err)
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	jws, err := signer.Sign(payload)
	require.NoError(t, err)
	token, err := jws.CompactSerialize()
	require.NoError(t, err)
	return token
}

func signVercel(t *testing.T, claims map[string]any) string {
	t.Helper()
	return signToken(t, jose.RS256, testRSAKey(), testKID, claims)
}

// handMadeToken is a compact JWS whose signature sign gives, for algs that
// go-jose does not sign with.
func handMadeToken(t *testing.T, header, claims map[string]any, sign func(input string) []byte) string {
	t.Helper()
	part := func(v any) string {
		b, err := json.Marshal(v)
		require.NoError(t, err)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	input := part(header) + "." + part(claims)
	return input + "." + base64.RawURLEncoding.EncodeToString(sign(input))
}

// issuerServer serves documents over TLS at the hosts and paths of real
// issuers, such as oidc.vercel.com, and counts the requests for each.
type issuerServer struct {
	client *http.Client
	mu     sync.Mutex
	docs   map[string][]byte
	hits   map[string]int
	// hold, when set, keeps each answer until it closes.
	hold chan struct{}
}

func newIssuerServer(t *testing.T) *issuerServer {
	t.Helper()
	s := &issuerServer{docs: map[string][]byte{}, hits: map[string]int{}}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		at := r.Host + r.URL.Path
		s.hits[at]++
		body, ok := s.docs[at]
		hold := s.hold
		s.mu.Unlock()
		if hold != nil {
			<-hold
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	cert, roots := testCertificate(t, "oidc.vercel.com", "*.example.com")
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	transport := &http.Transport{
		// Every host is this server, whatever port the URL names.
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}
	t.Cleanup(transport.CloseIdleConnections)
	s.client = &http.Client{Transport: transport}
	return s
}

func testCertificate(t *testing.T, hosts ...string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key := must(ecdsa.GenerateKey(elliptic.P256(), rand.Reader))
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), DNSNames: hosts, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(must(x509.ParseCertificate(der)))
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots
}

func (s *issuerServer) serve(at string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs[at] = body
}

func (s *issuerServer) unserve(at string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.docs, at)
}

func (s *issuerServer) hitsOf(at string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[at]
}

// serveVercel serves Vercel's real discovery document and key set of the
// team codercom, fetched on 2026-11-18, with keys added to the set.
func (s *issuerServer) serveVercel(t *testing.T, keys ...jose.JSONWebKey) {
	t.Helper()
	discovery, err := os.ReadFile("testdata/vercel-codercom-discovery.json")
	require.NoError(t, err)
	s.serve(testVercelDiscovery, discovery)
	s.serveVercelKeys(t, keys...)
}

func (s *issuerServer) serveVercelKeys(t *testing.T, keys ...jose.JSONWebKey) {
	t.Helper()
	vercelKeys, err := os.ReadFile("testdata/vercel-codercom-jwks.json")
	require.NoError(t, err)
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(vercelKeys, &set))
	for _, k := range keys {
		set.Keys = append(set.Keys, must(k.MarshalJSON()))
	}
	s.serve(testVercelKeys, must(json.Marshal(set)))
}

func checkAt(t *testing.T, c *workloadChecker, now time.Time, token string, rules ...TrustRule) (workloadMatch, error) {
	t.Helper()
	return c.check(context.Background(), rules, token, testAudience, now)
}

func requireTokenRefused(t *testing.T, err error, why string, msgAndArgs ...any) {
	t.Helper()
	require.ErrorIs(t, err, errSubjectToken, msgAndArgs...)
	require.ErrorContains(t, err, why, msgAndArgs...)
}

func TestATokenOfAVercelTeamMatchesThroughVercelsRealDocuments(t *testing.T) {
	t.Parallel()
	s := newIssuerServer(t)
	s.serveVercel(t, publicJWK(testRSAKey(), testKID))
	clock := quartz.NewMock(t)
	c := newWorkloadChecker(s.client, clock)
	now := clock.Now()

	m, err := checkAt(t, c, now, signVercel(t, vercelClaims(now)), testVercelRule())
	require.NoError(t, err)
	require.Equal(t, testVercelRule(), m.Rule)
	require.Equal(t, now.Add(2*time.Hour).Unix(), m.Expires.Unix())
	require.Equal(t, accessTokenLifetime, m.lifetime(now))
	require.Equal(t, 1, s.hitsOf(testVercelDiscovery))
	require.Equal(t, 1, s.hitsOf(testVercelKeys))

	keys, err := c.keys.keySet(context.Background(), testVercelIssuer, testKID)
	require.NoError(t, err)
	for _, kid := range []string{"mrk-4302ec1b670f48a98ad61dade4a23be7", "c0fdbe68-8811-47a0-8cd4-6b7a2ffbc905"} {
		found := keys.Key(kid)
		require.Len(t, found, 1, "Vercel's real key %s", kid)
		require.Equal(t, jose.RS256, keyAlgorithm(found[0]))
	}
}

// Vercel signs the tokens of every team with the same keys, so the tokens of
// another team verify. Only the issuer and the team ID tell them apart.
func TestATokenOfAnotherVercelTeamMatchesNoRule(t *testing.T) {
	t.Parallel()
	s := newIssuerServer(t)
	s.serveVercel(t, publicJWK(testRSAKey(), testKID))
	discovery, err := os.ReadFile("testdata/vercel-codercom-discovery.json")
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(discovery, &doc))
	doc["issuer"], doc["jwks_uri"] = "https://oidc.vercel.com/acme", "https://oidc.vercel.com/codercom/.well-known/jwks"
	s.serve("oidc.vercel.com/acme/.well-known/openid-configuration", must(json.Marshal(doc)))
	clock := quartz.NewMock(t)
	c := newWorkloadChecker(s.client, clock)
	now := clock.Now()

	for name, change := range map[string]func(map[string]any){
		"another team's issuer": func(cl map[string]any) {
			cl["iss"], cl["owner"], cl["owner_id"] = "https://oidc.vercel.com/acme", "acme", "team_acme"
		},
		"another team's issuer with this team's claims": func(cl map[string]any) { cl["iss"] = "https://oidc.vercel.com/acme" },
		"the global issuer":                             func(cl map[string]any) { cl["iss"] = "https://oidc.vercel.com" },
		"another team ID":                               func(cl map[string]any) { cl["owner_id"] = "team_acme" },
		"another project":                               func(cl map[string]any) { cl["project_id"] = "prj_other" },
		"a preview":                                     func(cl map[string]any) { cl["environment"] = "preview" },
		"a renamed team that took the slug": func(cl map[string]any) {
			cl["owner_id"], cl["sub"] = "team_acme", "owner:codercom:project:digest-bot:environment:production"
		},
	} {
		claims := vercelClaims(now)
		change(claims)
		_, err := checkAt(t, c, now, signVercel(t, claims), testVercelRule())
		requireTokenRefused(t, err, "it matches no trust rule of the client", name)
		for _, pinned := range testVercelRule().Claims {
			require.NotContains(t, err.Error(), pinned, "%s: a refusal names no value that the rule pins", name)
		}
		require.Zero(t, s.hitsOf("oidc.vercel.com/acme/.well-known/openid-configuration"), name)
	}
}

func TestOnlyRS256AndES256SignaturesPass(t *testing.T) {
	t.Parallel()
	clock := quartz.NewMock(t)
	now := clock.Now()
	rule := testKubernetesRule(t, publicJWK(testRSAKey(), testKID), publicJWK(testECDSAKey(), "ec"))
	c := newWorkloadChecker(&http.Client{}, clock)

	_, err := checkAt(t, c, now, signToken(t, jose.RS256, testRSAKey(), testKID, kubernetesClaims(now)), rule)
	require.NoError(t, err)
	_, err = checkAt(t, c, now, signToken(t, jose.ES256, testECDSAKey(), "ec", kubernetesClaims(now)), rule)
	require.NoError(t, err)

	publicDER := must(x509.MarshalPKIXPublicKey(testRSAKey().Public()))
	for name, token := range map[string]string{
		"none": handMadeToken(t, map[string]any{"alg": "none", "kid": testKID}, kubernetesClaims(now), func(string) []byte { return nil }),
		"HS256 keyed with the public key": handMadeToken(t, map[string]any{"alg": "HS256", "kid": testKID}, kubernetesClaims(now), func(input string) []byte {
			mac := hmac.New(sha256.New, publicDER)
			_, _ = mac.Write([]byte(input))
			return mac.Sum(nil)
		}),
		"RS384": signToken(t, jose.RS384, testRSAKey(), testKID, kubernetesClaims(now)),
		"PS256": signToken(t, jose.PS256, testRSAKey(), testKID, kubernetesClaims(now)),
		"the JSON serialization": func() string {
			signer := must(jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: testRSAKey()}, (&jose.SignerOptions{}).WithHeader("kid", testKID)))
			return must(signer.Sign(must(json.Marshal(kubernetesClaims(now))))).FullSerialize()
		}(),
	} {
		_, err := checkAt(t, c, now, token, rule)
		requireTokenRefused(t, err, "it must be a JWT in compact form, signed with RS256 or ES256", name)
	}

	_, err = checkAt(t, c, now, signToken(t, jose.RS256, testRSAKey(), "ec", kubernetesClaims(now)), rule)
	requireTokenRefused(t, err, "its kid and alg name no key of its issuer")
	_, err = checkAt(t, c, now, signToken(t, jose.RS256, otherRSAKey(), testKID, kubernetesClaims(now)), rule)
	requireTokenRefused(t, err, "its signature does not verify with its issuer's key")
	_, err = checkAt(t, c, now, signToken(t, jose.RS256, testRSAKey(), "", kubernetesClaims(now)), rule)
	requireTokenRefused(t, err, "its header must name its key in kid")
}

func TestTheAudienceMustBeFaucetAlone(t *testing.T) {
	t.Parallel()
	clock := quartz.NewMock(t)
	now := clock.Now()
	rule := testKubernetesRule(t, publicJWK(testRSAKey(), testKID))
	c := newWorkloadChecker(&http.Client{}, clock)
	for aud, ok := range map[string]bool{
		`"https://faucet.example.com"`:                               true,
		`["https://faucet.example.com"]`:                             true,
		`["https://faucet.example.com","https://vault.example.com"]`: false,
		`["https://vault.example.com","https://faucet.example.com"]`: false,
		`"https://faucet.example.com/oauth/token"`:                   false,
		`"https://faucet.example.com/"`:                              false,
		`"https://vercel.com/codercom"`:                              false,
		`[]`:                                                         false,
	} {
		claims := kubernetesClaims(now)
		claims["aud"] = json.RawMessage(aud)
		_, err := checkAt(t, c, now, signToken(t, jose.RS256, testRSAKey(), testKID, claims), rule)
		if ok {
			require.NoError(t, err, aud)
			continue
		}
		requireTokenRefused(t, err, "its aud must be https://faucet.example.com alone", aud)
	}
	claims := kubernetesClaims(now)
	delete(claims, "aud")
	_, err := checkAt(t, c, now, signToken(t, jose.RS256, testRSAKey(), testKID, claims), rule)
	requireTokenRefused(t, err, "its aud must be https://faucet.example.com alone")
}

func TestTheTokenMustBeValidNow(t *testing.T) {
	t.Parallel()
	clock := quartz.NewMock(t)
	now := clock.Now()
	rule := testKubernetesRule(t, publicJWK(testRSAKey(), testKID))
	c := newWorkloadChecker(&http.Client{}, clock)
	at := func(d time.Duration) int64 { return now.Add(d).Unix() }
	for name, tc := range map[string]struct {
		claims map[string]any
		want   string
	}{
		"expired a second ago":            {map[string]any{"exp": at(-time.Second)}, "it has expired"},
		"expiring now":                    {map[string]any{"exp": at(0)}, "it has expired"},
		"no exp":                          {map[string]any{"exp": nil}, "it must have an exp"},
		"an EXP but no exp":               {map[string]any{"exp": nil, "EXP": at(time.Hour)}, "it must have an exp"},
		"a string exp":                    {map[string]any{"exp": fmt.Sprint(at(time.Hour))}, "the registered claims of RFC 7519 in their types"},
		"nbf past the leeway":             {map[string]any{"nbf": at(workloadClockLeeway + time.Second)}, "its nbf is in the future"},
		"iat past the leeway":             {map[string]any{"iat": at(workloadClockLeeway + time.Second)}, "its iat is in the future"},
		"nbf and iat within the leeway":   {map[string]any{"nbf": at(workloadClockLeeway), "iat": at(workloadClockLeeway)}, ""},
		"no nbf or iat":                   {map[string]any{"nbf": nil, "iat": nil}, "it must have an iat or an nbf"},
		"an iat alone":                    {map[string]any{"nbf": nil}, ""},
		"an nbf alone":                    {map[string]any{"iat": nil}, ""},
		"a life of 24 hours":              {map[string]any{"iat": at(-time.Hour), "nbf": at(-time.Hour), "exp": at(23 * time.Hour)}, ""},
		"a life past 24 hours":            {map[string]any{"iat": at(-time.Hour), "nbf": at(-time.Hour), "exp": at(23*time.Hour + time.Second)}, "it must expire at most 24 hours after its iat and its nbf"},
		"a life past 24 hours from nbf":   {map[string]any{"iat": at(0), "nbf": at(-time.Hour), "exp": at(23*time.Hour + time.Second)}, "it must expire at most 24 hours after its iat and its nbf"},
		"a life past 24 hours from iat":   {map[string]any{"iat": at(-time.Hour), "nbf": nil, "exp": at(23*time.Hour + time.Second)}, "it must expire at most 24 hours after its iat and its nbf"},
		"expiring in a second":            {map[string]any{"exp": at(time.Second)}, ""},
		"a fractional exp past the start": {map[string]any{"exp": float64(at(time.Second)) + 0.5}, ""},
	} {
		claims := kubernetesClaims(now)
		for k, v := range tc.claims {
			if v == nil {
				delete(claims, k)
				continue
			}
			claims[k] = v
		}
		m, err := checkAt(t, c, now, signToken(t, jose.RS256, testRSAKey(), testKID, claims), rule)
		if tc.want == "" {
			require.NoError(t, err, name)
			require.LessOrEqual(t, m.lifetime(now), accessTokenLifetime, name)
			continue
		}
		requireTokenRefused(t, err, tc.want, name)
	}
}

func TestATokenWhoseActNamesAnotherPartyIsRefused(t *testing.T) {
	t.Parallel()
	clock := quartz.NewMock(t)
	now := clock.Now()
	rule := testKubernetesRule(t, publicJWK(testRSAKey(), testKID))
	c := newWorkloadChecker(&http.Client{}, clock)
	for _, act := range []any{
		map[string]any{"sub": "system:serviceaccount:apps:other-bot"},
		map[string]any{"iat": now.Unix(), "iss": testKubernetesIssuer},
		map[string]any{"aud": "x", "act": map[string]any{"sub": "y"}},
		nil, "x", []any{},
	} {
		claims := kubernetesClaims(now)
		claims["act"] = act
		_, err := checkAt(t, c, now, signToken(t, jose.RS256, testRSAKey(), testKID, claims), rule)
		requireTokenRefused(t, err, "its act claim names another party, the delegation of RFC 8693", act)
	}
	for _, act := range []any{map[string]any{}, map[string]any{"iat": now.Unix(), "aud": "https://vercel.com/codercom"}, map[string]any{"exp": now.Unix(), "nbf": now.Unix()}} {
		claims := kubernetesClaims(now)
		claims["act"] = act
		_, err := checkAt(t, c, now, signToken(t, jose.RS256, testRSAKey(), testKID, claims), rule)
		require.NoError(t, err, "an act of provenance alone names no other party: %v", act)
	}
	claims := kubernetesClaims(now)
	claims["may_act"] = map[string]any{"sub": "system:serviceaccount:apps:other-bot"}
	_, err := checkAt(t, c, now, signToken(t, jose.RS256, testRSAKey(), testKID, claims), rule)
	require.NoError(t, err, "may_act names who may act for the workload, and acts for no one")
}

func TestTheFaucetTokenEndsNoLaterThanTheSubjectToken(t *testing.T) {
	t.Parallel()
	clock := quartz.NewMock(t)
	now := clock.Now()
	rule := testKubernetesRule(t, publicJWK(testRSAKey(), testKID))
	c := newWorkloadChecker(&http.Client{}, clock)
	for exp, lifetime := range map[time.Duration]time.Duration{
		12 * time.Hour: accessTokenLifetime, accessTokenLifetime: accessTokenLifetime, 10 * time.Minute: 10 * time.Minute, time.Second: time.Second,
	} {
		claims := kubernetesClaims(now)
		claims["exp"] = now.Add(exp).Unix()
		m, err := checkAt(t, c, now, signToken(t, jose.RS256, testRSAKey(), testKID, claims), rule)
		require.NoError(t, err)
		require.Equal(t, lifetime, m.lifetime(now), "a subject token for %s", exp)
	}
}

// Faucet answers a token's lifetime in whole seconds, and a client of
// golang.org/x/oauth2 takes expires_in 0 for a token that never expires.
// exp is in whole seconds, and faucet's clock is not.
func TestAFaucetTokenLastsAtLeastASecond(t *testing.T) {
	t.Parallel()
	clock := quartz.NewMock(t)
	clock.Advance(600 * time.Millisecond)
	now := clock.Now()
	nextSecond := now.Truncate(time.Second).Add(time.Second)
	rule := testKubernetesRule(t, publicJWK(testRSAKey(), testKID))
	c := newWorkloadChecker(&http.Client{}, clock)

	claims := kubernetesClaims(now)
	claims["exp"] = nextSecond.Unix()
	_, err := checkAt(t, c, now, signToken(t, jose.RS256, testRSAKey(), testKID, claims), rule)
	requireTokenRefused(t, err, "it has expired", "400ms before its exp")

	claims["exp"] = nextSecond.Add(time.Second).Unix()
	m, err := checkAt(t, c, now, signToken(t, jose.RS256, testRSAKey(), testKID, claims), rule)
	require.NoError(t, err)
	require.Equal(t, 1400*time.Millisecond, m.lifetime(now))
}

func TestAClaimMatchesOnlyTheSameString(t *testing.T) {
	t.Parallel()
	s := newIssuerServer(t)
	s.serveVercel(t, publicJWK(testRSAKey(), testKID))
	clock := quartz.NewMock(t)
	now := clock.Now()
	c := newWorkloadChecker(s.client, clock)
	rule := testVercelRule()
	rule.Claims["project_id"] = "123"
	for value, ok := range map[string]bool{
		`"123"`: true, `123`: false, `123.0`: false, `["123"]`: false, `{"id":"123"}`: false, `null`: false, `"0123"`: false, `"123 "`: false,
	} {
		claims := vercelClaims(now)
		claims["project_id"] = json.RawMessage(value)
		_, err := checkAt(t, c, now, signVercel(t, claims), rule)
		if ok {
			require.NoError(t, err, value)
			continue
		}
		requireTokenRefused(t, err, "it matches no trust rule of the client", value)
	}
	claims := vercelClaims(now)
	delete(claims, "project_id")
	_, err := checkAt(t, c, now, signVercel(t, claims), rule)
	requireTokenRefused(t, err, "it matches no trust rule of the client")

	claims = vercelClaims(now)
	claims["environment"], claims["Environment"] = "preview", "production"
	_, err = checkAt(t, c, now, signVercel(t, claims), rule)
	requireTokenRefused(t, err, "it matches no trust rule of the client")
}

func TestAnUnknownKidCostsOneFetchAMinute(t *testing.T) {
	t.Parallel()
	s := newIssuerServer(t)
	s.serveVercel(t, publicJWK(testRSAKey(), testKID))
	clock := quartz.NewMock(t)
	c := newWorkloadChecker(s.client, clock)
	now := clock.Now()
	_, err := checkAt(t, c, now, signVercel(t, vercelClaims(now)), testVercelRule())
	require.NoError(t, err)

	s.serveVercelKeys(t, publicJWK(testRSAKey(), testKID), publicJWK(otherRSAKey(), "rotated"))
	clock.Advance(20 * time.Second)
	for i := range 20 {
		token := signToken(t, jose.RS256, testRSAKey(), fmt.Sprintf("made-up-%d", i), vercelClaims(now))
		_, err := checkAt(t, c, clock.Now(), token, testVercelRule())
		requireTokenRefused(t, err, "its kid and alg name no key of its issuer", "a made-up kid within a minute of the fetch")
	}
	rotated := signToken(t, jose.RS256, otherRSAKey(), "rotated", vercelClaims(now))
	_, err = checkAt(t, c, clock.Now(), rotated, testVercelRule())
	requireTokenRefused(t, err, "its kid and alg name no key of its issuer", "a key added within a minute of the fetch")
	require.Equal(t, 1, s.hitsOf(testVercelDiscovery))
	require.Equal(t, 1, s.hitsOf(testVercelKeys))

	clock.Advance(keyFetchInterval - 20*time.Second)
	now = clock.Now()
	_, err = checkAt(t, c, now, signToken(t, jose.RS256, otherRSAKey(), "rotated", vercelClaims(now)), testVercelRule())
	require.NoError(t, err, "the added key works a minute after the last fetch")
	_, err = checkAt(t, c, now, signToken(t, jose.RS256, testRSAKey(), "made-up", vercelClaims(now)), testVercelRule())
	requireTokenRefused(t, err, "its kid and alg name no key of its issuer")
	require.Equal(t, 2, s.hitsOf(testVercelKeys))
}

func TestTheKeysOfAnIssuerAreKeptForTheirLifetime(t *testing.T) {
	t.Parallel()
	s := newIssuerServer(t)
	s.serveVercel(t, publicJWK(testRSAKey(), testKID))
	clock := quartz.NewMock(t)
	c := newWorkloadChecker(s.client, clock)
	check := func() error {
		now := clock.Now()
		_, err := checkAt(t, c, now, signVercel(t, vercelClaims(now)), testVercelRule())
		return err
	}
	require.NoError(t, check())
	s.serveVercelKeys(t)
	clock.Advance(keySetLifetime - time.Second)
	require.NoError(t, check())
	require.Equal(t, 1, s.hitsOf(testVercelKeys))

	clock.Advance(time.Second)
	requireTokenRefused(t, check(), "its kid and alg name no key of its issuer")
	require.Equal(t, 2, s.hitsOf(testVercelKeys))
}

func TestAFailedFetchIsKeptForAMinute(t *testing.T) {
	t.Parallel()
	s := newIssuerServer(t)
	clock := quartz.NewMock(t)
	c := newWorkloadChecker(s.client, clock)
	check := func() error {
		now := clock.Now()
		_, err := checkAt(t, c, now, signVercel(t, vercelClaims(now)), testVercelRule())
		return err
	}
	for range 3 {
		err := check()
		require.ErrorIs(t, err, errIssuerKeys)
		require.ErrorContains(t, err, "the discovery document: it must be served with status 200 and no redirect, not 404")
		require.NotErrorIs(t, err, errSubjectToken)
	}
	require.Equal(t, 1, s.hitsOf(testVercelDiscovery))

	s.serveVercel(t, publicJWK(testRSAKey(), testKID))
	clock.Advance(keyFetchInterval)
	require.NoError(t, check())
	require.Equal(t, 2, s.hitsOf(testVercelDiscovery))
}

// testdata/k8s-* hold a token that a k3s v1.33.5 cluster minted for the
// service account apps/digest-bot with faucet's audience, and the cluster's
// discovery document and key set. The cluster's key no longer exists.
const (
	realKubernetesIssuer = "https://k8s.wi-test.invalid"
	realKubernetesIAT    = 1795005407
	realKubernetesEXP    = 1795009007
)

// The cluster is private: its discovery document names a jwks_uri that
// faucet's guarded client refuses, so the rule holds the cluster's keys.
func TestARealKubernetesTokenMatchesARuleWithTheClustersKeys(t *testing.T) {
	t.Parallel()
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	require.NoError(t, json.Unmarshal(must(os.ReadFile("testdata/k8s-discovery.json")), &doc))
	require.Equal(t, realKubernetesIssuer, doc.Issuer)
	_, err := newKeySource(faucet.NewHTTPClient(faucet.HTTPClientOptions{}), quartz.NewMock(t)).get(context.Background(), doc.JWKSURI)
	require.ErrorIs(t, err, faucet.ErrDestinationRefused, doc.JWKSURI)

	token := strings.TrimSpace(string(must(os.ReadFile("testdata/k8s-token.jwt"))))
	rule := TrustRule{
		Issuer: realKubernetesIssuer, Claims: map[string]string{"sub": "system:serviceaccount:apps:digest-bot"},
		JWKS: must(os.ReadFile("testdata/k8s-jwks.json")),
	}
	clock := quartz.NewMock(t)
	clock.Set(time.Unix(realKubernetesIAT, 0).Add(30 * time.Minute))
	c := newWorkloadChecker(&http.Client{}, clock)
	now := clock.Now()

	m, err := checkAt(t, c, now, token, rule)
	require.NoError(t, err)
	require.Equal(t, rule, m.Rule)
	require.Equal(t, int64(realKubernetesEXP), m.Expires.Unix())
	require.Equal(t, accessTokenLifetime, m.lifetime(now))

	otherAccount := rule
	otherAccount.Claims = map[string]string{"sub": "system:serviceaccount:apps:other-bot"}
	_, err = checkAt(t, c, now, token, otherAccount)
	requireTokenRefused(t, err, "it matches no trust rule of the client")
	_, err = c.check(context.Background(), []TrustRule{rule}, token, "https://vault.example.com", now)
	requireTokenRefused(t, err, "its aud must be https://vault.example.com alone")
	_, err = checkAt(t, c, time.Unix(realKubernetesEXP, 0), token, rule)
	requireTokenRefused(t, err, "it has expired")
}

// A token with a made-up kid makes faucet fetch again once a minute, and a
// fetch that fails then keeps the keys that faucet holds until their own
// lifetime ends.
func TestAFailedFetchKeepsTheKeysThatFaucetHolds(t *testing.T) {
	t.Parallel()
	s := newIssuerServer(t)
	s.serveVercel(t, publicJWK(testRSAKey(), testKID))
	clock := quartz.NewMock(t)
	c := newWorkloadChecker(s.client, clock)
	check := func(kid string) error {
		now := clock.Now()
		_, err := checkAt(t, c, now, signToken(t, jose.RS256, testRSAKey(), kid, vercelClaims(now)), testVercelRule())
		return err
	}
	require.NoError(t, check(testKID))
	s.unserve(testVercelKeys)
	clock.Advance(keyFetchInterval)
	for range 3 {
		err := check("made-up")
		require.ErrorIs(t, err, errIssuerKeys, "a kid that faucet lacks while its issuer fails")
		require.NotErrorIs(t, err, errSubjectToken)
		require.NoError(t, check(testKID), "a kid of the keys that faucet holds")
	}
	require.Equal(t, 2, s.hitsOf(testVercelKeys))

	clock.Advance(keySetLifetime - keyFetchInterval - time.Second)
	require.NoError(t, check(testKID))
	require.Equal(t, 2, s.hitsOf(testVercelKeys))
	clock.Advance(time.Second)
	require.ErrorIs(t, check(testKID), errIssuerKeys, "the keys last their lifetime from the fetch that got them")
	require.Equal(t, 3, s.hitsOf(testVercelKeys))
}

func TestAKeySourceStartsNoFetchOnceTheContextHasEnded(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	offline := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, net.ErrClosed
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := newKeySource(offline, quartz.NewMock(t)).keySet(ctx, testVercelIssuer, testKID)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, requests.Load())
}

func TestADiscoveryDocumentOfAnotherIssuerIsNotTrusted(t *testing.T) {
	t.Parallel()
	discovery, err := os.ReadFile("testdata/vercel-codercom-discovery.json")
	require.NoError(t, err)
	for _, issuer := range []string{"https://oidc.vercel.com/acme", "https://oidc.vercel.com/codercom/", "https://oidc.vercel.com", "https://OIDC.vercel.com/codercom", ""} {
		s := newIssuerServer(t)
		s.serveVercel(t, publicJWK(testRSAKey(), testKID))
		var doc map[string]any
		require.NoError(t, json.Unmarshal(discovery, &doc))
		doc["issuer"] = issuer
		s.serve(testVercelDiscovery, must(json.Marshal(doc)))
		clock := quartz.NewMock(t)
		now := clock.Now()
		_, err := checkAt(t, newWorkloadChecker(s.client, clock), now, signVercel(t, vercelClaims(now)), testVercelRule())
		require.ErrorIs(t, err, errIssuerKeys, issuer)
		require.ErrorContains(t, err, "the discovery document names another issuer", issuer)
		require.Zero(t, s.hitsOf(testVercelKeys), issuer)
	}
}

// A rule with pasted keys never reaches the network, and neither does a
// token whose iss no rule names.
func TestAPastedKeySetIsNeverFetched(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	offline := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, net.ErrClosed
	})}
	clock := quartz.NewMock(t)
	now := clock.Now()
	c := newWorkloadChecker(offline, clock)
	rule := testKubernetesRule(t, publicJWK(testECDSAKey(), "ec"))

	m, err := checkAt(t, c, now, signToken(t, jose.ES256, testECDSAKey(), "ec", kubernetesClaims(now)), rule)
	require.NoError(t, err)
	require.Equal(t, rule, m.Rule)
	_, err = checkAt(t, c, now, signToken(t, jose.ES256, testECDSAKey(), "made-up", kubernetesClaims(now)), rule)
	requireTokenRefused(t, err, "its kid and alg name no key of its issuer")

	vercel := testVercelRule()
	vercel.JWKS = must(json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{publicJWK(testRSAKey(), testKID)}}))
	_, err = checkAt(t, c, now, signVercel(t, vercelClaims(now)), vercel)
	require.NoError(t, err, "a Vercel rule with pasted keys")

	claims := kubernetesClaims(now)
	claims["iss"] = "https://issuer.example.com"
	_, err = checkAt(t, c, now, signToken(t, jose.RS256, testRSAKey(), testKID, claims), testVercelRule(), rule)
	requireTokenRefused(t, err, "it matches no trust rule of the client")
	require.Zero(t, requests.Load())
}

func TestTheFirstRuleThatMatchesWins(t *testing.T) {
	t.Parallel()
	clock := quartz.NewMock(t)
	now := clock.Now()
	c := newWorkloadChecker(&http.Client{}, clock)
	key := publicJWK(testECDSAKey(), "ec")
	other := testKubernetesRule(t, key)
	other.Claims = map[string]string{"sub": "system:serviceaccount:default:default"}
	first := testKubernetesRule(t, key)
	second := testKubernetesRule(t, key, publicJWK(testRSAKey(), testKID))
	token := signToken(t, jose.ES256, testECDSAKey(), "ec", kubernetesClaims(now))
	for range 10 {
		m, err := checkAt(t, c, now, token, other, first, second)
		require.NoError(t, err)
		require.Equal(t, first, m.Rule)
		m, err = checkAt(t, c, now, token, other, second, first)
		require.NoError(t, err)
		require.Equal(t, second, m.Rule)
	}
}

func TestAMatchNeedsTheVerifiedIssToBeTheRulesIssuer(t *testing.T) {
	t.Parallel()
	clock := quartz.NewMock(t)
	now := clock.Now()
	c := newWorkloadChecker(&http.Client{}, clock)
	rule, err := parseTrustRule(testKubernetesRule(t, publicJWK(testRSAKey(), testKID)))
	require.NoError(t, err)
	for iss, ok := range map[any]bool{testKubernetesIssuer: true, testKubernetesIssuer + "/": false, "https://issuer.example.com": false, nil: false} {
		claims := kubernetesClaims(now)
		claims["iss"] = iss
		jws := must(jose.ParseSignedCompact(signToken(t, jose.RS256, testRSAKey(), testKID, claims), workloadAlgorithms))
		_, err := c.match(context.Background(), rule, jws, jws.Signatures[0].Header, testAudience, now)
		if ok {
			require.NoError(t, err, iss)
			continue
		}
		requireTokenRefused(t, err, "its iss must be the rule's issuer", iss)
	}
}

// Of the rules that a token fails, check answers for the one that tells the
// most: keys that faucet could not fetch, as the token may match once they
// come, then a refusal after the token's signature verified.
func TestCheckAnswersTheFailureOfTheRuleThatTellsTheMost(t *testing.T) {
	t.Parallel()
	s := newIssuerServer(t)
	clock := quartz.NewMock(t)
	now := clock.Now()
	c := newWorkloadChecker(s.client, clock)
	otherProject := testVercelRule()
	otherProject.Claims["project_id"] = "prj_other"
	otherProject.JWKS = must(json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{publicJWK(testRSAKey(), testKID)}}))
	vercel := signVercel(t, vercelClaims(now))
	for _, rules := range [][]TrustRule{{otherProject, testVercelRule()}, {testVercelRule(), otherProject}} {
		_, err := checkAt(t, c, now, vercel, rules...)
		require.ErrorIs(t, err, errIssuerKeys, "the keys of one rule could not be fetched")
	}

	claims := kubernetesClaims(now)
	claims["aud"] = "https://vault.example.com"
	wrongAudience := signToken(t, jose.RS256, testRSAKey(), testKID, claims)
	signed := testKubernetesRule(t, publicJWK(testRSAKey(), testKID))
	for name, unsigned := range map[string]TrustRule{
		"another kid":          testKubernetesRule(t, publicJWK(testRSAKey(), "another")),
		"another key":          testKubernetesRule(t, publicJWK(otherRSAKey(), testKID)),
		"a key of another alg": testKubernetesRule(t, publicJWK(testECDSAKey(), testKID)),
	} {
		for _, rules := range [][]TrustRule{{unsigned, signed}, {signed, unsigned}} {
			_, err := checkAt(t, c, now, wrongAudience, rules...)
			requireTokenRefused(t, err, "its aud must be https://faucet.example.com alone", name)
		}
	}
}

func TestCheckStopsWhenTheContextEnds(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	offline := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, net.ErrClosed
	})}
	clock := quartz.NewMock(t)
	now := clock.Now()
	c := newWorkloadChecker(offline, clock)
	otherProject := testVercelRule()
	otherProject.Claims["project_id"] = "prj_other"
	otherProject.JWKS = must(json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{publicJWK(testRSAKey(), testKID)}}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.check(ctx, []TrustRule{otherProject, testVercelRule()}, signVercel(t, vercelClaims(now)), testAudience, now)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, errSubjectToken)
	require.Zero(t, requests.Load())
}

// A rule that a store kept before its issuer's profile refused it matches
// nothing, and costs no fetch.
func TestARuleThatTheProfilesRefuseMatchesNothing(t *testing.T) {
	t.Parallel()
	s := newIssuerServer(t)
	s.serveVercel(t, publicJWK(testRSAKey(), testKID))
	clock := quartz.NewMock(t)
	now := clock.Now()
	preview := testVercelRule()
	preview.Claims["environment"] = "preview"
	claims := vercelClaims(now)
	claims["environment"] = "preview"
	_, err := checkAt(t, newWorkloadChecker(s.client, clock), now, signVercel(t, claims), preview)
	requireTokenRefused(t, err, "it matches no trust rule of the client")
	require.Zero(t, s.hitsOf(testVercelDiscovery))
}

// Through the guarded client, a discovery document cannot send faucet to an
// address that is not public, and no redirect is followed.
func TestADiscoveryDocumentCannotSendFaucetToAPrivateAddress(t *testing.T) {
	t.Parallel()
	var jwksURI atomic.Value
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != discoveryPath {
			http.Redirect(w, r, discoveryPath, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"issuer":%q,"jwks_uri":%q}`, "https://"+r.Host, jwksURI.Load())
	}))
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	client := faucet.NewHTTPClient(faucet.HTTPClientOptions{
		AllowInsecureLoopback: true, RootCAs: roots,
		Resolver: testResolver{"keys.example.com": {netip.MustParseAddr("10.0.0.1")}},
	})
	s := newKeySource(client, quartz.NewMock(t))
	for _, uri := range []string{
		"https://10.0.0.1/keys", "https://169.254.169.254/keys", "https://[fd00::1]/keys", "https://keys.example.com/keys", "http://keys.example.com/keys",
		"https://10.0.0.1/keys?token=hunter2",
	} {
		jwksURI.Store(uri)
		_, err := s.fetch(context.Background(), srv.URL)
		require.ErrorContains(t, err, "the key set: ", uri)
		require.ErrorIs(t, err, faucet.ErrDestinationRefused, uri)
		require.NotContains(t, err.Error(), "/keys", "the error, which the grant logs, quotes no URL: %s", uri)
	}
	_, err := s.fetch(context.Background(), srv.URL+"/moved")
	require.ErrorContains(t, err, "the discovery document: it must be served with status 200 and no redirect, not 302")
}

func TestABodyThatStallsPastTheDeadlineArrivesLate(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issuer":`))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := newKeySource(srv.Client(), quartz.NewMock(t)).get(ctx, srv.URL+discoveryPath)
	require.EqualError(t, err, "it must arrive within 5s")
	require.NotErrorIs(t, err, context.DeadlineExceeded)
}

type testResolver map[string][]netip.Addr

func (r testResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	return r[host], nil
}

func TestChecksThatStartAtOnceShareOneFetch(t *testing.T) {
	t.Parallel()
	s := newIssuerServer(t)
	s.serveVercel(t, publicJWK(testRSAKey(), testKID))
	hold := make(chan struct{})
	s.mu.Lock()
	s.hold = hold
	s.mu.Unlock()
	clock := quartz.NewMock(t)
	now := clock.Now()
	c := newWorkloadChecker(s.client, clock)
	token := signVercel(t, vercelClaims(now))

	const checks = 8
	errs := make([]error, checks)
	var wg sync.WaitGroup
	for i := range checks {
		wg.Go(func() { _, errs[i] = checkAt(t, c, now, token, testVercelRule()) })
	}
	require.Eventually(t, func() bool { return s.hitsOf(testVercelDiscovery) > 0 }, 10*time.Second, time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	close(hold)
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "check %d", i)
	}
	require.Equal(t, 1, s.hitsOf(testVercelDiscovery))
	require.Equal(t, 1, s.hitsOf(testVercelKeys))
}

func TestFaucetKeepsTheKeysOfAtMostMaxCachedKeySetsIssuers(t *testing.T) {
	t.Parallel()
	s := newIssuerServer(t)
	keys := must(json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{publicJWK(testRSAKey(), testKID)}}))
	clock := quartz.NewMock(t)
	c := newWorkloadChecker(s.client, clock)
	check := func(i int) {
		t.Helper()
		issuer := fmt.Sprintf("https://cluster-%d.example.com", i)
		claims := kubernetesClaims(clock.Now())
		claims["iss"] = issuer
		rule := TrustRule{Issuer: issuer, Claims: map[string]string{"sub": "system:serviceaccount:kube-system:coredns"}}
		_, err := checkAt(t, c, clock.Now(), signToken(t, jose.RS256, testRSAKey(), testKID, claims), rule)
		require.NoError(t, err)
	}
	discoveryOf := func(i int) string { return fmt.Sprintf("cluster-%d.example.com%s", i, discoveryPath) }
	for i := range maxCachedKeySets + 1 {
		host := fmt.Sprintf("cluster-%d.example.com", i)
		s.serve(host+discoveryPath, must(json.Marshal(map[string]string{"issuer": "https://" + host, "jwks_uri": "https://" + host + "/openid/v1/jwks"})))
		s.serve(host+"/openid/v1/jwks", keys)
		check(i)
		clock.Advance(time.Second)
	}
	check(maxCachedKeySets)
	require.Equal(t, 1, s.hitsOf(discoveryOf(maxCachedKeySets)))
	check(1)
	require.Equal(t, 1, s.hitsOf(discoveryOf(1)))
	check(0)
	require.Equal(t, 2, s.hitsOf(discoveryOf(0)), "the first issuer's keys were dropped")
}
