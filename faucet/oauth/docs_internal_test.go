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
	hours := func(d time.Duration) int { return int(d / time.Hour) }
	days := func(d time.Duration) int { return int(d / (24 * time.Hour)) }
	require.Equal(t, 24*time.Hour, authRequestRetention, "the docs say \"a day\"")
	require.Equal(t, 24*time.Hour, accessCheckRetention, "the docs say \"a day\"")
	require.InDelta(t, 0.5, recheckShare, 0, "the docs say \"half of what is left\"")
	require.Equal(t, time.Second, minWorkloadTokenLifetime, "the docs say \"at least a second away\"")
	require.Equal(t, time.Minute, workloadClockLeeway, "the docs say \"at most a minute ahead\"")
	require.Equal(t, time.Minute, keyFetchInterval, "the docs say \"once a minute\", \"for a minute\" and \"after a minute\"")

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
		{"docs/security.md", "Superblink Events keeps the failure of a fetch for %d minute", []any{minutes(failedFetchAge)}},
		{"docs/browser-login.md", "It keeps the failure of a fetch for %d minute", []any{minutes(failedFetchAge)}},
		{"docs/service-accounts.md", "an fct_ access token for %d minutes", []any{minutes(accessTokenLifetime)}},
		{"docs/browser-login.md", "an access token for %d minutes", []any{minutes(accessTokenLifetime)}},
		{"docs/browser-login.md", "an fct_ access token for %d minutes", []any{minutes(accessTokenLifetime)}},
		{"docs/browser-login.md", "| Its SHA-256. | %d minutes.", []any{minutes(accessTokenLifetime)}},
		{"docs/browser-login.md", "The page works for %d minutes.", []any{minutes(authRequestLifetime)}},
		{"docs/browser-login.md", "and wait at least %d minutes after the deploy", []any{minutes(authRequestLifetime)}},
		{"docs/browser-login.md", "visible and in focus for %d milliseconds", []any{milliseconds(continueDelay)}},
		{"docs/browser-login.md", "trade the code for Superblink Events' tokens within %d seconds", []any{seconds(codeLifetime)}},
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
		{"docs/security.md", "asks GitHub again about the %d repositories that the user used last", []any{recheckLimit}},
		{"docs/security.md", "to keep calling once its %d-minute access token expires", []any{minutes(accessTokenLifetime)}},
		{"docs/security.md", "and the start of its wait **see a revocation within %d minutes**", []any{minutes(accessTokenLifetime)}},
		{"docs/browser-login.md", "asks GitHub again about the %d repositories that the user used last", []any{recheckLimit}},
		{"docs/browser-login.md", "It spends at most %d seconds on that, and half of what is left", []any{seconds(recheckTimeout)}},
		{"docs/browser-login.md", "lost access to is denied within %d minutes", []any{minutes(accessTokenLifetime)}},
		{"docs/browser-login.md", "An access token lasts %d minutes, so an agent that keeps working refreshes", []any{minutes(accessTokenLifetime)}},
		{"docs/upgrading.md", "asks GitHub again about up to %d repositories, which can make the refresh take up to %d seconds longer", []any{recheckLimit, seconds(recheckTimeout)}},
		{"docs/browser-login.md", "among those used in the last %d minutes", []any{minutes(accessTokenLifetime)}},
		{"docs/security.md", "among those used in the last %d minutes", []any{minutes(accessTokenLifetime)}},
		{"docs/browser-login.md", "or a day after a session last used it, when `/cron/purge` deletes it", nil},
		{"docs/upgrading.md", "`/cron/purge` deletes a stored check a day after a session last used it", nil},
		{"docs/upgrading.md", "and a stored denial %d minutes after its check", []any{minutes(accessDenialRetention)}},
		{"docs/browser-login.md", "A denial for %d minutes after its check, when `/cron/purge` deletes it.", []any{minutes(accessDenialRetention)}},
		{"docs/browser-login.md", "Superblink Events keeps a denial only as a marker for %d minutes", []any{minutes(accessDenialRetention)}},
		{"docs/browser-login.md", "After those %d minutes, every session that could take that pass", []any{minutes(accessDenialRetention)}},
		{"docs/security.md", "Superblink Events keeps a denial only as a marker for %d minutes", []any{minutes(accessDenialRetention)}},
		{"docs/service-accounts.md", "Its `exp` is at least a second away. It has an `iat` or an `nbf`, each at most a minute ahead of Superblink Events' clock, and it lives at most %d hours", []any{hours(maxSubjectTokenLifetime)}},
		{"docs/security.md", "Superblink Events takes no token that lives more than %d hours, from its `iat` or `nbf` to its `exp`.", []any{hours(maxSubjectTokenLifetime)}},
		{"docs/service-accounts.md", "Superblink Events takes no subject token that lives more than %d hours", []any{hours(maxSubjectTokenLifetime)}},
		{"docs/service-accounts.md", "--audience https://superblink-events.example.com --duration %dh`", []any{hours(maxSubjectTokenLifetime)}},
		{"docs/service-accounts.md", "Superblink Events' token lasts %d minutes, or until the platform's token expires", []any{minutes(accessTokenLifetime)}},
		{"docs/service-accounts.md", "works until it expires, %d minutes at most", []any{minutes(accessTokenLifetime)}},
		{"docs/service-accounts.md", "`Retry-After` gives the seconds until Superblink Events fetches again, %d at most.", []any{seconds(keyFetchInterval)}},
		{"docs/service-accounts.md", "also when its `kid` is missing from keys that Superblink Events fetched less than a minute ago", nil},
		{"docs/security.md", "It then mints its own token, which lasts %d minutes and never past the platform's token.", []any{minutes(accessTokenLifetime)}},
		{"docs/security.md", "must arrive within %d seconds together, and each must be %d KiB at most", []any{seconds(keyFetchTimeout), MaxKeyDocumentBytes >> 10}},
		{"docs/security.md", "Superblink Events keeps an issuer's keys for %d minutes**, so a key that the issuer drops stops working within %d minutes", []any{minutes(keySetLifetime), minutes(keySetLifetime)}},
		{"docs/security.md", "at most once a minute per issuer", nil},
		{"docs/security.md", "It keeps the keys of up to %d issuers.", []any{maxCachedKeySets}},
		{"docs/security.md", "keeps the keys that Superblink Events holds** until their %d minutes end", []any{minutes(keySetLifetime)}},
		{"docs/security.md", "Superblink Events keeps the failure for a minute, so while an issuer fails", nil},
		{"docs/security.md", "with `Retry-After` set to the seconds left of the minute", nil},
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
