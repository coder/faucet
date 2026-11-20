package storetest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/oauth"
)

// loginSubtests is the contract of oauth.LoginStore, one behavior per name.
var loginSubtests = []struct {
	name string
	run  func(t *testing.T, s oauth.Store)
}{
	// Authorization requests.
	{"ConsentStoresAPendingRequestThatItsGitHubStateFinds", consentStoresAPendingRequestThatItsGitHubStateFinds},
	{"ConsentValidatesInput", consentValidatesInput},
	{"ConsentRefusesAnExpiredRequestAndAnotherRequestOfTheID", consentRefusesAnExpiredRequestAndAnotherRequestOfTheID},
	{"ALaterConsentOfTheSameBrowserReplacesTheGitHubState", aLaterConsentOfTheSameBrowserReplacesTheGitHubState},
	{"ExpireEndsOnlyAPendingRequest", expireEndsOnlyAPendingRequest},
	{"ApproveStoresTheConnectionAndTheCodeInOneStep", approveStoresTheConnectionAndTheCodeInOneStep},
	{"ApproveRefusesAnOutdatedGitHubStateAnExpiredRequestAndASecondApproval", approveRefusesAnOutdatedGitHubStateAnExpiredRequestAndASecondApproval},
	{"ARefusedApprovalLeavesTheConnectionAsItWas", aRefusedApprovalLeavesTheConnectionAsItWas},
	{"ApproveReplacesTheTokensOfAUserWhoLogsInAgain", approveReplacesTheTokensOfAUserWhoLogsInAgain},
	{"ApproveEndsWhatTheReplacedConnectionServesUnlessItIsAlive", approveEndsWhatTheReplacedConnectionServesUnlessItIsAlive},
	{"RedeemMakesTheSessionAndItsFirstAccessTokenFromTheRequest", redeemMakesTheSessionAndItsFirstAccessTokenFromTheRequest},
	{"RedeemRefusesAnExpiredCodeAnUnknownCodeAndASecondUse", redeemRefusesAnExpiredCodeAnUnknownCodeAndASecondUse},
	{"OnlyOneOfManyRedemptionsAtOnceMakesASession", onlyOneOfManyRedemptionsAtOnceMakesASession},
	{"PurgeDeletesTheRequestsWithoutACodeAtExpiryAndTheOthersBeforeTheTime", purgeDeletesTheRequestsWithoutACodeAtExpiryAndTheOthersBeforeTheTime},
	{"ConsentApproveRedeemAndRotateValidateInput", consentApproveRedeemAndRotateValidateInput},
	{"AHashThatAnotherRowHoldsIsRefusedAndChangesNothing", aHashThatAnotherRowHoldsIsRefusedAndChangesNothing},
	// Sessions.
	{"ARefreshTokenIsFoundUntilItsSessionGoes", aRefreshTokenIsFoundUntilItsSessionGoes},
	{"RotateTradesTheTokenAndStoresTheNextAccessToken", rotateTradesTheTokenAndStoresTheNextAccessToken},
	{"RotateTradesAUsedTokenAgainWithinItsGrace", rotateTradesAUsedTokenAgainWithinItsGrace},
	{"RotateAtAnEarlierTimeKeepsTheLaterRefreshedTime", rotateAtAnEarlierTimeKeepsTheLaterRefreshedTime},
	{"RotateRetiresTheUnusedTokensThatNoClientStillHolds", rotateRetiresTheUnusedTokensThatNoClientStillHolds},
	{"RotateRefusesATokenUsedBeforeItsGraceAnEndedSessionAndAnUnknownToken", rotateRefusesATokenUsedBeforeItsGraceAnEndedSessionAndAnUnknownToken},
	{"RotateRefusesAnAccessTokenWithAScopeThatTheSessionLacks", rotateRefusesAnAccessTokenWithAScopeThatTheSessionLacks},
	{"EveryRotationOfOneTokenAtOnceTradesIt", everyRotationOfOneTokenAtOnceTradesIt},
	{"SiblingTokensRotatedAtOnceTradeOrAreRefused", siblingTokensRotatedAtOnceTradeOrAreRefused},
	{"EndSessionKeepsTheFirstTime", endSessionKeepsTheFirstTime},
	{"DeleteEndedSessionsDeletesTheEndedAndTheIdleWithTheirTokens", deleteEndedSessionsDeletesTheEndedAndTheIdleWithTheirTokens},
	{"DeleteUnusedConnectionsDeletesTheOnesThatNothingNames", deleteUnusedConnectionsDeletesTheOnesThatNothingNames},
	// Connections.
	{"ConnectionOfAnUnknownIDIsUnknown", connectionOfAnUnknownIDIsUnknown},
	{"RefreshConnectionKeepsReplacesOrEndsTheSessions", refreshConnectionKeepsReplacesOrEndsTheSessions},
	{"RefreshConnectionEndsOnlyTheLiveSessionsOfItsConnection", refreshConnectionEndsOnlyTheLiveSessionsOfItsConnection},
	{"RefreshConnectionChangesNothingWhenItsCallbackFailsOrItsChangeIsNotValid", refreshConnectionChangesNothingWhenItsCallbackFailsOrItsChangeIsNotValid},
	{"ASecondRefreshOfAConnectionWaitsAndSeesWhatTheFirstStored", aSecondRefreshOfAConnectionWaitsAndSeesWhatTheFirstStored},
	{"AnApprovalWaitsForARefreshOfItsConnection", anApprovalWaitsForARefreshOfItsConnection},
	{"ARefreshWaitsForTheCheckOfAnApproval", aRefreshWaitsForTheCheckOfAnApproval},
	{"StepsThatDoNotChangeTheConnectionDoNotWaitForItsRefresh", stepsThatDoNotChangeTheConnectionDoNotWaitForItsRefresh},
	{"LoginRowsShareNoSliceWithTheirCaller", loginRowsShareNoSliceWithTheirCaller},
}

// runOAuthLogin runs the oauth.LoginStore contract. It takes an oauth.Store
// because a step of a login stores access tokens, which AccessToken reads.
func runOAuthLogin(t *testing.T, newStore func(t *testing.T) oauth.Store) {
	for _, st := range loginSubtests {
		t.Run(st.name, func(t *testing.T) {
			t.Parallel()
			st.run(t, newStore(t))
		})
	}
}

// holdFor is how long a test lets a step that must wait try to get ahead.
// A store that does not make it wait takes far less.
const holdFor = 200 * time.Millisecond

// loginNow is the time of a test. Postgres keeps microseconds, so it has
// none below them.
func loginNow() time.Time { return time.Now().Truncate(time.Microsecond) }

func browserOf(id string) oauth.TokenHash     { return oauth.HashToken("browser " + id) }
func githubStateOf(id string) oauth.TokenHash { return oauth.HashToken("github state " + id) }
func codeOf(id string) oauth.TokenHash        { return oauth.HashToken("code " + id) }
func refreshOf(name string) oauth.TokenHash   { return oauth.HashToken("fcr_" + name) }
func accessOf(name string) oauth.TokenHash    { return oauth.HashToken("fct_" + name) }

// authRequestAt is a valid new request made at created. The hashes of its
// secrets are named after its ID.
func authRequestAt(id string, created time.Time) oauth.AuthRequest {
	return oauth.AuthRequest{
		ID:            id,
		State:         oauth.AuthRequestPending,
		ClientID:      "https://agent.example.com/client.json",
		ClientName:    "Example Agent",
		RedirectURI:   "http://127.0.0.1:33418/callback",
		ClientState:   "client state " + id,
		Resource:      "https://faucet.example.com/github/mcp",
		Scopes:        []faucet.Scope{faucet.ScopeEventsRead, faucet.ScopeEventsSubscribe},
		CodeChallenge: "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM",
		BrowserHash:   browserOf(id),
		CreatedAt:     created,
		ExpiresAt:     created.Add(10 * time.Minute),
	}
}

// connectionAt is a valid connection of the GitHub user with this ID,
// updated at updated. Each call makes new token slices.
func connectionAt(userID int64, updated time.Time) oauth.Connection {
	return oauth.Connection{
		ID:               oauth.ConnectionID(oauth.ProviderGitHub, userID),
		Provider:         oauth.ProviderGitHub,
		UpstreamUserID:   userID,
		Login:            fmt.Sprintf("user%d", userID),
		AccessToken:      fmt.Appendf(nil, "sealed access token of %d at %d", userID, updated.UnixMicro()),
		RefreshToken:     fmt.Appendf(nil, "sealed refresh token of %d at %d", userID, updated.UnixMicro()),
		KeyID:            "key-1",
		AccessExpiresAt:  updated.Add(8 * time.Hour),
		RefreshExpiresAt: updated.Add(180 * 24 * time.Hour),
		UpdatedAt:        updated,
	}
}

// stillAlive and revoked answer an approval's question whether GitHub
// still takes the authorization of the connection that it replaces.
func stillAlive(oauth.Connection) bool { return true }
func revoked(oauth.Connection) bool    { return false }

// approvalOf approves r, with the GitHub state of its first consent, at now.
func approvalOf(r oauth.AuthRequest, c oauth.Connection, now time.Time) oauth.Approval {
	return oauth.Approval{
		RequestID:   r.ID,
		GitHubState: githubStateOf(r.ID),
		Connection:  c,
		Code:        codeOf(r.ID),
		Now:         now,
		ExpiresAt:   now.Add(60 * time.Second),
	}
}

// redemptionOf redeems the code of the request with this ID at now. Its
// tokens are named after name.
func redemptionOf(id, name string, now time.Time) oauth.Redemption {
	return oauth.Redemption{
		Code:            codeOf(id),
		Refresh:         refreshOf(name),
		Access:          accessOf(name),
		AccessExpiresAt: now.Add(time.Hour),
		Now:             now,
	}
}

// reuseGrace is the reuse grace of the rotations of a test.
const reuseGrace = 30 * time.Second

