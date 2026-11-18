package oauth

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A response that expired long before its Date is not kept, whatever its
// Age says: the lifetime of so old an Expires is the smallest Duration,
// which must not wrap around when Age is taken from it.
func TestAResponseThatExpiredBeforeItsDateIsNotKept(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	date := now.Format(http.TimeFormat)
	for name, header := range map[string]http.Header{
		"the first year, and an age":  {"Expires": {"Sat, 01 Jan 0001 00:00:00 GMT"}, "Date": {date}, "Age": {"1"}},
		"the first year, and no age":  {"Expires": {"Sat, 01 Jan 0001 00:00:00 GMT"}, "Date": {date}},
		"a minute before, and an age": {"Expires": {now.Add(-time.Minute).Format(http.TimeFormat)}, "Date": {date}, "Age": {"1"}},
		"an age past the max-age":     {"Cache-Control": {"max-age=60"}, "Age": {"61"}},
		"an age at the max-age":       {"Cache-Control": {"max-age=60"}, "Age": {"60"}},
		"the largest age":             {"Cache-Control": {"max-age=60"}, "Age": {"99999999999999999999"}},
	} {
		require.LessOrEqual(t, freshFor(header, now), time.Duration(0), name)
	}
	require.Equal(t, 59*time.Second, freshFor(http.Header{"Cache-Control": {"max-age=60"}, "Age": {"1"}}, now))
}
