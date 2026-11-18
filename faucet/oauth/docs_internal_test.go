package oauth

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The docs state numbers that come from the constants of this package. A
// change to a constant fails here until the docs say the new number.
func TestDocsStateTheNumbersThatTheConstantsGive(t *testing.T) {
	t.Parallel()

	milliseconds := func(d time.Duration) int { return int(d / time.Millisecond) }
	seconds := func(d time.Duration) int { return int(d / time.Second) }
	minutes := func(d time.Duration) int { return int(d / time.Minute) }
	days := func(d time.Duration) int { return int(d / (24 * time.Hour)) }
	require.Equal(t, 24*time.Hour, authRequestRetention, "the docs say \"a day\"")

	for _, p := range []struct {
		file, format string
		args         []any
	}{
		{"docs/security.md", "each valid for %d minutes on the one endpoint", []any{minutes(accessTokenLifetime)}},
		{"docs/security.md", "A code works once, for %d seconds.", []any{seconds(codeLifetime)}},
		{"docs/security.md", "visible and in focus for %d milliseconds", []any{milliseconds(continueDelay)}},
		{"docs/security.md", "A fetch must end within %d seconds, and the document must be %d KiB at most", []any{seconds(documentTimeout), maxDocumentBytes >> 10}},
		{"docs/security.md", "It keeps up to %d documents, each for as long as its cache headers let a shared cache keep it and %d minutes at most", []any{maxCachedDocuments, minutes(maxDocumentAge)}},
		{"docs/browser-login.md", "header lets a shared cache keep it, and %d minutes at most, so a change to a document shows within %d minutes", []any{minutes(maxDocumentAge), minutes(maxDocumentAge)}},
		{"docs/security.md", "Faucet keeps the failure of a fetch for %d minute", []any{minutes(failedFetchAge)}},
		{"docs/browser-login.md", "It keeps the failure of a fetch for %d minute", []any{minutes(failedFetchAge)}},
		{"docs/service-accounts.md", "an fct_ access token for %d minutes", []any{minutes(accessTokenLifetime)}},
		{"docs/browser-login.md", "an access token for %d minutes", []any{minutes(accessTokenLifetime)}},
		{"docs/browser-login.md", "an fct_ access token for %d minutes", []any{minutes(accessTokenLifetime)}},
		{"docs/browser-login.md", "| Its SHA-256. | %d minutes.", []any{minutes(accessTokenLifetime)}},
		{"docs/browser-login.md", "The page works for %d minutes.", []any{minutes(authRequestLifetime)}},
		{"docs/browser-login.md", "visible and in focus for %d milliseconds", []any{milliseconds(continueDelay)}},
		{"docs/browser-login.md", "trade the code for faucet's tokens within %d seconds", []any{seconds(codeLifetime)}},
		{"docs/browser-login.md", "and of the code. | %d minutes to finish in the browser, then %d seconds to redeem the code", []any{minutes(authRequestLifetime), seconds(codeLifetime)}},
		{"docs/browser-login.md", "deletes the request once it expired, or a day after for a request with a code", nil},
		{"docs/browser-login.md", "The refresh token went unused for %d days.", []any{days(sessionIdleLifetime)}},
		{"docs/browser-login.md", "A refresh token comes back %d seconds or more after its first use.", []any{seconds(refreshReuseGrace)}},
		{"docs/browser-login.md", "Within those %d seconds the token trades again", []any{seconds(refreshReuseGrace)}},
		{"docs/security.md", "A refresh token that comes back %d seconds or more after its first use ends its session", []any{seconds(refreshReuseGrace)}},
		{"docs/security.md", "Within those %d seconds it trades again", []any{seconds(refreshReuseGrace)}},
		{"docs/upgrading.md", "deletes login requests once they expired, or a day after for those with a code", nil},
		{"docs/upgrading.md", "sessions that ended or went unused for %d days", []any{days(sessionIdleLifetime)}},
		{"docs/browser-login.md", "| Until the session ends or goes unused for %d days.", []any{days(sessionIdleLifetime)}},
		{"docs/browser-login.md", "`/cron/purge` deletes it a day after its last update", nil},
		{"docs/security.md", "once the session ended or went unused for %d days", []any{days(sessionIdleLifetime)}},
		{"docs/security.md", "`/cron/purge` deletes it a day after its last update", nil},
	} {
		doc, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(p.file)))
		require.NoError(t, err)
		requireDocSays(t, p.file, string(doc), p.format, p.args...)
	}
}

// requireDocSays fails unless doc says the phrase that format and args give,
// each time it says the words before the first verb, so that a second copy
// with an old number fails too.
func requireDocSays(t *testing.T, file, doc, format string, args ...any) {
	t.Helper()
	phrase := fmt.Sprintf(format, args...)
	lead, _, _ := strings.Cut(format, "%")
	require.True(t, strings.Contains(doc, phrase), "%s does not say %q", file, phrase)
	require.Equal(t, strings.Count(doc, lead), strings.Count(doc, phrase), "%s: every %q must go on as %q", file, lead, phrase)
}