// rotationOf presents the refresh token with this hash at now, with a reuse
// grace of reuseGrace. The tokens it asks for are named after name.
func rotationOf(presented oauth.TokenHash, name string, now time.Time) oauth.Rotation {
	return oauth.Rotation{
		Presented:       presented,
		Next:            refreshOf(name),
		Access:          accessOf(name),
		AccessExpiresAt: now.Add(time.Hour),
		Scopes:          []faucet.Scope{faucet.ScopeEventsRead},
		UsedAfter:       now.Add(-reuseGrace),
		Now:             now,
	}
}

// consentOf is r once its user pressed Continue: it holds the GitHub state
// of its ID.
func consentOf(r oauth.AuthRequest) oauth.AuthRequest {
	r.GitHubStateHash = githubStateOf(r.ID)
	return r
}

// consentAgain is the consent of r's request, from r's browser, that
// presses Continue again and holds githubState.
func consentAgain(r oauth.AuthRequest, githubState oauth.TokenHash) oauth.AuthRequest {
	again := authRequestAt(r.ID, r.CreatedAt)
	again.BrowserHash, again.GitHubStateHash = r.BrowserHash, githubState
	return again
}

// consentedRequest stores the request with this ID, made at now, whose
// user pressed Continue.
func consentedRequest(t *testing.T, s oauth.Store, id string, now time.Time) oauth.AuthRequest {
	t.Helper()
	r := consentOf(authRequestAt(id, now))
	require.NoError(t, s.ConsentAuthRequest(context.Background(), r, now))
	return r
}

// approvedRequest stores the request with this ID and approves it for c,
// all at now.
func approvedRequest(t *testing.T, s oauth.Store, id string, c oauth.Connection, now time.Time) oauth.AuthRequest {
	t.Helper()
	return approve(t, s, consentedRequest(t, s, id, now), c, now)
}

// approve approves r, which holds the GitHub state of its first consent,
// for c at now, and returns it as it is after.
func approve(t *testing.T, s oauth.Store, r oauth.AuthRequest, c oauth.Connection, now time.Time) oauth.AuthRequest {
	t.Helper()
	a := approvalOf(r, c, now)
	require.NoError(t, s.ApproveAuthRequest(context.Background(), a, stillAlive))
	r.State, r.CodeHash, r.ConnectionID, r.ExpiresAt = oauth.AuthRequestApproved, a.Code, c.ID, a.ExpiresAt
	return r
}

// newSession logs the user of c in with the request with this ID, all at
// now. The session's first tokens are named after the ID.
func newSession(t *testing.T, s oauth.Store, id string, c oauth.Connection, now time.Time) oauth.Session {
	t.Helper()
	approvedRequest(t, s, id, c, now)
	session, err := s.RedeemAuthRequest(context.Background(), redemptionOf(id, id, now))
	require.NoError(t, err)
	return session
}

func requireSameAuthRequest(t *testing.T, want, got oauth.AuthRequest) {
	t.Helper()
	require.True(t, want.CreatedAt.Equal(got.CreatedAt), "created_at: want %v, got %v", want.CreatedAt, got.CreatedAt)
	require.True(t, want.ExpiresAt.Equal(got.ExpiresAt), "expires_at: want %v, got %v", want.ExpiresAt, got.ExpiresAt)
	got.CreatedAt, got.ExpiresAt = want.CreatedAt, want.ExpiresAt
	require.Equal(t, want, got)
}

func requireSameSession(t *testing.T, want, got oauth.Session) {
	t.Helper()
	require.True(t, want.RefreshedAt.Equal(got.RefreshedAt), "refreshed_at: want %v, got %v", want.RefreshedAt, got.RefreshedAt)
	require.True(t, want.EndedAt.Equal(got.EndedAt), "ended_at: want %v, got %v", want.EndedAt, got.EndedAt)
	got.RefreshedAt, got.EndedAt = want.RefreshedAt, want.EndedAt
	require.Equal(t, want, got)
}

func requireSameConnection(t *testing.T, want, got oauth.Connection) {
	t.Helper()
	require.True(t, want.AccessExpiresAt.Equal(got.AccessExpiresAt), "access_expires_at: want %v, got %v", want.AccessExpiresAt, got.AccessExpiresAt)
	require.True(t, want.RefreshExpiresAt.Equal(got.RefreshExpiresAt), "refresh_expires_at: want %v, got %v", want.RefreshExpiresAt, got.RefreshExpiresAt)
	require.True(t, want.UpdatedAt.Equal(got.UpdatedAt), "updated_at: want %v, got %v", want.UpdatedAt, got.UpdatedAt)
	got.AccessExpiresAt, got.RefreshExpiresAt, got.UpdatedAt = want.AccessExpiresAt, want.RefreshExpiresAt, want.UpdatedAt
	require.Equal(t, want, got)
}

func requireRequestByGitHubState(t *testing.T, s oauth.Store, want oauth.AuthRequest) {
	t.Helper()
	got, err := s.AuthRequestByGitHubState(context.Background(), want.GitHubStateHash)
	require.NoError(t, err)
	requireSameAuthRequest(t, want, got)
}

func requireRequestByCode(t *testing.T, s oauth.Store, want oauth.AuthRequest) {
	t.Helper()
	got, err := s.AuthRequestByCode(context.Background(), want.CodeHash)
	require.NoError(t, err)
	requireSameAuthRequest(t, want, got)
}

func requireNoRequestByGitHubState(t *testing.T, s oauth.Store, githubState oauth.TokenHash, msgAndArgs ...any) {
	t.Helper()
	_, err := s.AuthRequestByGitHubState(context.Background(), githubState)
	require.ErrorIs(t, err, oauth.ErrUnknownAuthRequest, msgAndArgs...)
}

func requireNoRequestByCode(t *testing.T, s oauth.Store, code oauth.TokenHash, msgAndArgs ...any) {
	t.Helper()
	_, err := s.AuthRequestByCode(context.Background(), code)
	require.ErrorIs(t, err, oauth.ErrUnknownAuthRequest, msgAndArgs...)
}

func requireSessionIs(t *testing.T, s oauth.Store, want oauth.Session) {
	t.Helper()
	got, err := s.Session(context.Background(), want.ID)
	require.NoError(t, err)
	requireSameSession(t, want, got)
}

func requireConnectionIs(t *testing.T, s oauth.Store, want oauth.Connection, msgAndArgs ...any) {
	t.Helper()
	got, err := s.Connection(context.Background(), want.ID)
	require.NoError(t, err, msgAndArgs...)
	requireSameConnection(t, want, got)
}

// requireRefreshToken checks that the store holds want, of the session
// with that ID.
func requireRefreshToken(t *testing.T, s oauth.Store, want oauth.RefreshToken, msgAndArgs ...any) {
	t.Helper()
	session, got, err := s.SessionByRefreshToken(context.Background(), want.Hash)
	require.NoError(t, err, msgAndArgs...)
	require.Equal(t, want.SessionID, session.ID, msgAndArgs...)
	require.True(t, want.IssuedAt.Equal(got.IssuedAt), "issued_at: want %v, got %v", want.IssuedAt, got.IssuedAt)
	require.True(t, want.UsedAt.Equal(got.UsedAt), "used_at: want %v, got %v", want.UsedAt, got.UsedAt)
	require.True(t, want.RetiredAt.Equal(got.RetiredAt), "retired_at: want %v, got %v", want.RetiredAt, got.RetiredAt)
	got.IssuedAt, got.UsedAt, got.RetiredAt = want.IssuedAt, want.UsedAt, want.RetiredAt
	require.Equal(t, want, got, msgAndArgs...)
}

// requireNoRefreshToken checks that the store holds no refresh token with
// this hash.
func requireNoRefreshToken(t *testing.T, s oauth.Store, refresh oauth.TokenHash, msgAndArgs ...any) {
	t.Helper()
	_, _, err := s.SessionByRefreshToken(context.Background(), refresh)
	require.ErrorIs(t, err, oauth.ErrUnknownSession, msgAndArgs...)
}

// requireNoTokens checks that no session has this refresh token and that no
// access token has this hash.
func requireNoTokens(t *testing.T, s oauth.Store, refresh, access oauth.TokenHash, msgAndArgs ...any) {
	t.Helper()
	ctx := context.Background()
	_, _, err := s.SessionByRefreshToken(ctx, refresh)
	require.ErrorIs(t, err, oauth.ErrUnknownSession, msgAndArgs...)
	_, err = s.AccessToken(ctx, access)
	require.ErrorIs(t, err, oauth.ErrUnknownToken, msgAndArgs...)
}

func consentStoresAPendingRequestThatItsGitHubStateFinds(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	r := consentOf(authRequestAt("req-1", now))
	r.ClientName, r.ClientState = "", ""
	other := consentOf(authRequestAt("req-2", now))
	require.NoError(t, s.ConsentAuthRequest(ctx, r, now))
	require.NoError(t, s.ConsentAuthRequest(ctx, other, now))

	requireRequestByGitHubState(t, s, r)
	requireRequestByGitHubState(t, s, other)
	for _, h := range []oauth.TokenHash{{}, codeOf(r.ID), r.BrowserHash} {
		requireNoRequestByGitHubState(t, s, h)
		requireNoRequestByCode(t, s, h, "a pending request has no code")
	}
}

func consentValidatesInput(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	for name, mutate := range map[string]func(*oauth.AuthRequest){
		"no ID":                 func(r *oauth.AuthRequest) { r.ID = "" },
		"no state":              func(r *oauth.AuthRequest) { r.State = "" },
		"another state":         func(r *oauth.AuthRequest) { r.State = oauth.AuthRequestApproved },
		"no client ID":          func(r *oauth.AuthRequest) { r.ClientID = "" },
		"no redirect URI":       func(r *oauth.AuthRequest) { r.RedirectURI = "" },
		"no resource":           func(r *oauth.AuthRequest) { r.Resource = "" },
		"no code challenge":     func(r *oauth.AuthRequest) { r.CodeChallenge = "" },
		"no scope":              func(r *oauth.AuthRequest) { r.Scopes = nil },
		"an unknown scope":      func(r *oauth.AuthRequest) { r.Scopes = []faucet.Scope{"events:everything"} },
		"no browser hash":       func(r *oauth.AuthRequest) { r.BrowserHash = oauth.TokenHash{} },
		"no GitHub state":       func(r *oauth.AuthRequest) { r.GitHubStateHash = oauth.TokenHash{} },
		"a code":                func(r *oauth.AuthRequest) { r.CodeHash = codeOf(r.ID) },
		"a connection":          func(r *oauth.AuthRequest) { r.ConnectionID = oauth.ConnectionID(oauth.ProviderGitHub, 1) },
		"no creation time":      func(r *oauth.AuthRequest) { r.CreatedAt = time.Time{} },
		"an expiry at creation": func(r *oauth.AuthRequest) { r.ExpiresAt = r.CreatedAt },
	} {
		id := "req " + name
		r := consentOf(authRequestAt(id, now))
		mutate(&r)
		require.Error(t, s.ConsentAuthRequest(ctx, r, now), name)
		requireNoRequestByGitHubState(t, s, githubStateOf(id), name)
		requireNoRequestByCode(t, s, codeOf(id), name)
	}
}

