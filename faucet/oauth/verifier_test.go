package oauth_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/oauth"
	"github.com/coder/quartz"
)

const resource = "https://faucet.example.com/github/mcp"

func newAccessToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return "fct_" + base64.RawURLEncoding.EncodeToString(b)
}

type verifierHarness struct {
	t     *testing.T
	store *fakeStore
	clock *quartz.Mock
	// live holds the subjects that the resolvers find. A subject that is
	// missing has ended.
	live     map[string]bool
	verifier *oauth.Verifier
}

func newVerifierHarness(t *testing.T, kinds ...oauth.SubjectKind) *verifierHarness {
	t.Helper()
	h := &verifierHarness{t: t, store: newFakeStore(), clock: quartz.NewMock(t), live: map[string]bool{}}
	resolvers := map[oauth.SubjectKind]oauth.SubjectResolver{}
	for _, kind := range kinds {
		resolvers[kind] = h.resolve(kind)
	}
	var err error
	h.verifier, err = oauth.NewVerifier(h.store, h.clock, resolvers)
	require.NoError(t, err)
	return h
}

func (h *verifierHarness) resolve(kind oauth.SubjectKind) oauth.SubjectResolver {
	return func(_ context.Context, subject string, scopes []faucet.Scope) (faucet.Principal, error) {
		if !h.live[subject] {
			return faucet.Principal{}, faucet.ErrInvalidToken
		}
		if kind == oauth.SubjectUser {
			return faucet.NewOAuthUser("alice", faucet.OAuthSession{SessionID: subject, ConnectionID: "conn_alice"}, scopes)
		}
		return faucet.NewService(faucet.ServiceClient{ClientID: subject}, scopes)
	}
}

// issue stores token for subject as a service token for resource that
// expires in 15 minutes, and makes the subject live.
func (h *verifierHarness) issue(token string, mutate ...func(*oauth.AccessToken)) {
	h.t.Helper()
	at := oauth.AccessToken{
		Hash:        oauth.HashToken(token),
		ExpiresAt:   h.clock.Now().Add(15 * time.Minute),
		Scopes:      []faucet.Scope{faucet.ScopeEventsRead},
		Resource:    resource,
		SubjectKind: oauth.SubjectService,
		Subject:     "ci-bot",
	}
	for _, m := range mutate {
		m(&at)
	}
	h.live[at.Subject] = true
	require.NoError(h.t, h.store.InsertAccessToken(context.Background(), at))
}

func (h *verifierHarness) requireRejected(token, why string) {
	h.t.Helper()
	_, err := h.verifier.Verify(context.Background(), token, resource)
	require.ErrorIs(h.t, err, faucet.ErrInvalidToken, why)
}

func TestVerifyReturnsThePrincipalOfALiveSubjectWithTheTokensScopes(t *testing.T) {
	t.Parallel()

	h := newVerifierHarness(t, oauth.SubjectService, oauth.SubjectUser)
	service, user := newAccessToken(t), newAccessToken(t)
	h.issue(service)
	h.issue(user, func(at *oauth.AccessToken) {
		at.SubjectKind, at.Subject = oauth.SubjectUser, "sess_1"
		at.Scopes = []faucet.Scope{faucet.ScopeEventsSubscribe}
	})

	p, err := h.verifier.Verify(context.Background(), service, resource)
	require.NoError(t, err)
	require.Equal(t, "svc:ci-bot", p.Login)
	require.True(t, p.HasScope(faucet.ScopeEventsRead))
	require.False(t, p.HasScope(faucet.ScopeEventsSubscribe), "the principal holds the scopes of the token, no more")

	p, err = h.verifier.Verify(context.Background(), user, resource)
	require.NoError(t, err)
	session, ok := p.OAuthSession()
	require.True(t, ok)
	require.Equal(t, "sess_1", session.SessionID, "a user principal carries the session of its token")
	require.True(t, p.HasScope(faucet.ScopeEventsSubscribe))
	require.False(t, p.HasScope(faucet.ScopeEventsRead))
}

