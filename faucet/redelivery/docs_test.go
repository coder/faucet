package redelivery_test

import (
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet/redelivery"
)

// docs/redelivery.md promises numbers about redelivery. They come from the constants
// of the package and from the schedule that the doc recommends, so a change
// to a constant fails here until the doc says the new number.
func TestRedeliveryDocStatesTheRedeliveryCapacityThatTheConstantsGive(t *testing.T) {
	t.Parallel()

	const (
		schedule    = 10 * time.Minute
		asksPerHour = int(time.Hour / schedule)
	)
	pages, perPage := redelivery.DefaultMaxPagesPerRun, redelivery.DeliveriesPerPage
	perRun := pages * perPage
	covered := schedule + redelivery.ReadOverlap
	// The doc rounds the sweep's capacity down to a hundred, for a run that starts late.
	keepsUpWith := steadyCapacity(perRun, covered) / 100 * 100
	requestsPerHour := redelivery.DefaultMaxAsksPerRun * asksPerHour

	offsets := redelivery.AskOffsets()
	minutes := make([]string, len(offsets))
	for i, offset := range offsets {
		minutes[i] = fmt.Sprint(int(offset / time.Minute))
	}
	askList := strings.Join(minutes[:len(minutes)-1], ", ") + " and " + minutes[len(minutes)-1] + " minutes"

	doc, err := os.ReadFile("../../docs/redelivery.md")
	require.NoError(t, err)
	text := string(doc)

	overlap := int(redelivery.ReadOverlap / time.Minute)
	for _, p := range []pin{
		{"A run reads at most %d pages of %d attempts, which is %s attempts, and sends at most %d requests", []any{pages, perPage, thousands(perRun), redelivery.DefaultMaxAsksPerRun}},
		{"a run holds at most %s attempts of its window", []any{thousands(perRun - 2)}},
		{"That is %s attempts an hour at best", []any{thousands(steadyCapacity(perRun, covered))}},
		{"that starts late, the sweep keeps up with %s attempts an hour, and sends at most %d requests an hour", []any{thousands(keepsUpWith), requestsPerHour}},
		{"A delivery is asked for %d times, at %s after", []any{redelivery.MaxAsksPerDelivery, askList}},
		{"the previous run stopped, less a %d minute overlap", []any{overlap}},
		{"and less the %d minute overlap", []any{overlap}},
		{"holds more attempts in the %d minute overlap", []any{overlap}},
		{"GitHub keeps a delivery for %d hours", []any{int(redelivery.GitHubDeliveryRetention / time.Hour)}},
		{"its lookback of %d hours", []any{int(redelivery.StartLookback / time.Hour)}},
		{"ends within %d minutes of the first run", []any{int(offsets[len(offsets)-1] / time.Minute)}},
		{"a run covers %d minutes of the list (%d new minutes plus the %d minute overlap)", []any{int(covered / time.Minute), int(schedule / time.Minute), overlap}},
		{"because a run spends %s of its", []any{shareText(redelivery.ScanShare)}},
		{"A run reads the list for %s of that", []any{shareText(redelivery.ScanShare)}},
		{"asks until %s of the time has passed", []any{shareText(redelivery.AskStartShare)}},
		{"Schedule it every %d minutes (`*/%d * * * *`)", []any{int(schedule / time.Minute), int(schedule / time.Minute)}},
	} {
		requireDocSays(t, text, p)
	}
	require.Greater(t, keepsUpWith, 4200, "the capacity must cover the busiest hook that the simulations model")
}

func TestRedeliveryDocStatesTheDateWindowThatTheConstantsGive(t *testing.T) {
	t.Parallel()

	slack, margin := redelivery.DateSlack, redelivery.DateMargin
	marginAndOverlap := hoursAndMinutes(margin + redelivery.ReadOverlap)
	quietFor := hoursAndMinutes(redelivery.GitHubDeliveryRetention - slack - margin - redelivery.ReadOverlap)
	retention := int(redelivery.GitHubDeliveryRetention / time.Hour)

	doc, err := os.ReadFile("../../docs/redelivery.md")
	require.NoError(t, err)
	text := string(doc)

	for _, p := range []pin{
		{"plus a slack of %d hour", []any{int(slack / time.Hour)}},
		{"a gap starts there, less a margin of %d hours", []any{int(margin / time.Hour)}},
		{"After a silence a gap starts %s before the `Date` of the run before it", []any{marginAndOverlap}},
		{"two runs are about %s apart", []any{quietFor}},
		{"When a `Date` runs more than %s ahead of GitHub's list", []any{marginAndOverlap}},
		{"When a `Date` is more than %d hour behind the clock that deletes", []any{int(slack / time.Hour)}},
		{"when it runs the margin of %d hours ahead, up to about %d hours", []any{int(margin / time.Hour), int((margin + slack) / time.Hour)}},
		{"the last run that read the list was about %s before it", []any{quietFor}},
		{"Redelivery trusts a `Date` up to %d hours ahead of GitHub's list (the margin) and up to %d hour behind the clock that deletes", []any{int(margin / time.Hour), int(slack / time.Hour)}},
		{"for the part of GitHub's %d hours before its lookback", []any{retention}},
		{"says where GitHub's %d hours end", []any{retention}},
	} {
		requireDocSays(t, text, p)
	}
}

// pin is a phrase of docs/redelivery.md built from the constants.
type pin struct {
	format string
	args   []any
}

// requireDocSays fails unless the doc says the phrase, and says it wherever
// it says the words before the first number, so that a second copy of the
// phrase cannot keep an old number.
func requireDocSays(t *testing.T, doc string, p pin) {
	t.Helper()
	phrase := fmt.Sprintf(p.format, p.args...)
	lead, _, _ := strings.Cut(p.format, "%")
	require.NotEmpty(t, lead, "a pin starts with words")
	require.True(t, strings.Contains(doc, phrase), "docs/redelivery.md does not say %q", phrase)
	require.Equal(t, strings.Count(doc, lead), strings.Count(doc, phrase), "docs/redelivery.md: every %q must go on as %q", lead, phrase)
}

// shareText writes a share of a run's time as docs/redelivery.md does.
func shareText(share float64) string {
	if share == 0.5 {
		return "half"
	}
	return fmt.Sprintf("%d percent", int(math.Round(share*100)))
}

func hoursAndMinutes(d time.Duration) string {
	return fmt.Sprintf("%d hours and %d minutes", int(d/time.Hour), int(d%time.Hour/time.Minute))
}

func thousands(n int) string {
	return fmt.Sprintf("%d,%03d", n/1000, n%1000)
}