// A request that has expired is not stored, and a consent stores nothing
// over another request of its ID: another browser's, one at or after its
// expiry, or one that went on.
func consentRefusesAnExpiredRequestAndAnotherRequestOfTheID(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	r := consentedRequest(t, s, "req-1", now)
	expired := consentedRequest(t, s, "req-2", now)
	require.NoError(t, s.ExpireAuthRequest(ctx, expired.ID))
	expired.State = oauth.AuthRequestExpired
	approved := approvedRequest(t, s, "req-3", connectionAt(1, now), now)

	attempt := oauth.HashToken("github state of an attempt")
	again := func(of oauth.AuthRequest, change func(*oauth.AuthRequest)) oauth.AuthRequest {
		a := authRequestAt(of.ID, now)
		a.GitHubStateHash = attempt
		if change != nil {
			change(&a)
		}
		return a
	}
	for name, try := range map[string]struct {
		r   oauth.AuthRequest
		now time.Time
	}{
		"another browser":     {again(r, func(a *oauth.AuthRequest) { a.BrowserHash = browserOf("another browser") }), now},
		"at its expiry":       {again(r, nil), r.ExpiresAt},
		"after its expiry":    {again(r, nil), r.ExpiresAt.Add(time.Hour)},
		"an expired request":  {again(expired, nil), now},
		"an approved request": {again(approved, nil), now},
		"a new request at its expiry": {
			again(oauth.AuthRequest{ID: "req-new"}, nil), now.Add(10 * time.Minute),
		},
	} {
		require.ErrorIs(t, s.ConsentAuthRequest(ctx, try.r, try.now), oauth.ErrUnknownAuthRequest, name)
		requireNoRequestByGitHubState(t, s, attempt, name)
	}
	requireRequestByGitHubState(t, s, r)
	requireRequestByGitHubState(t, s, expired)
	requireRequestByCode(t, s, approved)
	_, err := s.AuthRequestByGitHubState(ctx, githubStateOf("req-new"))
	require.ErrorIs(t, err, oauth.ErrUnknownAuthRequest)

	require.NoError(t, s.ConsentAuthRequest(ctx, again(r, nil), r.ExpiresAt.Add(-time.Microsecond)))
	r.GitHubStateHash = attempt
	requireRequestByGitHubState(t, s, r)
}

// Continue pressed again in the same browser, as after going back to the
// consent page, replaces the GitHub state and keeps the rest of the request.
func aLaterConsentOfTheSameBrowserReplacesTheGitHubState(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	r := consentedRequest(t, s, "req-1", now)
	later := authRequestAt(r.ID, now)
	later.ClientName, later.GitHubStateHash = "Another Name", oauth.HashToken("github state of the second Continue")
	require.NoError(t, s.ConsentAuthRequest(ctx, later, now.Add(time.Minute)))

	requireNoRequestByGitHubState(t, s, githubStateOf(r.ID), "the first state finds the request no more")
	r.GitHubStateHash = later.GitHubStateHash
	requireRequestByGitHubState(t, s, r)
}

func expireEndsOnlyAPendingRequest(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	c := connectionAt(1, now)
	pending := consentedRequest(t, s, "req-1", now)
	approved := approvedRequest(t, s, "req-2", c, now)
	redeemed := approvedRequest(t, s, "req-3", c, now)
	_, err := s.RedeemAuthRequest(ctx, redemptionOf(redeemed.ID, redeemed.ID, now))
	require.NoError(t, err)
	redeemed.State = oauth.AuthRequestRedeemed

	for _, id := range []string{pending.ID, approved.ID, redeemed.ID, pending.ID, "req-unknown"} {
		require.NoError(t, s.ExpireAuthRequest(ctx, id), id)
	}
	pending.State = oauth.AuthRequestExpired
	requireRequestByGitHubState(t, s, pending)
	requireRequestByCode(t, s, approved)
	requireRequestByCode(t, s, redeemed)

	require.ErrorIs(t, s.ConsentAuthRequest(ctx, consentAgain(pending, oauth.HashToken("another state")), now), oauth.ErrUnknownAuthRequest)
	require.ErrorIs(t, s.ApproveAuthRequest(ctx, approvalOf(pending, c, now), stillAlive), oauth.ErrUnknownAuthRequest)
	_, err = s.RedeemAuthRequest(ctx, redemptionOf(approved.ID, approved.ID, now))
	require.NoError(t, err, "an approved request is still redeemed")
}

func approveStoresTheConnectionAndTheCodeInOneStep(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	c := connectionAt(1, now)
	r := consentedRequest(t, s, "req-1", now)
	_, err := s.Connection(ctx, c.ID)
	require.ErrorIs(t, err, oauth.ErrUnknownConnection)

	a := approvalOf(r, c, now.Add(time.Minute))
	require.NoError(t, s.ApproveAuthRequest(ctx, a, func(oauth.Connection) bool {
		t.Error("alive ran for a connection that the store does not hold")
		return true
	}))
	requireConnectionIs(t, s, c)
	want := r
	want.State, want.CodeHash, want.ConnectionID, want.ExpiresAt = oauth.AuthRequestApproved, a.Code, c.ID, a.ExpiresAt
	requireRequestByCode(t, s, want)
	requireRequestByGitHubState(t, s, want)
}

func approveRefusesAnOutdatedGitHubStateAnExpiredRequestAndASecondApproval(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	c := connectionAt(1, now)
	r := consentedRequest(t, s, "req-1", now)
	current := oauth.HashToken("github state of the second Continue")
	require.NoError(t, s.ConsentAuthRequest(ctx, consentAgain(r, current), now))
	r.GitHubStateHash = current
	other := consentedRequest(t, s, "req-2", now)
	expired := consentedRequest(t, s, "req-3", now)
	require.NoError(t, s.ExpireAuthRequest(ctx, expired.ID))
	expired.State = oauth.AuthRequestExpired

	attempt := oauth.HashToken("code of an attempt")
	approval := func(id string, githubState oauth.TokenHash, at time.Time) oauth.Approval {
		return oauth.Approval{RequestID: id, GitHubState: githubState, Connection: c, Code: attempt, Now: at, ExpiresAt: at.Add(time.Minute)}
	}
	for name, a := range map[string]oauth.Approval{
		"an outdated GitHub state":        approval(r.ID, githubStateOf(r.ID), now),
		"another request's GitHub state":  approval(r.ID, githubStateOf(expired.ID), now),
		"a request at its expiry":         approval(r.ID, current, r.ExpiresAt),
		"a request after its expiry":      approval(r.ID, current, r.ExpiresAt.Add(time.Hour)),
		"an expired request":              approval(expired.ID, githubStateOf(expired.ID), now),
		"an unknown ID":                   approval("req-unknown", current, now),
		"another ID with the right state": approval(other.ID, current, now),
	} {
		require.ErrorIs(t, s.ApproveAuthRequest(ctx, a, stillAlive), oauth.ErrUnknownAuthRequest, name)
		requireNoRequestByCode(t, s, attempt, name)
		_, err := s.Connection(ctx, c.ID)
		require.ErrorIs(t, err, oauth.ErrUnknownConnection, name)
	}
	requireRequestByGitHubState(t, s, r)
	requireRequestByGitHubState(t, s, other)
	requireRequestByGitHubState(t, s, expired)

	first := approval(r.ID, current, r.ExpiresAt.Add(-time.Microsecond))
	require.NoError(t, s.ApproveAuthRequest(ctx, first, stillAlive))
	second := approval(r.ID, current, now)
	second.Code, second.Connection = codeOf("a second approval"), connectionAt(2, now)
	require.ErrorIs(t, s.ApproveAuthRequest(ctx, second, stillAlive), oauth.ErrUnknownAuthRequest)
	requireNoRequestByCode(t, s, second.Code)
	_, err := s.Connection(ctx, second.Connection.ID)
	require.ErrorIs(t, err, oauth.ErrUnknownConnection)
	r.State, r.CodeHash, r.ConnectionID, r.ExpiresAt = oauth.AuthRequestApproved, first.Code, c.ID, first.ExpiresAt
	requireRequestByCode(t, s, r)
}

func aRefusedApprovalLeavesTheConnectionAsItWas(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	first := connectionAt(1, now)
	session := newSession(t, s, "req-0", first, now)
	held := approvedRequest(t, s, "req-1", first, now)
	again := consentedRequest(t, s, "req-2", now)
	current := oauth.HashToken("github state of the second Continue")
	require.NoError(t, s.ConsentAuthRequest(ctx, consentAgain(again, current), now))
	again.GitHubStateHash = current
	newer := connectionAt(1, now.Add(time.Hour))
	newer.Login = "renamed"

	outdated := approvalOf(again, newer, now.Add(time.Minute))
	require.ErrorIs(t, s.ApproveAuthRequest(ctx, outdated, revoked), oauth.ErrUnknownAuthRequest)
	requireConnectionIs(t, s, first, "a refused approval")

	takenCode := outdated
	takenCode.GitHubState, takenCode.Code = current, held.CodeHash
	require.Error(t, s.ApproveAuthRequest(ctx, takenCode, revoked))
	requireConnectionIs(t, s, first, "an approval that fails on its code")
	requireRequestByCode(t, s, held)
	requireRequestByGitHubState(t, s, again)
	requireSessionIs(t, s, session)

	stranger := outdated
	stranger.Connection = connectionAt(2, now)
	require.ErrorIs(t, s.ApproveAuthRequest(ctx, stranger, revoked), oauth.ErrUnknownAuthRequest)
	_, err := s.Connection(ctx, stranger.Connection.ID)
	require.ErrorIs(t, err, oauth.ErrUnknownConnection, "a refused approval makes no connection")
}