func TestVerifyRejectsAnExpiredToken(t *testing.T) {
	t.Parallel()

	h := newVerifierHarness(t, oauth.SubjectService)
	token := newAccessToken(t)
	h.issue(token)

	h.clock.Advance(15*time.Minute - time.Nanosecond)
	_, err := h.verifier.Verify(context.Background(), token, resource)
	require.NoError(t, err, "a token is good until its expiry")
	h.clock.Advance(time.Nanosecond)
	h.requireRejected(token, "a token is not good at its expiry")
}

func TestVerifyRejectsATokenForAnotherResource(t *testing.T) {
	t.Parallel()

	h := newVerifierHarness(t, oauth.SubjectService)
	token := newAccessToken(t)
	h.issue(token, func(at *oauth.AccessToken) { at.Resource = "https://faucet.example.com/mcp" })
	h.requireRejected(token, "a token is valid only on the resource it was issued for")
	h.requireRejected(newAccessToken(t), "a token that faucet never stored")
}

func TestVerifyRejectsATokenWhoseSubjectEnded(t *testing.T) {
	t.Parallel()

	h := newVerifierHarness(t, oauth.SubjectService)
	token := newAccessToken(t)
	h.issue(token)
	h.live["ci-bot"] = false
	h.requireRejected(token, "a disabled client's token stops working before it expires")
}

func TestVerifyRejectsASubjectKindWithNoResolver(t *testing.T) {
	t.Parallel()

	h := newVerifierHarness(t, oauth.SubjectService)
	token := newAccessToken(t)
	h.issue(token, func(at *oauth.AccessToken) { at.SubjectKind, at.Subject = oauth.SubjectUser, "sess_1" })
	h.requireRejected(token, "faucet cannot tell whether such a subject is live")
}

func TestVerifyRejectsWhatIsNotAnAccessTokenWithoutReadingTheStore(t *testing.T) {
	t.Parallel()

	h := newVerifierHarness(t, oauth.SubjectService)
	token := newAccessToken(t)
	h.issue(token)
	body := strings.TrimPrefix(token, "fct_")
	for _, bad := range []string{
		"fct_short",
		"fcr_" + body,
		"fcs_" + body,
		"FCT_" + body,
		"fct_" + body + "A",
		"fct_" + body[:42],
		"fct_" + body[:42] + "+",
		"fct_" + body + "=",
		"ghp_" + body,
		"",
	} {
		h.requireRejected(bad, bad)
	}
	require.Zero(t, h.store.readCount(), "a bearer that is not an access token is refused before the store")
}

func TestVerifyReportsAStoreFailureAsNoAnswer(t *testing.T) {
	t.Parallel()

	h := newVerifierHarness(t, oauth.SubjectService)
	h.store.err = xerrors.New("connection refused")
	_, err := h.verifier.Verify(context.Background(), newAccessToken(t), resource)
	require.Error(t, err)
	require.NotErrorIs(t, err, faucet.ErrInvalidToken, "a check that could not run is not a rejected token")
}

func TestMintedKnowsEveryFaucetPrefix(t *testing.T) {
	t.Parallel()

	for _, token := range []string{"fct_", "fct_x", "fcr_x", "fcs_x"} {
		require.True(t, oauth.Minted(token), token)
	}
	for _, token := range []string{"", "ghp_x", "gho_x", "github_pat_x", "FCT_x", "fct", "xfct_x"} {
		require.False(t, oauth.Minted(token), token)
	}
}

func TestNewVerifierNeedsItsParts(t *testing.T) {
	t.Parallel()

	_, err := oauth.NewVerifier(nil, quartz.NewMock(t), nil)
	require.Error(t, err)
	_, err = oauth.NewVerifier(newFakeStore(), nil, nil)
	require.Error(t, err)
	_, err = oauth.NewVerifier(newFakeStore(), quartz.NewMock(t), map[oauth.SubjectKind]oauth.SubjectResolver{oauth.SubjectService: nil})
	require.Error(t, err)
}
