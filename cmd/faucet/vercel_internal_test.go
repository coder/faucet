package main

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
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

// Vercel hands a request to the server only when a rewrite routes its path to
// the faucet service. Each source is read as a whole-path regular expression,
// which is what a path-to-regexp source of only groups means.
func TestVercelRewritesRouteTheMCPPathsToTheServer(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("../../vercel.json")
	require.NoError(t, err)
	var config struct {
		Rewrites []struct {
			Source      string `json:"source"`
			Destination struct {
				Service string `json:"service"`
			} `json:"destination"`
		} `json:"rewrites"`
	}
	require.NoError(t, json.Unmarshal(raw, &config))

	for _, path := range mcpPaths {
		var service string
		for _, rewrite := range config.Rewrites {
			if regexp.MustCompile("^" + rewrite.Source + "$").MatchString(path) {
				service = rewrite.Destination.Service
				break
			}
		}
		require.Equal(t, "faucet", service, "the rewrite of %s", path)
	}
}