func approveReplacesTheTokensOfAUserWhoLogsInAgain(t *testing.T, s oauth.Store) {
	now := loginNow()
	first := connectionAt(1, now)
	session := newSession(t, s, "req-1", first, now)
	second := connectionAt(1, now.Add(time.Hour))
	second.Login, second.KeyID = "renamed", "key-2"
	again := approvedRequest(t, s, "req-2", second, now.Add(time.Hour))

	requireConnectionIs(t, s, second)
	requireRequestByCode(t, s, again)
	require.Equal(t, first.ID, again.ConnectionID, "a user has one connection however often they log in")
	requireSessionIs(t, s, session)
}

// A login that replaces the tokens of a connection whose authorization
// GitHub no longer takes ends, in the same step, the sessions of the
// connection and the codes of it that no client redeemed yet.
func approveEndsWhatTheReplacedConnectionServesUnlessItIsAlive(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	first := connectionAt(1, now)
	session := newSession(t, s, "req-1", first, now)
	unredeemed := approvedRequest(t, s, "req-2", first, now)
	other := newSession(t, s, "req-3", connectionAt(2, now), now)
	approvedAt := func(r oauth.AuthRequest, a oauth.Approval) oauth.AuthRequest {
		r.State, r.CodeHash, r.ConnectionID, r.ExpiresAt = oauth.AuthRequestApproved, a.Code, a.Connection.ID, a.ExpiresAt
		return r
	}
	codeEnded := func(r oauth.AuthRequest) {
		t.Helper()
		requireNoRequestByCode(t, s, r.CodeHash, "a code of the replaced connection")
		r.State, r.CodeHash, r.ConnectionID = oauth.AuthRequestExpired, oauth.TokenHash{}, ""
		requireRequestByGitHubState(t, s, r)
	}

	kept := consentedRequest(t, s, "req-4", now)
	keptApproval := approvalOf(kept, connectionAt(1, now.Add(time.Minute)), now.Add(time.Minute))
	var seen oauth.Connection
	require.NoError(t, s.ApproveAuthRequest(ctx, keptApproval, func(current oauth.Connection) bool {
		seen = current
		return true
	}))
	requireSameConnection(t, first, seen)
	requireSessionIs(t, s, session)
	requireRequestByCode(t, s, unredeemed)
	kept = approvedAt(kept, keptApproval)

	ending := consentedRequest(t, s, "req-5", now)
	endingApproval := approvalOf(ending, connectionAt(1, now.Add(2*time.Minute)), now.Add(2*time.Minute))
	require.NoError(t, s.ApproveAuthRequest(ctx, endingApproval, func(current oauth.Connection) bool {
		seen = current
		return false
	}))
	requireSameConnection(t, keptApproval.Connection, seen)
	requireConnectionIs(t, s, endingApproval.Connection)
	session.EndedAt = endingApproval.Now
	requireSessionIs(t, s, session)
	requireSessionIs(t, s, other)
	codeEnded(unredeemed)
	codeEnded(kept)
	requireRequestByCode(t, s, approvedAt(ending, endingApproval))
	_, err := s.RedeemAuthRequest(ctx, redemptionOf(ending.ID, ending.ID, endingApproval.Now))
	require.NoError(t, err, "the code of the approval that ended the others is redeemed")
}

func redeemMakesTheSessionAndItsFirstAccessTokenFromTheRequest(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	c := connectionAt(1, now)
	r := authRequestAt("req-1", now)
	r.ClientID, r.ClientName = "https://other-agent.example.com/client.json", "Other Agent"
	r.Resource, r.Scopes = "https://faucet.example.com/mcp", []faucet.Scope{faucet.ScopeEventsRead}
	require.NoError(t, s.ConsentAuthRequest(ctx, consentOf(r), now))
	require.NoError(t, s.ApproveAuthRequest(ctx, approvalOf(r, c, now), stillAlive))

	red := redemptionOf(r.ID, "first", now.Add(30*time.Second))
	session, err := s.RedeemAuthRequest(ctx, red)
	require.NoError(t, err)
	want := oauth.Session{
		ID:           r.ID,
		ClientID:     r.ClientID,
		ClientName:   r.ClientName,
		ConnectionID: c.ID,
		Resource:     r.Resource,
		Scopes:       []faucet.Scope{faucet.ScopeEventsRead},
		RefreshedAt:  red.Now,
	}
	requireSameSession(t, want, session)
	require.True(t, session.Live())
	requireSessionIs(t, s, want)
	got, _, err := s.SessionByRefreshToken(ctx, red.Refresh)
	require.NoError(t, err)
	requireSameSession(t, want, got)
	requireRefreshToken(t, s, oauth.RefreshToken{Hash: red.Refresh, SessionID: r.ID, IssuedAt: red.Now})

	token, err := s.AccessToken(ctx, red.Access)
	require.NoError(t, err)
	requireSameAccessToken(t, oauth.AccessToken{
		Hash:        red.Access,
		ExpiresAt:   red.AccessExpiresAt,
		Scopes:      []faucet.Scope{faucet.ScopeEventsRead},
		Resource:    r.Resource,
		SubjectKind: oauth.SubjectUser,
		Subject:     r.ID,
	}, token)
	redeemed, err := s.AuthRequestByCode(ctx, codeOf(r.ID))
	require.NoError(t, err)
	require.Equal(t, oauth.AuthRequestRedeemed, redeemed.State)
}

func redeemRefusesAnExpiredCodeAnUnknownCodeAndASecondUse(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	r := approvedRequest(t, s, "req-1", connectionAt(1, now), now)

	for name, red := range map[string]oauth.Redemption{
		"at its expiry":    redemptionOf(r.ID, "attempt", r.ExpiresAt),
		"after its expiry": redemptionOf(r.ID, "attempt", r.ExpiresAt.Add(time.Hour)),
		"an unknown code":  redemptionOf("req-unknown", "attempt", now),
	} {
		_, err := s.RedeemAuthRequest(ctx, red)
		require.ErrorIs(t, err, oauth.ErrUnknownAuthRequest, name)
		requireNoTokens(t, s, red.Refresh, red.Access, name)
		_, err = s.Session(ctx, r.ID)
		require.ErrorIs(t, err, oauth.ErrUnknownSession, name)
	}
	requireRequestByCode(t, s, r)

	first, err := s.RedeemAuthRequest(ctx, redemptionOf(r.ID, "first", r.ExpiresAt.Add(-time.Microsecond)))
	require.NoError(t, err)
	second := redemptionOf(r.ID, "second", r.ExpiresAt.Add(-time.Microsecond))
	_, err = s.RedeemAuthRequest(ctx, second)
	require.ErrorIs(t, err, oauth.ErrUnknownAuthRequest)
	requireNoTokens(t, s, second.Refresh, second.Access)
	requireSessionIs(t, s, first)
}

func onlyOneOfManyRedemptionsAtOnceMakesASession(t *testing.T, s oauth.Store) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	now := loginNow()
	r := approvedRequest(t, s, "req-1", connectionAt(1, now), now)

	const callers = 16
	reds := make([]oauth.Redemption, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		reds[i] = redemptionOf(r.ID, fmt.Sprintf("caller %d", i), now)
		wg.Go(func() {
			<-start
			_, errs[i] = s.RedeemAuthRequest(ctx, reds[i])
		})
	}
	close(start)
	wg.Wait()

	winner := -1
	for i, err := range errs {
		if err == nil {
			require.Equal(t, -1, winner, "callers %d and %d both made a session", winner, i)
			winner = i
			continue
		}
		require.ErrorIs(t, err, oauth.ErrUnknownAuthRequest, "caller %d", i)
		requireNoTokens(t, s, reds[i].Refresh, reds[i].Access, "caller %d", i)
	}
	require.NotEqual(t, -1, winner, "one caller makes the session")
	requireRefreshToken(t, s, oauth.RefreshToken{Hash: reds[winner].Refresh, SessionID: r.ID, IssuedAt: now})
}

// A request without a code goes once it expired. One with a code goes when
// it expired before the time, and its session lives on.
func purgeDeletesTheRequestsWithoutACodeAtExpiryAndTheOthersBeforeTheTime(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	old := now.Add(-time.Hour)
	c := connectionAt(1, old)
	atExpiry := consentedRequest(t, s, "req-at-expiry", now.Add(-10*time.Minute))
	given := consentedRequest(t, s, "req-given-up", old)
	require.NoError(t, s.ExpireAuthRequest(ctx, given.ID))
	live := consentedRequest(t, s, "req-live", now.Add(-10*time.Minute+time.Microsecond))
	oldRedeemed := approvedRequest(t, s, "req-old-redeemed", c, old)
	session, err := s.RedeemAuthRequest(ctx, redemptionOf(oldRedeemed.ID, oldRedeemed.ID, old))
	require.NoError(t, err)
	boundary := approvedRequest(t, s, "req-boundary", c, old.Add(5*time.Minute))
	codesBefore := boundary.ExpiresAt

	n, err := s.DeleteExpiredAuthRequests(ctx, now, codesBefore)
	require.NoError(t, err)
	require.EqualValues(t, 3, n)
	requireNoRequestByGitHubState(t, s, atExpiry.GitHubStateHash, "a request without a code at its expiry")
	requireNoRequestByGitHubState(t, s, given.GitHubStateHash, "an expired request")
	requireNoRequestByCode(t, s, oldRedeemed.CodeHash, "a request with a code that expired before the time")
	requireRequestByGitHubState(t, s, live)
	requireRequestByCode(t, s, boundary)

	requireSessionIs(t, s, session)
	_, err = s.RotateSession(ctx, rotationOf(refreshOf(session.ID), "after the purge", now))
	require.NoError(t, err, "the session of a purged request lives on")

	n, err = s.DeleteExpiredAuthRequests(ctx, now, codesBefore)
	require.NoError(t, err)
	require.Zero(t, n)
}

