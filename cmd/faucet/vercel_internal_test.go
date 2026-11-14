package main

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

// Vercel Cron calls the paths of vercel.json. A path that the server does not
// serve fails every run with a 404 that nobody sees, and a route that no cron
// calls never runs.
func TestVercelCronsCallTheCronRoutesThatTheServerMounts(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("../../vercel.json")
	require.NoError(t, err)
	var config struct {
		Crons []struct{ Path, Schedule string } `json:"crons"`
	}
	require.NoError(t, json.Unmarshal(raw, &config))

	scheduled := map[string]string{}
	for _, cron := range config.Crons {
		scheduled[cron.Path] = cron.Schedule
	}
	require.Len(t, scheduled, len(config.Crons), "a path is scheduled twice")

	router := chi.NewRouter()
	cronRoutes{}.mount(router)
	var mounted []string
	require.NoError(t, chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		require.Equal(t, http.MethodGet, method, "Vercel Cron sends GET")
		mounted = append(mounted, route)
		return nil
	}))
	var paths []string
	for path := range scheduled {
		paths = append(paths, path)
	}
	sort.Strings(mounted)
	sort.Strings(paths)
	require.Equal(t, mounted, paths)

	// The README sizes the redelivery sweep for a run every 10 minutes.
	require.Equal(t, "*/10 * * * *", scheduled[redeliverPath])
}
