package faucet

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"
)

// The docs state numbers that come from the constants of this package. A
// change to a constant fails here until the docs say the new number.
func TestDocsStateTheNumbersThatTheConstantsGive(t *testing.T) {
	t.Parallel()

	defaultTTL, err := parseTTL(nil)
	require.NoError(t, err)
	seconds := func(d time.Duration) int { return int(d / time.Second) }
	minutes := func(d time.Duration) int { return int(d / time.Minute) }
	kib := func(n int) int { return n >> 10 }

	for _, p := range []struct {
		file, format string
		args         []any
	}{
		{"README.md", "long-polls (up to %d seconds)", []any{maxWaitSeconds}},
		{"docs/deploy.md", "(a wait lasts up to %d seconds)", []any{maxWaitSeconds}},
		{"docs/deploy.md", "which a live subscriber does within %d minutes", []any{minutes(maxSubscriptionTTL)}},
		{"docs/security.md", "remembers a successful check for %d seconds per token", []any{seconds(accessCacheTTL)}},
		{"docs/security.md", "a revocation can take up to %d seconds to reach", []any{seconds(accessCacheTTL)}},
		{"docs/security.md", "can run for up to %d more seconds", []any{maxWaitSeconds}},
		{"docs/security.md", "One login can hold %d waits open at once", []any{defaultMaxWaitsPerLogin}},
		{"docs/security.md", "and times out after %d seconds", []any{seconds(callbackRequestTimeout)}},
		{"docs/security.md", "declares a body over %d KiB or that has headers over %d KiB", []any{kib(maxCallbackResponseBytes), kib(maxCallbackHeaderBytes)}},
		{"docs/events.md", "the cursor and the %d deliveries that a subscription may hold", []any{MaxPendingDeliveries}},
		{"docs/webhooks.md", "url not verified here in the last %d minutes", []any{minutes(callbackVerificationTTL)}},
		{"docs/webhooks.md", "verified that login and URL in the last %d minutes", []any{minutes(callbackVerificationTTL)}},
		{"docs/webhooks.md", "remembers a success for that login and URL for %d minutes", []any{minutes(callbackVerificationTTL)}},
		{"docs/webhooks.md", "clamped to between %d seconds and %d minutes (default %d minutes)", []any{seconds(minSubscriptionTTL), minutes(maxSubscriptionTTL), minutes(defaultTTL)}},
		{"docs/webhooks.md", "A login can hold %d live subscriptions", []any{maxLiveSubscriptions}},
		{"docs/webhooks.md", "a TLS error or a %d second timeout", []any{seconds(callbackRequestTimeout)}},
		{"docs/webhooks.md", "is retried after %s: at most %d attempts within %d minutes", []any{retrySchedule(deliveryBackoff[:]), MaxDeliveryAttempts, minutes(DeliveryWindow)}},
		{"docs/webhooks.md", "A subscription that holds %d unsent deliveries", []any{MaxPendingDeliveries}},
		{"docs/webhooks.md", "For a grace window of up to %d minutes", []any{minutes(rotationGrace)}},
		{"docs/webhooks.md", "answer `2xx` within %d seconds", []any{seconds(callbackRequestTimeout)}},
		{"docs/webhooks.md", "Each login has one per destination: %d at once, then one %s", []any{handshakeLoginBurst, refillText(handshakeLoginRefill)}},
		{"docs/webhooks.md", "Each destination has one for all logins: %d at once, then one %s", []any{handshakeHostBurst, refillText(handshakeHostRefill)}},
		{"docs/diagrams/source.html", "waits up to %d s<", []any{maxWaitSeconds}},
		{"docs/diagrams/source.html", ">within %d min<", []any{minutes(maxSubscriptionTTL)}},
	} {
		doc, err := os.ReadFile(filepath.Join("..", filepath.FromSlash(p.file)))
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
	require.NotEmpty(t, lead, "a pin starts with words")
	require.True(t, strings.Contains(doc, phrase), "%s does not say %q", file, phrase)
	require.Equal(t, strings.Count(doc, lead), strings.Count(doc, phrase), "%s: every %q must go on as %q", file, lead, phrase)
}

// retrySchedule writes the waits as docs/webhooks.md does, one unit per run
// of waits: "15 seconds, 1, 3 and 6 minutes".
func retrySchedule(waits []time.Duration) string {
	var runs []string
	for i := 0; i < len(waits); {
		unit, name := time.Second, "seconds"
		if waits[i] >= time.Minute {
			unit, name = time.Minute, "minutes"
		}
		var numbers []string
		for ; i < len(waits) && (waits[i] >= time.Minute) == (unit == time.Minute); i++ {
			numbers = append(numbers, strconv.Itoa(int(waits[i]/unit)))
		}
		last := len(numbers) - 1
		if last == 0 {
			runs = append(runs, numbers[0]+" "+name)
		} else {
			runs = append(runs, strings.Join(numbers[:last], ", ")+" and "+numbers[last]+" "+name)
		}
	}
	return strings.Join(runs, ", ")
}

// refillText writes a token bucket's refill as docs/webhooks.md does.
func refillText(every time.Duration) string {
	if every == time.Second {
		return "a second"
	}
	return fmt.Sprintf("every %d seconds", every/time.Second)
}

// docs/webhooks.md says which CallbackReason values a delivery records as
// lastError and which events/subscribe answers as data.reason. internal/docgen
// renders both columns from its registry, and this test checks them against
// what the code returns for each kind of failure.
func TestWebhookDocSaysWhereEachCallbackReasonComesFrom(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	transport := []error{
		ErrResponseTooLarge,
		xerrors.New("net/http: server response headers exceeded 16384 bytes; aborted"),
		context.DeadlineExceeded,
		http.ErrSchemeMismatch,
		&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}},
		xerrors.New("dial tcp: connect: connection refused"),
	}
	now := time.Now()
	claim := DeliveryClaim{Ref: DeliveryRef{Attempt: 1}, EnqueuedAt: now}

	lastError := map[CallbackReason]bool{}
	for _, err := range transport {
		lastError[classify(claim, now, 0, deliveryFailureOf(err)).finish.Failure] = true
	}
	for status := 100; status < 600; status++ {
		if failure := classify(claim, now, status, "").finish.Failure; failure != "" {
			lastError[failure] = true
		}
	}

	dataReason := map[CallbackReason]bool{}
	note := func(err error) {
		var cbErr *CallbackError
		if errors.As(err, &cbErr) {
			dataReason[cbErr.Reason] = true
		}
	}
	for _, err := range transport {
		note(transportFailure(ctx, err))
	}
	for status := 100; status < 600; status++ {
		note(checkAnswer(ctx, &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"challenge":"other"}`))}, "nonce"))
	}
	allowlist, err := NewCallbackAllowlist([]string{"https://allowed.example.com"}, false)
	require.NoError(t, err)
	note(allowlist.VerifyCallback(ctx, CallbackRequest{URL: "https://other.example.com/hook"}))

	doc, err := os.ReadFile(filepath.Join("..", "docs", "webhooks.md"))
	require.NoError(t, err)
	_, block, found := strings.Cut(string(doc), "<!-- BEGIN generated:callback-reasons -->\n")
	require.True(t, found)
	block, _, found = strings.Cut(block, "<!-- END generated:callback-reasons -->")
	require.True(t, found)
	rows := strings.Split(strings.TrimSpace(block), "\n")[2:]
	seen := map[CallbackReason]bool{}
	for reason := range lastError {
		seen[reason] = true
	}
	for reason := range dataReason {
		seen[reason] = true
	}
	require.Len(t, rows, len(seen), "a row for each value that the code gives")
	for _, row := range rows {
		cells := strings.Split(row, " | ")
		require.GreaterOrEqual(t, len(cells), 4, row)
		value := CallbackReason(strings.Trim(cells[0], "| `"))
		require.True(t, value.valid(), row)
		require.Equal(t, lastError[value], cells[1] == "yes", "%s: In lastError", value)
		require.Equal(t, dataReason[value], cells[2] == "yes", "%s: In data.reason", value)
	}
}