func consentApproveRedeemAndRotateValidateInput(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	c := connectionAt(1, now)
	r := consentedRequest(t, s, "req-1", now)

	require.Error(t, s.ConsentAuthRequest(ctx, consentAgain(r, oauth.TokenHash{}), now), "a consent with no GitHub state")
	requireRequestByGitHubState(t, s, r)

	for name, mutate := range map[string]func(*oauth.Approval){
		"no request ID":                  func(a *oauth.Approval) { a.RequestID = "" },
		"no GitHub state":                func(a *oauth.Approval) { a.GitHubState = oauth.TokenHash{} },
		"no code":                        func(a *oauth.Approval) { a.Code = oauth.TokenHash{} },
		"no time":                        func(a *oauth.Approval) { a.Now = time.Time{} },
		"a code that expires at once":    func(a *oauth.Approval) { a.ExpiresAt = a.Now },
		"a connection that is not valid": func(a *oauth.Approval) { a.Connection.Login = "" },
		"a connection with another ID":   func(a *oauth.Approval) { a.Connection.ID = oauth.ConnectionID(oauth.ProviderGitHub, 2) },
	} {
		a := approvalOf(r, c, now)
		mutate(&a)
		require.Error(t, s.ApproveAuthRequest(ctx, a, stillAlive), name)
		requireRequestByGitHubState(t, s, r)
		_, err := s.Connection(ctx, c.ID)
		require.ErrorIs(t, err, oauth.ErrUnknownConnection, name)
	}
	r = approvedRequest(t, s, "req-2", c, now)

	for name, mutate := range map[string]func(*oauth.Redemption){
		"no code":                              func(red *oauth.Redemption) { red.Code = oauth.TokenHash{} },
		"no refresh hash":                      func(red *oauth.Redemption) { red.Refresh = oauth.TokenHash{} },
		"no access hash":                       func(red *oauth.Redemption) { red.Access = oauth.TokenHash{} },
		"no time":                              func(red *oauth.Redemption) { red.Now = time.Time{} },
		"an access token that expires at once": func(red *oauth.Redemption) { red.AccessExpiresAt = red.Now },
	} {
		red := redemptionOf(r.ID, r.ID, now)
		mutate(&red)
		_, err := s.RedeemAuthRequest(ctx, red)
		require.Error(t, err, name)
		requireNoTokens(t, s, refreshOf(r.ID), accessOf(r.ID), name)
		requireRequestByCode(t, s, r)
	}
	session, err := s.RedeemAuthRequest(ctx, redemptionOf(r.ID, r.ID, now))
	require.NoError(t, err)

	for name, mutate := range map[string]func(*oauth.Rotation){
		"no presented hash":                    func(rot *oauth.Rotation) { rot.Presented = oauth.TokenHash{} },
		"no next hash":                         func(rot *oauth.Rotation) { rot.Next = oauth.TokenHash{} },
		"the presented hash as the next one":   func(rot *oauth.Rotation) { rot.Next = rot.Presented },
		"no access hash":                       func(rot *oauth.Rotation) { rot.Access = oauth.TokenHash{} },
		"no scope":                             func(rot *oauth.Rotation) { rot.Scopes = nil },
		"an unknown scope":                     func(rot *oauth.Rotation) { rot.Scopes = []faucet.Scope{"events:everything"} },
		"no time":                              func(rot *oauth.Rotation) { rot.Now = time.Time{} },
		"an access token that expires at once": func(rot *oauth.Rotation) { rot.AccessExpiresAt = rot.Now },
		"no start of the reuse grace":          func(rot *oauth.Rotation) { rot.UsedAfter = time.Time{} },
		"a reuse grace that starts later":      func(rot *oauth.Rotation) { rot.UsedAfter = rot.Now.Add(time.Microsecond) },
	} {
		rot := rotationOf(refreshOf(session.ID), "next", now)
		mutate(&rot)
		_, err := s.RotateSession(ctx, rot)
		require.Error(t, err, name)
		requireNoTokens(t, s, refreshOf("next"), accessOf("next"), name)
		requireSessionIs(t, s, session)
	}
}

func aHashThatAnotherRowHoldsIsRefusedAndChangesNothing(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	c := connectionAt(1, now)
	holder := newSession(t, s, "req-1", c, now)
	held, err := s.AuthRequestByCode(ctx, codeOf(holder.ID))
	require.NoError(t, err)
	require.Error(t, s.ConsentAuthRequest(ctx, consentAgain(authRequestAt("req-2", now), held.GitHubStateHash), now))
	requireRequestByGitHubState(t, s, held)
	r := consentedRequest(t, s, "req-2", now)

	a := approvalOf(r, c, now)
	a.Code = held.CodeHash
	require.Error(t, s.ApproveAuthRequest(ctx, a, stillAlive))
	requireRequestByCode(t, s, held)
	requireRequestByGitHubState(t, s, r)
	r = approve(t, s, r, c, now)

	for name, mutate := range map[string]func(*oauth.Redemption){
		"a refresh hash that a token holds": func(red *oauth.Redemption) { red.Refresh = refreshOf(holder.ID) },
		"an access hash that a token holds": func(red *oauth.Redemption) { red.Access = accessOf(holder.ID) },
	} {
		red := redemptionOf(r.ID, r.ID, now)
		mutate(&red)
		_, err := s.RedeemAuthRequest(ctx, red)
		require.Error(t, err, name)
		require.NotErrorIs(t, err, oauth.ErrUnknownAuthRequest, name)
		requireRequestByCode(t, s, r)
		_, err = s.Session(ctx, r.ID)
		require.ErrorIs(t, err, oauth.ErrUnknownSession, name)
		requireNoTokens(t, s, refreshOf(r.ID), accessOf(r.ID), name)
		requireSessionIs(t, s, holder)
	}
	session, err := s.RedeemAuthRequest(ctx, redemptionOf(r.ID, r.ID, now))
	require.NoError(t, err)

	for name, mutate := range map[string]func(*oauth.Rotation){
		"a refresh hash that a token holds": func(rot *oauth.Rotation) { rot.Next = refreshOf(holder.ID) },
		"an access hash that a token holds": func(rot *oauth.Rotation) { rot.Access = accessOf(holder.ID) },
	} {
		rot := rotationOf(refreshOf(session.ID), "next", now)
		mutate(&rot)
		_, err := s.RotateSession(ctx, rot)
		require.Error(t, err, name)
		require.NotErrorIs(t, err, oauth.ErrUnknownSession, name)
		requireSessionIs(t, s, session)
		requireNoTokens(t, s, refreshOf("next"), accessOf("next"), name)
		requireSessionIs(t, s, holder)
	}
}

func aRefreshTokenIsFoundUntilItsSessionGoes(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	session := newSession(t, s, "req-1", connectionAt(1, now), now)
	first := rotationOf(refreshOf(session.ID), "second", now.Add(time.Minute))
	rotated, err := s.RotateSession(ctx, first)
	require.NoError(t, err)

	requireRefreshToken(t, s, oauth.RefreshToken{Hash: first.Presented, SessionID: session.ID, IssuedAt: now, UsedAt: first.Now})
	requireRefreshToken(t, s, oauth.RefreshToken{Hash: first.Next, SessionID: session.ID, IssuedAt: first.Now})
	got, _, err := s.SessionByRefreshToken(ctx, first.Presented)
	require.NoError(t, err)
	requireSameSession(t, rotated, got)
	for _, unknown := range []oauth.TokenHash{{}, refreshOf("unknown"), accessOf("req-1")} {
		requireNoRefreshToken(t, s, unknown)
	}

	second := rotationOf(first.Next, "third", now.Add(2*time.Minute))
	_, err = s.RotateSession(ctx, second)
	require.NoError(t, err)
	requireRefreshToken(t, s, oauth.RefreshToken{Hash: first.Presented, SessionID: session.ID, IssuedAt: now, UsedAt: first.Now}, "a token used before the grace")
	requireRefreshToken(t, s, oauth.RefreshToken{Hash: first.Next, SessionID: session.ID, IssuedAt: first.Now, UsedAt: second.Now})

	require.NoError(t, s.EndSession(ctx, session.ID, now.Add(3*time.Minute)))
	ended, _, err := s.SessionByRefreshToken(ctx, second.Next)
	require.NoError(t, err, "an ended session is found")
	require.False(t, ended.Live())
}

func rotateTradesTheTokenAndStoresTheNextAccessToken(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	session := newSession(t, s, "req-1", connectionAt(1, now), now)
	rot := rotationOf(refreshOf(session.ID), "next", now.Add(time.Hour))

	got, err := s.RotateSession(ctx, rot)
	require.NoError(t, err)
	want := session
	want.RefreshedAt = rot.Now
	requireSameSession(t, want, got)
	requireSessionIs(t, s, want)
	requireRefreshToken(t, s, oauth.RefreshToken{Hash: rot.Presented, SessionID: session.ID, IssuedAt: now, UsedAt: rot.Now})
	requireRefreshToken(t, s, oauth.RefreshToken{Hash: rot.Next, SessionID: session.ID, IssuedAt: rot.Now})

	token, err := s.AccessToken(ctx, rot.Access)
	require.NoError(t, err)
	requireSameAccessToken(t, oauth.AccessToken{
		Hash:        rot.Access,
		ExpiresAt:   rot.AccessExpiresAt,
		Scopes:      []faucet.Scope{faucet.ScopeEventsRead},
		Resource:    session.Resource,
		SubjectKind: oauth.SubjectUser,
		Subject:     session.ID,
	}, token)
}

