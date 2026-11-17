package redelivery_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet/redelivery"
)

// The README promises numbers about redelivery. They come from the constants
// of the package and from the schedule that the README recommends, so a change
// to a constant fails here until the README says the new number.
func TestReadmeStatesTheRedeliveryCapacityThatTheConstantsGive(t *testing.T) {
	t.Parallel()

	const (
		schedule    = 10 * time.Minute
		perPage     = 100
		asksPerHour = int(time.Hour / schedule)
	)
	pages := redelivery.DefaultMaxPagesPerRun
	perRun := pages * perPage
	covered := schedule + redelivery.ReadOverlap
	// The README rounds the sweep's capacity down to a hundred, for a run that starts late.
	keepsUpWith := steadyCapacity(perRun, covered) / 100 * 100
	requestsPerHour := redelivery.DefaultMaxAsksPerRun * asksPerHour

	offsets := redelivery.AskOffsets()
	minutes := make([]string, len(offsets))
	for i, offset := range offsets {
		minutes[i] = fmt.Sprint(int(offset / time.Minute))
	}
	askList := strings.Join(minutes[:len(minutes)-1], ", ") + " and " + minutes[len(minutes)-1] + " minutes"

	readme, err := os.ReadFile("../../README.md")
	require.NoError(t, err)
	text := string(readme)

	for _, want := range []string{
		fmt.Sprintf("at most %d pages of %d attempts, which is %s attempts", pages, perPage, thousands(perRun)),
		fmt.Sprintf("sends at most %d requests", redelivery.DefaultMaxAsksPerRun),
		fmt.Sprintf("a run holds at most %s attempts of its window", thousands(perRun-2)),
		fmt.Sprintf("That is %s attempts an hour at best", thousands(steadyCapacity(perRun, covered))),
		fmt.Sprintf("keeps up with %s attempts an hour", thousands(keepsUpWith)),
		fmt.Sprintf("sends at most %d requests an hour", requestsPerHour),
		fmt.Sprintf("A delivery is asked for %d times, at %s after", redelivery.MaxAsksPerDelivery, askList),
		fmt.Sprintf("%d minute overlap", int(redelivery.ReadOverlap/time.Minute)),
		fmt.Sprintf("GitHub keeps a delivery for %d hours", int(redelivery.GitHubDeliveryRetention/time.Hour)),
		fmt.Sprintf("lookback of %d hours", int(redelivery.StartLookback/time.Hour)),
		fmt.Sprintf("ends within %d minutes of the first run", int(offsets[len(offsets)-1]/time.Minute)),
		"*/10 * * * *",
	} {
		require.Contains(t, text, want)
	}
	require.Greater(t, keepsUpWith, 4200, "the capacity must cover the busiest hook that the simulations model")
}

func TestReadmeStatesTheDateWindowThatTheConstantsGive(t *testing.T) {
	t.Parallel()

	slack, margin := redelivery.DateSlack, redelivery.DateMargin
	marginAndOverlap := hoursAndMinutes(margin + redelivery.ReadOverlap)
	quietFor := hoursAndMinutes(redelivery.GitHubDeliveryRetention - slack - margin - redelivery.ReadOverlap)

	readme, err := os.ReadFile("../../README.md")
	require.NoError(t, err)
	text := string(readme)

	for _, want := range []string{
		fmt.Sprintf("a slack of %d hour", int(slack/time.Hour)),
		fmt.Sprintf("a margin of %d hour", int(margin/time.Hour)),
		fmt.Sprintf("starts %s before the `Date` of the run before it", marginAndOverlap),
		fmt.Sprintf("two runs are about %s apart", quietFor),
		fmt.Sprintf("more than %s ahead of GitHub's list", marginAndOverlap),
		fmt.Sprintf("more than %d hour behind the clock that deletes", int(slack/time.Hour)),
		fmt.Sprintf("when it runs the margin of %d hours ahead, up to about %d hours", int(margin/time.Hour), int((margin+slack)/time.Hour)),
		fmt.Sprintf("the last run that read the list was about %s before it", quietFor),
	} {
		require.Contains(t, text, want)
	}
}

func hoursAndMinutes(d time.Duration) string {
	return fmt.Sprintf("%d hours and %d minutes", int(d/time.Hour), int(d%time.Hour/time.Minute))
}

func thousands(n int) string {
	return fmt.Sprintf("%d,%03d", n/1000, n%1000)
}
