package oauth_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A refresh token that a rotation replaced ends its session when it comes
// again, since one of its two holders is not the client. Two processes that
// share a refresh token so log each other out (R14): the cost of rotation.
func TestARotatedOutRefreshTokenEndsItsSession(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	first := h.redeem(h.approve("login-1", 1))
	second := h.refresh(first)
	require.NotEqual(t, first.refresh, second.refresh)
	require.NotEqual(t, first.access, second.access)

	h.clock.Advance(time.Minute)
	grantRequireRefused(t, h.post("", grantRefreshForm(first.refresh)), http.StatusBadRequest, "invalid_grant", "the rotated-out token")
	session := h.session("login-1")
	require.False(t, session.Live(), "the session ended")
	require.True(t, h.clock.Now().Equal(session.EndedAt))
	grantRequireRefused(t, h.post("", grantRefreshForm(second.refresh)), http.StatusBadRequest, "invalid_grant", "the current token of the ended session")
}