// A used token trades again while its first use came after the start of the
// grace, and keeps the time of that first use.
func rotateTradesAUsedTokenAgainWithinItsGrace(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	session := newSession(t, s, "req-1", connectionAt(1, now), now)
	first := rotationOf(refreshOf(session.ID), "first", now.Add(time.Minute))
	_, err := s.RotateSession(ctx, first)
	require.NoError(t, err)

	again := rotationOf(first.Presented, "again", first.Now.Add(10*time.Second))
	again.UsedAfter = first.Now.Add(-time.Microsecond)
	got, err := s.RotateSession(ctx, again)
	require.NoError(t, err)
	want := session
	want.RefreshedAt = again.Now
	requireSameSession(t, want, got)
	requireRefreshToken(t, s, oauth.RefreshToken{Hash: first.Presented, SessionID: session.ID, IssuedAt: now, UsedAt: first.Now})
	requireRefreshToken(t, s, oauth.RefreshToken{Hash: first.Next, SessionID: session.ID, IssuedAt: first.Now})
	requireRefreshToken(t, s, oauth.RefreshToken{Hash: again.Next, SessionID: session.ID, IssuedAt: again.Now})
	_, err = s.AccessToken(ctx, again.Access)
	require.NoError(t, err)

	late := rotationOf(first.Presented, "late", first.Now.Add(reuseGrace))
	late.UsedAfter = first.Now
	_, err = s.RotateSession(ctx, late)
	require.ErrorIs(t, err, oauth.ErrUnknownSession, "a first use at the start of the grace")
	requireNoTokens(t, s, late.Next, late.Access)
	requireSessionIs(t, s, want)
}

// A rotation at a time before the session's refreshed time, as by an
// instance whose clock lags, trades the token and keeps the later time, so
// that it makes no older access check fresh again.
func rotateAtAnEarlierTimeKeepsTheLaterRefreshedTime(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	session := newSession(t, s, "req-1", connectionAt(1, now), now)
	first := rotationOf(refreshOf(session.ID), "first", now.Add(time.Hour))
	_, err := s.RotateSession(ctx, first)
	require.NoError(t, err)

	lagging := rotationOf(first.Next, "lagging", first.Now.Add(-time.Minute))
	got, err := s.RotateSession(ctx, lagging)
	require.NoError(t, err)
	want := session
	want.RefreshedAt = first.Now
	requireSameSession(t, want, got)
	requireSessionIs(t, s, want)
	requireRefreshToken(t, s, oauth.RefreshToken{Hash: lagging.Next, SessionID: session.ID, IssuedAt: lagging.Now})
}

// A rotation retires the unused tokens of its session that the client no
// longer holds: those issued at or before the start of the grace. A retired
// token trades no more. The rotation keeps every used token as it is, and
// the tokens of other sessions.
func rotateRetiresTheUnusedTokensThatNoClientStillHolds(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	c := connectionAt(1, now)
	session := newSession(t, s, "req-1", c, now)
	other := newSession(t, s, "req-2", c, now)
	t1 := now.Add(time.Minute)
	first := rotationOf(refreshOf(session.ID), "first", t1)
	twin := rotationOf(first.Presented, "twin", t1)
	second := rotationOf(first.Next, "second", t1.Add(time.Second))
	late := rotationOf(first.Presented, "late", second.Now)
	third := rotationOf(first.Presented, "third", t1.Add(2*time.Second))
	fourth := rotationOf(second.Next, "fourth", t1.Add(3*time.Second))
	for _, rot := range []oauth.Rotation{first, twin, second, late, third, fourth} {
		_, err := s.RotateSession(ctx, rot)
		require.NoError(t, err)
	}

	kept := rotationOf(fourth.Next, "kept", t1.Add(15*time.Minute))
	kept.UsedAfter = second.Now
	_, err := s.RotateSession(ctx, kept)
	require.NoError(t, err)
	for name, want := range map[string]oauth.RefreshToken{
		"used before the grace":                       {Hash: first.Presented, IssuedAt: now, UsedAt: t1},
		"unused and issued before the grace":          {Hash: twin.Next, IssuedAt: t1, RetiredAt: kept.Now},
		"used at the start of the grace":              {Hash: first.Next, IssuedAt: t1, UsedAt: second.Now},
		"unused and issued at the start of the grace": {Hash: late.Next, IssuedAt: second.Now, RetiredAt: kept.Now},
		"used after the start of the grace":           {Hash: second.Next, IssuedAt: second.Now, UsedAt: fourth.Now},
		"unused and issued after the start":           {Hash: third.Next, IssuedAt: third.Now},
		"the presented token":                         {Hash: kept.Presented, IssuedAt: fourth.Now, UsedAt: kept.Now},
		"the next token":                              {Hash: kept.Next, IssuedAt: kept.Now},
	} {
		want.SessionID = session.ID
		requireRefreshToken(t, s, want, name)
	}
	requireRefreshToken(t, s, oauth.RefreshToken{Hash: refreshOf(other.ID), SessionID: other.ID, IssuedAt: now}, "a token of another session")

	afterRetirement := rotationOf(twin.Next, "after retirement", kept.Now.Add(time.Second))
	_, err = s.RotateSession(ctx, afterRetirement)
	require.ErrorIs(t, err, oauth.ErrUnknownSession, "a retired token")
	requireNoTokens(t, s, afterRetirement.Next, afterRetirement.Access)
	requireRefreshToken(t, s, oauth.RefreshToken{Hash: twin.Next, SessionID: session.ID, IssuedAt: t1, RetiredAt: kept.Now})
}

func rotateRefusesATokenUsedBeforeItsGraceAnEndedSessionAndAnUnknownToken(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	c := connectionAt(1, now)
	live := newSession(t, s, "req-1", c, now)
	rotated, err := s.RotateSession(ctx, rotationOf(refreshOf(live.ID), "second", now.Add(time.Minute)))
	require.NoError(t, err)
	ended := newSession(t, s, "req-2", c, now)
	require.NoError(t, s.EndSession(ctx, ended.ID, now.Add(time.Minute)))
	ended.EndedAt = now.Add(time.Minute)

	for name, presented := range map[string]oauth.TokenHash{
		"a token used before its grace": refreshOf(live.ID),
		"an ended session":              refreshOf(ended.ID),
		"an unknown token":              refreshOf("unknown"),
	} {
		rot := rotationOf(presented, "attempt", now.Add(2*time.Minute))
		_, err := s.RotateSession(ctx, rot)
		require.ErrorIs(t, err, oauth.ErrUnknownSession, name)
		requireNoTokens(t, s, rot.Next, rot.Access, name)
		requireSessionIs(t, s, rotated)
		requireSessionIs(t, s, ended)
	}
	requireRefreshToken(t, s, oauth.RefreshToken{Hash: refreshOf(live.ID), SessionID: live.ID, IssuedAt: now, UsedAt: now.Add(time.Minute)})
}

// A refresh can narrow the scopes of the next access token and never widen
// them past what the user approved.
func rotateRefusesAnAccessTokenWithAScopeThatTheSessionLacks(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	c := connectionAt(1, now)
	r := authRequestAt("req-1", now)
	r.Scopes = []faucet.Scope{faucet.ScopeEventsRead}
	r = consentOf(r)
	require.NoError(t, s.ConsentAuthRequest(ctx, r, now))
	approve(t, s, r, c, now)
	session, err := s.RedeemAuthRequest(ctx, redemptionOf(r.ID, r.ID, now))
	require.NoError(t, err)
	require.Equal(t, []faucet.Scope{faucet.ScopeEventsRead}, session.Scopes)

	wider := rotationOf(refreshOf(session.ID), "wider", now.Add(time.Minute))
	wider.Scopes = []faucet.Scope{faucet.ScopeEventsRead, faucet.ScopeEventsSubscribe}
	_, err = s.RotateSession(ctx, wider)
	require.ErrorIs(t, err, oauth.ErrUnknownSession)
	requireNoTokens(t, s, wider.Next, wider.Access, "a wider rotation")
	requireSessionIs(t, s, session)
	requireRefreshToken(t, s, oauth.RefreshToken{Hash: wider.Presented, SessionID: session.ID, IssuedAt: now}, "the token stays unused")

	same := rotationOf(refreshOf(session.ID), "same", now.Add(time.Minute))
	_, err = s.RotateSession(ctx, same)
	require.NoError(t, err)
}

// Rotations of one token at once all trade it, as the parallel requests of
// one client do, and the token keeps the time of its first use.
func everyRotationOfOneTokenAtOnceTradesIt(t *testing.T, s oauth.Store) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	now := loginNow()
	session := newSession(t, s, "req-1", connectionAt(1, now), now)

	const callers = 16
	rots := make([]oauth.Rotation, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		rots[i] = rotationOf(refreshOf(session.ID), fmt.Sprintf("caller %d", i), now.Add(time.Minute+time.Duration(i)*time.Millisecond))
		wg.Go(func() {
			<-start
			_, errs[i] = s.RotateSession(ctx, rots[i])
		})
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "caller %d", i)
		requireRefreshToken(t, s, oauth.RefreshToken{Hash: rots[i].Next, SessionID: session.ID, IssuedAt: rots[i].Now}, "caller %d", i)
		_, err = s.AccessToken(ctx, rots[i].Access)
		require.NoError(t, err, "caller %d", i)
	}
	_, token, err := s.SessionByRefreshToken(ctx, refreshOf(session.ID))
	require.NoError(t, err)
	require.True(t, slices.ContainsFunc(rots, func(r oauth.Rotation) bool { return r.Now.Equal(token.UsedAt) }), "the first use keeps its time")
}

// Two unused tokens of one session that come at once, long after a client
// got both, have two holders, and the rotation of each retires the other.
// Exactly one of the two trades its token. The other answers
// ErrUnknownSession and changes nothing.
func siblingTokensRotatedAtOnceTradeOrAreRefused(t *testing.T, s oauth.Store) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	now := loginNow()
	c := connectionAt(1, now)
	const sessions = 16
	rots := make([][2]oauth.Rotation, sessions)
	for i := range sessions {
		id := fmt.Sprintf("req-%d", i)
		newSession(t, s, id, c, now)
		for j, name := range []string{"older", "younger"} {
			_, err := s.RotateSession(ctx, rotationOf(refreshOf(id), fmt.Sprintf("%s %s", id, name), now.Add(time.Minute)))
			require.NoError(t, err)
			rots[i][j] = rotationOf(refreshOf(fmt.Sprintf("%s %s", id, name)), fmt.Sprintf("%s %s next", id, name), now.Add(time.Hour))
		}
	}

	errs := make([][2]error, sessions)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range sessions {
		for j := range 2 {
			wg.Go(func() {
				<-start
				_, errs[i][j] = s.RotateSession(ctx, rots[i][j])
			})
		}
	}
	close(start)
	wg.Wait()

	for i := range sessions {
		traded := 0
		for j, err := range errs[i] {
			if err == nil {
				traded++
				continue
			}
			require.ErrorIs(t, err, oauth.ErrUnknownSession, "session %d, rotation %d", i, j)
			requireNoTokens(t, s, rots[i][j].Next, rots[i][j].Access, "session %d, rotation %d", i, j)
		}
		require.Equal(t, 1, traded, "session %d: exactly one of the two trades", i)
	}
}

