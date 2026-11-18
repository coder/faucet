package oauth_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet/internal/pgtest"
	"github.com/coder/faucet/faucet/oauth"
	"github.com/coder/faucet/faucet/pgstore"
)

// usertokensStores are the stores that the tests of a GitHub refresh run
// on: the row lock of Postgres is what keeps two refreshes apart there.
var usertokensStores = map[string]func(t *testing.T) oauth.Store{
	"fake":     func(*testing.T) oauth.Store { return newFakeStore() },
	"postgres": func(t *testing.T) oauth.Store { return pgstore.New(pgtest.NewDB(t, pgstore.Migrate)) },
}

// usertokensSoon is a time at which the GitHub tokens that a harness gave
// out at its start expire within the refresh grant's ten minutes.
const usertokensSoon = 8*time.Hour - 5*time.Minute

// usertokensRefreshAll presents the refresh token of each session at once.
func usertokensRefreshAll(t *testing.T, h *grantHarness, sessions []grantSession) []tokenReply {
	t.Helper()
	start := make(chan struct{})
	replies := make([]tokenReply, len(sessions))
	errs := make([]error, len(sessions))
	var wg sync.WaitGroup
	for i, s := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			replies[i], errs[i] = h.send("", grantRefreshForm(s.refresh))
		}()
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	return replies
}

// Several agents of one user refresh at once while the user's GitHub token
// is about to expire. GitHub's refresh tokens work once, so a second refresh
// with the old one is refused, which ends every session of the user. With
// one GitHub refresh per connection at a time, which reads the pair again
// under the lock, GitHub sees one refresh and every session stays (R2).
func TestSessionsOfOneUserThatRefreshAtOnceRefreshTheGitHubTokenOnce(t *testing.T) {
	t.Parallel()
	for name, newStore := range usertokensStores {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newGrantHarness(t, newStore(t))
			var sessions []grantSession
			for _, id := range []string{"login-1", "login-2", "login-3"} {
				sessions = append(sessions, h.redeem(h.approve(id, 1)))
			}
			h.clock.Advance(usertokensSoon)

			replies := usertokensRefreshAll(t, h, sessions)
			refreshes, _ := h.github.counts()
			require.Equal(t, 1, refreshes, "GitHub saw one refresh")
			for i, s := range sessions {
				next := grantTokens(t, s.id, replies[i])
				require.NotEqual(t, s.refresh, next.refresh, s.id)
				require.NotEqual(t, s.access, next.access, s.id)
				session := h.session(s.id)
				require.True(t, session.Live(), s.id)
				require.Equal(t, oauth.HashToken(next.refresh), session.RefreshHash, s.id)
			}
			c, err := h.store.Connection(context.Background(), oauth.ConnectionID(oauth.ProviderGitHub, 1))
			require.NoError(t, err)
			require.True(t, h.clock.Now().Add(8*time.Hour).Equal(c.AccessExpiresAt), "the connection holds GitHub's new pair")
		})
	}
}

// GitHub refusing the refresh token of a connection ends the sessions of
// that connection, and of no other (R2).
func TestGitHubRefusingARefreshTokenEndsTheSessionsOfThatUserOnly(t *testing.T) {
	t.Parallel()
	for name, newStore := range usertokensStores {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newGrantHarness(t, newStore(t))
			first := h.redeem(h.approve("login-a1", 1))
			h.redeem(h.approve("login-a2", 1))
			other := h.redeem(h.approve("login-b", 2))
			h.github.revoke(1)
			h.clock.Advance(usertokensSoon)

			grantRequireRefused(t, h.post("", grantRefreshForm(first.refresh)), http.StatusBadRequest, "invalid_grant", "a refused GitHub refresh")
			require.False(t, h.session("login-a1").Live())
			require.False(t, h.session("login-a2").Live())
			require.True(t, h.session("login-b").Live())
			h.refresh(other)
		})
	}
}
