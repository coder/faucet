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

	"github.com/coder/quartz"
	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/oauth"
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

	for _, token := range []string{"fct_", "fct_x", "fcr_x", "fcs_x", "FCT_x", "Fcr_x", "fcS_x"} {
		require.True(t, oauth.Minted(token), token)
	}
	for _, token := range []string{"", "ghp_x", "gho_x", "github_pat_x", "fct", "xfct_x", "fct-x"} {
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

func TestVerifyRefusesAPrincipalThatIsNotTheTokensSubject(t *testing.T) {
	t.Parallel()

	read := []faucet.Scope{faucet.ScopeEventsRead}
	for name, c := range map[string]struct {
		kind    oauth.SubjectKind
		resolve oauth.SubjectResolver
	}{
		"a scope the token does not hold": {oauth.SubjectService, func(_ context.Context, id string, _ []faucet.Scope) (faucet.Principal, error) {
			return faucet.NewService(faucet.ServiceClient{ClientID: id}, []faucet.Scope{faucet.ScopeEventsRead, faucet.ScopeEventsSubscribe})
		}},
		"another client": {oauth.SubjectService, func(_ context.Context, _ string, scopes []faucet.Scope) (faucet.Principal, error) {
			return faucet.NewService(faucet.ServiceClient{ClientID: "other-bot"}, scopes)
		}},
		"another kind for a service": {oauth.SubjectService, func(_ context.Context, id string, scopes []faucet.Scope) (faucet.Principal, error) {
			return faucet.NewOAuthUser("alice", faucet.OAuthSession{SessionID: id, ConnectionID: "c"}, scopes)
		}},
		"a GitHub token caller for a service": {oauth.SubjectService, func(context.Context, string, []faucet.Scope) (faucet.Principal, error) {
			return faucet.NewPrincipal("alice", "")
		}},
		"another session": {oauth.SubjectUser, func(_ context.Context, _ string, scopes []faucet.Scope) (faucet.Principal, error) {
			return faucet.NewOAuthUser("alice", faucet.OAuthSession{SessionID: "sess_other", ConnectionID: "c"}, scopes)
		}},
		"another kind for a user": {oauth.SubjectUser, func(_ context.Context, id string, scopes []faucet.Scope) (faucet.Principal, error) {
			return faucet.NewService(faucet.ServiceClient{ClientID: id}, scopes)
		}},
		"a service renamed after a person": {oauth.SubjectService, func(_ context.Context, id string, scopes []faucet.Scope) (faucet.Principal, error) {
			p, err := faucet.NewService(faucet.ServiceClient{ClientID: id}, scopes)
			p.Login = "octocat"
			return p, err
		}},
		"a user renamed as a service": {oauth.SubjectUser, func(_ context.Context, id string, scopes []faucet.Scope) (faucet.Principal, error) {
			p, err := faucet.NewOAuthUser("alice", faucet.OAuthSession{SessionID: id, ConnectionID: "c"}, scopes)
			p.Login = "svc:ci-bot"
			return p, err
		}},
		"a scope widened in the slice it was given": {oauth.SubjectService, func(_ context.Context, id string, scopes []faucet.Scope) (faucet.Principal, error) {
			scopes[0] = faucet.ScopeEventsSubscribe
			return faucet.NewService(faucet.ServiceClient{ClientID: id}, scopes)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store, clock := newFakeStore(), quartz.NewMock(t)
			v, err := oauth.NewVerifier(store, clock, map[oauth.SubjectKind]oauth.SubjectResolver{c.kind: c.resolve})
			require.NoError(t, err)
			token := newAccessToken(t)
			require.NoError(t, store.InsertAccessToken(context.Background(), oauth.AccessToken{
				Hash: oauth.HashToken(token), ExpiresAt: clock.Now().Add(time.Minute), Scopes: read,
				Resource: resource, SubjectKind: c.kind, Subject: "sess_1",
			}))
			p, err := v.Verify(context.Background(), token, resource)
			require.Error(t, err, "the resolver answered for someone the token was not issued to")
			require.NotErrorIs(t, err, faucet.ErrInvalidToken, "a resolver that answers for another principal is a faucet bug, not a bad token")
			require.Equal(t, faucet.Principal{}, p)
		})
	}
}

func TestVerifyRejectsAStoredTokenThatThisBuildCannotRead(t *testing.T) {
	t.Parallel()

	h := newVerifierHarness(t, oauth.SubjectService)
	unknownScope, unknownKind := newAccessToken(t), newAccessToken(t)
	h.live["ci-bot"] = true
	h.store.tokens[oauth.HashToken(unknownScope)] = oauth.AccessToken{
		Hash: oauth.HashToken(unknownScope), ExpiresAt: h.clock.Now().Add(time.Minute), Scopes: []faucet.Scope{"events:admin"},
		Resource: resource, SubjectKind: oauth.SubjectService, Subject: "ci-bot",
	}
	h.store.tokens[oauth.HashToken(unknownKind)] = oauth.AccessToken{
		Hash: oauth.HashToken(unknownKind), ExpiresAt: h.clock.Now().Add(time.Minute), Scopes: []faucet.Scope{faucet.ScopeEventsRead},
		Resource: resource, SubjectKind: "robot", Subject: "ci-bot",
	}
	h.requireRejected(unknownScope, "a scope that a later build wrote is a rejected token, not a failed check")
	h.requireRejected(unknownKind, "so is a subject kind")
}

func TestVerifyNamesWhyItRejectsAndNeverTheToken(t *testing.T) {
	t.Parallel()

	h := newVerifierHarness(t, oauth.SubjectService)
	expired, elsewhere, ended := newAccessToken(t), newAccessToken(t), newAccessToken(t)
	h.issue(expired, func(at *oauth.AccessToken) { at.ExpiresAt = h.clock.Now() })
	h.issue(elsewhere, func(at *oauth.AccessToken) { at.Resource = "https://faucet.example.com/mcp" })
	h.issue(ended, func(at *oauth.AccessToken) { at.Subject = "gone-bot" })
	h.live["gone-bot"] = false
	for token, reason := range map[string]string{
		expired:              "expired",
		elsewhere:            "another resource",
		ended:                "subject",
		newAccessToken(t):    "unknown",
		"fcr_" + expired[4:]: "not an access token",
	} {
		_, err := h.verifier.Verify(context.Background(), token, resource)
		require.ErrorIs(t, err, faucet.ErrInvalidToken)
		require.Contains(t, err.Error(), reason)
		require.NotContains(t, err.Error(), token[4:20], "an error never holds the token")
	}
}