func endSessionKeepsTheFirstTime(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	c := connectionAt(1, now)
	ended := newSession(t, s, "req-1", c, now)
	other := newSession(t, s, "req-2", c, now)

	require.NoError(t, s.EndSession(ctx, ended.ID, now.Add(time.Minute)))
	require.NoError(t, s.EndSession(ctx, ended.ID, now.Add(time.Hour)))
	require.NoError(t, s.EndSession(ctx, "req-unknown", now))
	ended.EndedAt = now.Add(time.Minute)
	requireSessionIs(t, s, ended)
	requireSessionIs(t, s, other)
}

func deleteEndedSessionsDeletesTheEndedAndTheIdleWithTheirTokens(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	idleSince := now.Add(-180 * 24 * time.Hour)
	c := connectionAt(1, idleSince)
	live := newSession(t, s, "req-live", c, now)
	ended := newSession(t, s, "req-ended", c, now)
	require.NoError(t, s.EndSession(ctx, ended.ID, now))
	idle := newSession(t, s, "req-idle", c, idleSince)
	almostIdle := newSession(t, s, "req-almost-idle", c, idleSince.Add(time.Microsecond))
	endsLater := newSession(t, s, "req-ends-later", c, now)
	require.NoError(t, s.EndSession(ctx, endsLater.ID, now.Add(time.Microsecond)))
	endsLater.EndedAt = now.Add(time.Microsecond)

	n, err := s.DeleteEndedSessions(ctx, now, idleSince)
	require.NoError(t, err)
	require.EqualValues(t, 2, n)
	for _, gone := range []oauth.Session{ended, idle} {
		_, err := s.Session(ctx, gone.ID)
		require.ErrorIs(t, err, oauth.ErrUnknownSession, gone.ID)
		requireNoRefreshToken(t, s, refreshOf(gone.ID), gone.ID)
	}
	for _, kept := range []oauth.Session{live, almostIdle, endsLater} {
		requireSessionIs(t, s, kept)
		requireRefreshToken(t, s, oauth.RefreshToken{Hash: refreshOf(kept.ID), SessionID: kept.ID, IssuedAt: kept.RefreshedAt}, kept.ID)
	}
	n, err = s.DeleteEndedSessions(ctx, now, idleSince)
	require.NoError(t, err)
	require.Zero(t, n)
}

// A connection goes once no session and no request names it, and it was
// updated before the time.
func deleteUnusedConnectionsDeletesTheOnesThatNothingNames(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	before := now.Add(-24 * time.Hour)
	old := before.Add(-time.Hour)
	withSession := connectionAt(1, old)
	newSession(t, s, "req-1", withSession, old)
	withRequest := connectionAt(2, old)
	approvedRequest(t, s, "req-2", withRequest, now)
	unused := connectionAt(3, old)
	newSession(t, s, "req-3", unused, old)
	recent := connectionAt(4, before)
	newSession(t, s, "req-4", recent, old)
	for _, id := range []string{"req-3", "req-4"} {
		require.NoError(t, s.EndSession(ctx, id, old))
	}
	_, err := s.DeleteEndedSessions(ctx, now, now.Add(-180*24*time.Hour))
	require.NoError(t, err)
	_, err = s.DeleteExpiredAuthRequests(ctx, now, before)
	require.NoError(t, err)

	n, err := s.DeleteUnusedConnections(ctx, before)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	_, err = s.Connection(ctx, unused.ID)
	require.ErrorIs(t, err, oauth.ErrUnknownConnection)
	for name, kept := range map[string]oauth.Connection{"a session names it": withSession, "a request names it": withRequest, "updated at the time": recent} {
		requireConnectionIs(t, s, kept, name)
	}
	n, err = s.DeleteUnusedConnections(ctx, before)
	require.NoError(t, err)
	require.Zero(t, n)
}

func connectionOfAnUnknownIDIsUnknown(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	approvedRequest(t, s, "req-1", connectionAt(1, now), now)
	unknown := connectionAt(2, now).ID

	_, err := s.Connection(ctx, unknown)
	require.ErrorIs(t, err, oauth.ErrUnknownConnection)
	called := false
	_, err = s.RefreshConnection(ctx, unknown, now, func(oauth.Connection) (oauth.ConnectionChange, error) {
		called = true
		return oauth.ConnectionChange{Kind: oauth.KeepConnection}, nil
	})
	require.ErrorIs(t, err, oauth.ErrUnknownConnection)
	require.False(t, called, "refresh is not called for an unknown connection")
}

func refreshConnectionKeepsReplacesOrEndsTheSessions(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	c := connectionAt(1, now)
	session := newSession(t, s, "req-1", c, now)
	replacement := connectionAt(1, now.Add(time.Hour))
	replacement.Login = "renamed"

	for _, step := range []struct {
		change oauth.ConnectionChange
		before oauth.Connection
		after  oauth.Connection
		ended  bool
	}{
		{oauth.ConnectionChange{Kind: oauth.KeepConnection}, c, c, false},
		{oauth.ConnectionChange{Kind: oauth.ReplaceConnection, Connection: replacement}, c, replacement, false},
		{oauth.ConnectionChange{Kind: oauth.EndConnectionSessions}, replacement, replacement, true},
	} {
		var seen oauth.Connection
		got, err := s.RefreshConnection(ctx, c.ID, now.Add(2*time.Hour), func(current oauth.Connection) (oauth.ConnectionChange, error) {
			seen = current
			return step.change, nil
		})
		require.NoError(t, err, "kind %d", step.change.Kind)
		requireSameConnection(t, step.before, seen)
		requireSameConnection(t, step.after, got)
		requireConnectionIs(t, s, step.after, "kind %d", step.change.Kind)
		want := session
		if step.ended {
			want.EndedAt = now.Add(2 * time.Hour)
		}
		requireSessionIs(t, s, want)
	}
}

func refreshConnectionEndsOnlyTheLiveSessionsOfItsConnection(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	mine, theirs := connectionAt(1, now), connectionAt(2, now)
	live := newSession(t, s, "req-1", mine, now)
	endedBefore := newSession(t, s, "req-2", mine, now)
	require.NoError(t, s.EndSession(ctx, endedBefore.ID, now.Add(time.Minute)))
	other := newSession(t, s, "req-3", theirs, now)

	_, err := s.RefreshConnection(ctx, mine.ID, now.Add(time.Hour), func(oauth.Connection) (oauth.ConnectionChange, error) {
		return oauth.ConnectionChange{Kind: oauth.EndConnectionSessions}, nil
	})
	require.NoError(t, err)
	live.EndedAt, endedBefore.EndedAt = now.Add(time.Hour), now.Add(time.Minute)
	requireSessionIs(t, s, live)
	requireSessionIs(t, s, endedBefore)
	requireSessionIs(t, s, other)
	_, err = s.RotateSession(ctx, rotationOf(refreshOf(live.ID), "after the end", now.Add(time.Hour)))
	require.ErrorIs(t, err, oauth.ErrUnknownSession, "an ended session rotates no more")
}

func refreshConnectionChangesNothingWhenItsCallbackFailsOrItsChangeIsNotValid(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	c := connectionAt(1, now)
	session := newSession(t, s, "req-1", c, now)
	replacement := connectionAt(1, now.Add(time.Hour))
	invalid := connectionAt(1, now.Add(time.Hour))
	invalid.Login = ""
	failure := errors.New("GitHub is down")

	for name, step := range map[string]struct {
		change oauth.ConnectionChange
		err    error
	}{
		"a failed replacement":            {oauth.ConnectionChange{Kind: oauth.ReplaceConnection, Connection: replacement}, failure},
		"a failed end":                    {oauth.ConnectionChange{Kind: oauth.EndConnectionSessions}, failure},
		"a replacement with another ID":   {oauth.ConnectionChange{Kind: oauth.ReplaceConnection, Connection: connectionAt(2, now)}, nil},
		"a replacement that is not valid": {oauth.ConnectionChange{Kind: oauth.ReplaceConnection, Connection: invalid}, nil},
		"an end with a connection":        {oauth.ConnectionChange{Kind: oauth.EndConnectionSessions, Connection: replacement}, nil},
		"a keep with a connection":        {oauth.ConnectionChange{Kind: oauth.KeepConnection, Connection: replacement}, nil},
		"an unknown kind":                 {oauth.ConnectionChange{}, nil},
	} {
		_, err := s.RefreshConnection(ctx, c.ID, now.Add(time.Hour), func(oauth.Connection) (oauth.ConnectionChange, error) {
			return step.change, step.err
		})
		require.Error(t, err, name)
		if step.err != nil {
			require.ErrorIs(t, err, step.err, name)
		}
		requireConnectionIs(t, s, c, name)
		requireSessionIs(t, s, session)
		_, err = s.Connection(ctx, connectionAt(2, now).ID)
		require.ErrorIs(t, err, oauth.ErrUnknownConnection, name)
	}
}

// heldRefresh starts a refresh of the connection with this ID whose callback
// returns change once release is called, and returns when the callback
// runs. done gets the refresh's error. The end of the test releases it too.
func heldRefresh(ctx context.Context, t *testing.T, s oauth.Store, id string, now time.Time, change oauth.ConnectionChange) (release func(), done <-chan error) {
	t.Helper()
	started, released := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(released) }) }
	t.Cleanup(release)
	errc := make(chan error, 1)
	go func() {
		_, err := s.RefreshConnection(ctx, id, now, func(oauth.Connection) (oauth.ConnectionChange, error) {
			close(started)
			<-released
			return change, nil
		})
		errc <- err
	}()
	select {
	case <-started:
	case err := <-errc:
		t.Fatalf("the refresh returned (%v) before its callback ran", err)
	}
	return release, errc
}

func aSecondRefreshOfAConnectionWaitsAndSeesWhatTheFirstStored(t *testing.T, s oauth.Store) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	now := loginNow()
	c := connectionAt(1, now)
	approvedRequest(t, s, "req-1", c, now)
	replacement := connectionAt(1, now.Add(time.Hour))
	replacement.Login = "refreshed"

	release, firstDone := heldRefresh(ctx, t, s, c.ID, now, oauth.ConnectionChange{Kind: oauth.ReplaceConnection, Connection: replacement})
	seen := make(chan oauth.Connection, 1)
	secondDone := make(chan error, 1)
	go func() {
		_, err := s.RefreshConnection(ctx, c.ID, now, func(current oauth.Connection) (oauth.ConnectionChange, error) {
			seen <- current
			return oauth.ConnectionChange{Kind: oauth.KeepConnection}, nil
		})
		secondDone <- err
	}()

	select {
	case <-seen:
		t.Fatal("the second refresh ran its callback while the first one held the connection")
	case <-time.After(holdFor):
	}
	release()
	require.NoError(t, <-firstDone)
	requireSameConnection(t, replacement, <-seen)
	require.NoError(t, <-secondDone)
}

func anApprovalWaitsForARefreshOfItsConnection(t *testing.T, s oauth.Store) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	now := loginNow()
	c := connectionAt(1, now)
	approvedRequest(t, s, "req-1", c, now)
	again := consentedRequest(t, s, "req-2", now)
	refreshed := connectionAt(1, now.Add(time.Hour))
	refreshed.Login = "refreshed"
	approved := connectionAt(1, now.Add(2*time.Hour))
	approved.Login = "approved"

	release, refreshDone := heldRefresh(ctx, t, s, c.ID, now, oauth.ConnectionChange{Kind: oauth.ReplaceConnection, Connection: refreshed})
	approveDone := make(chan error, 1)
	go func() { approveDone <- s.ApproveAuthRequest(ctx, approvalOf(again, approved, now), stillAlive) }()

	select {
	case err := <-approveDone:
		t.Fatalf("the approval returned (%v) while a refresh held its connection", err)
	case <-time.After(holdFor):
	}
	release()
	require.NoError(t, <-refreshDone)
	require.NoError(t, <-approveDone)
	requireConnectionIs(t, s, approved, "the approval stores its tokens after the refresh")
	got, err := s.AuthRequestByCode(ctx, codeOf(again.ID))
	require.NoError(t, err)
	require.Equal(t, oauth.AuthRequestApproved, got.State)
}

// An approval asks GitHub about the connection that it replaces while it
// holds it, so a refresh of the connection waits and then sees the tokens of
// the approval.
func aRefreshWaitsForTheCheckOfAnApproval(t *testing.T, s oauth.Store) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	now := loginNow()
	c := connectionAt(1, now)
	approvedRequest(t, s, "req-1", c, now)
	again := consentedRequest(t, s, "req-2", now)
	approved := connectionAt(1, now.Add(time.Hour))
	approved.Login = "approved"

	started, released := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }
	t.Cleanup(release)
	approveDone := make(chan error, 1)
	go func() {
		approveDone <- s.ApproveAuthRequest(ctx, approvalOf(again, approved, now), func(oauth.Connection) bool {
			close(started)
			<-released
			return true
		})
	}()
	select {
	case <-started:
	case err := <-approveDone:
		t.Fatalf("the approval returned (%v) before its check ran", err)
	}
	seen := make(chan oauth.Connection, 1)
	refreshDone := make(chan error, 1)
	go func() {
		_, err := s.RefreshConnection(ctx, c.ID, now, func(current oauth.Connection) (oauth.ConnectionChange, error) {
			seen <- current
			return oauth.ConnectionChange{Kind: oauth.KeepConnection}, nil
		})
		refreshDone <- err
	}()

	select {
	case <-seen:
		t.Fatal("a refresh ran its callback while an approval checked the connection")
	case <-time.After(holdFor):
	}
	release()
	require.NoError(t, <-approveDone)
	requireSameConnection(t, approved, <-seen)
	require.NoError(t, <-refreshDone)
}

// A refresh calls GitHub while it holds its connection, so only a step that
// changes the connection may wait for it.
func stepsThatDoNotChangeTheConnectionDoNotWaitForItsRefresh(t *testing.T, s oauth.Store) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	now := loginNow()
	held, other := connectionAt(1, now), connectionAt(2, now)
	session := newSession(t, s, "req-1", held, now)
	toRedeem := approvedRequest(t, s, "req-2", held, now)
	approvedRequest(t, s, "req-3", other, now)
	again := consentedRequest(t, s, "req-4", now)

	release, refreshDone := heldRefresh(ctx, t, s, held.ID, now, oauth.ConnectionChange{Kind: oauth.KeepConnection})
	watchdog := time.AfterFunc(10*time.Second, release)
	requireConnectionIs(t, s, held)
	requireSessionIs(t, s, session)
	_, err := s.RotateSession(ctx, rotationOf(refreshOf(session.ID), "during the refresh", now))
	require.NoError(t, err)
	_, err = s.RedeemAuthRequest(ctx, redemptionOf(toRedeem.ID, toRedeem.ID, now))
	require.NoError(t, err)
	_, err = s.RefreshConnection(ctx, other.ID, now, func(oauth.Connection) (oauth.ConnectionChange, error) {
		return oauth.ConnectionChange{Kind: oauth.KeepConnection}, nil
	})
	require.NoError(t, err)
	require.NoError(t, s.ApproveAuthRequest(ctx, approvalOf(again, other, now), stillAlive))
	require.True(t, watchdog.Stop(), "a step waited for the refresh of a connection it does not change")
	release()
	require.NoError(t, <-refreshDone)
}

func loginRowsShareNoSliceWithTheirCaller(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	r, want := consentOf(authRequestAt("req-1", now)), consentOf(authRequestAt("req-1", now))
	require.NoError(t, s.ConsentAuthRequest(ctx, r, now))
	r.Scopes[0] = faucet.ScopeEventsSubscribe
	c, wantConnection := connectionAt(1, now), connectionAt(1, now)
	a := approvalOf(want, c, now)
	require.NoError(t, s.ApproveAuthRequest(ctx, a, stillAlive))
	c.AccessToken[0], c.RefreshToken[0] = 'X', 'X'
	want.State, want.GitHubStateHash, want.CodeHash, want.ConnectionID, want.ExpiresAt = oauth.AuthRequestApproved, a.GitHubState, a.Code, c.ID, a.ExpiresAt
	for _, read := range []func() (oauth.AuthRequest, error){
		func() (oauth.AuthRequest, error) { return s.AuthRequestByGitHubState(ctx, want.GitHubStateHash) },
		func() (oauth.AuthRequest, error) { return s.AuthRequestByCode(ctx, want.CodeHash) },
	} {
		got, err := read()
		require.NoError(t, err)
		requireSameAuthRequest(t, want, got)
		got.Scopes[0] = faucet.ScopeEventsSubscribe
		requireRequestByGitHubState(t, s, want)
		requireRequestByCode(t, s, want)
	}
	gotConnection, err := s.Connection(ctx, c.ID)
	require.NoError(t, err)
	requireSameConnection(t, wantConnection, gotConnection)
	gotConnection.AccessToken[0], gotConnection.RefreshToken[0] = 'X', 'X'
	requireConnectionIs(t, s, wantConnection)

	rot := rotationOf(refreshOf(want.ID), "next", now)
	wantSession := oauth.Session{
		ID: want.ID, ClientID: want.ClientID, ClientName: want.ClientName, ConnectionID: c.ID, Resource: want.Resource,
		Scopes: authRequestAt(want.ID, now).Scopes, RefreshedAt: now,
	}
	for _, step := range []func() (oauth.Session, error){
		func() (oauth.Session, error) { return s.RedeemAuthRequest(ctx, redemptionOf(want.ID, want.ID, now)) },
		func() (oauth.Session, error) { return s.RotateSession(ctx, rot) },
		func() (oauth.Session, error) { return s.Session(ctx, want.ID) },
		func() (oauth.Session, error) {
			session, _, err := s.SessionByRefreshToken(ctx, rot.Next)
			return session, err
		},
	} {
		got, err := step()
		require.NoError(t, err)
		require.Equal(t, wantSession.Scopes, got.Scopes)
		got.Scopes[0] = faucet.ScopeEventsSubscribe
	}
	rot.Scopes[0] = faucet.ScopeEventsSubscribe
	requireSessionIs(t, s, wantSession)
	stored, _, err := s.SessionByRefreshToken(ctx, rot.Next)
	require.NoError(t, err)
	requireSameSession(t, wantSession, stored)
	token, err := s.AccessToken(ctx, accessOf(want.ID))
	require.NoError(t, err)
	require.Equal(t, wantSession.Scopes, token.Scopes)
	token, err = s.AccessToken(ctx, rot.Access)
	require.NoError(t, err)
	require.Equal(t, []faucet.Scope{faucet.ScopeEventsRead}, token.Scopes)

	kept, err := s.RefreshConnection(ctx, c.ID, now, func(current oauth.Connection) (oauth.ConnectionChange, error) {
		current.AccessToken[0], current.RefreshToken[0] = 'X', 'X'
		return oauth.ConnectionChange{Kind: oauth.KeepConnection}, nil
	})
	require.NoError(t, err)
	requireSameConnection(t, wantConnection, kept)
	kept.AccessToken[0] = 'X'
	requireConnectionIs(t, s, wantConnection)
	replacement, wantReplacement := connectionAt(1, now.Add(time.Hour)), connectionAt(1, now.Add(time.Hour))
	replaced, err := s.RefreshConnection(ctx, c.ID, now, func(oauth.Connection) (oauth.ConnectionChange, error) {
		return oauth.ConnectionChange{Kind: oauth.ReplaceConnection, Connection: replacement}, nil
	})
	require.NoError(t, err)
	replacement.AccessToken[0], replaced.RefreshToken[0] = 'X', 'X'
	requireConnectionIs(t, s, wantReplacement)
}
