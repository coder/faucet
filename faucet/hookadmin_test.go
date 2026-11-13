package faucet_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

const testHookToken = "ghp_hook0123456789abcdefghijklmnopqrstuvwx"

type recordedCall struct {
	method string
	path   string
	query  url.Values
	auth   string
}

type hookStub struct {
	*githubStub
	mu    sync.Mutex
	calls []recordedCall
}

func newHookStub(t *testing.T, handler http.HandlerFunc) *hookStub {
	t.Helper()
	stub := &hookStub{}
	stub.githubStub = newGitHubStub(t, func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		stub.calls = append(stub.calls, recordedCall{r.Method, r.URL.Path, r.URL.Query(), r.Header.Get("Authorization")})
		stub.mu.Unlock()
		handler(w, r)
	})
	return stub
}

func (s *hookStub) recorded() []recordedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedCall(nil), s.calls...)
}

func newHookAdmin(t *testing.T, stub *hookStub, target faucet.HookTarget) faucet.GitHubHookAdmin {
	t.Helper()
	admin, err := faucet.NewGitHubHookAdmin(stub.URL, testHookToken, target, stub.Client())
	require.NoError(t, err)
	return admin
}

var orgHook = faucet.HookTarget{Org: "octo-org", HookID: 9}

const deliveriesBody = `[
  {"id": 31, "guid": "guid-b", "delivered_at": "2026-11-13T10:05:00Z", "redelivery": false, "status_code": 500, "event": "workflow_run", "action": "completed", "repository_id": 1},
  {"id": 30, "guid": "guid-a", "delivered_at": "2026-11-13T10:00:00Z", "redelivery": true, "status_code": 200, "event": "ping", "action": null, "repository_id": null}
]`

func TestGitHubHookAdminListsDeliveriesNewestFirstWithACursor(t *testing.T) {
	t.Parallel()

	stub := newHookStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `<https://api.github.com/orgs/octo-org/hooks/9/deliveries?per_page=100&cursor=v1_29>; rel="next", `+
			`<https://api.github.com/orgs/octo-org/hooks/9/deliveries?per_page=100&cursor=v1_32>; rel="prev"`)
		reply(http.StatusOK, deliveriesBody)(w, nil)
	})
	admin := newHookAdmin(t, stub, orgHook)

	page, err := admin.ListDeliveries(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, []faucet.HookDelivery{
		{ID: 31, GUID: "guid-b", DeliveredAt: time.Date(2026, 11, 13, 10, 5, 0, 0, time.UTC), StatusCode: 500},
		{ID: 30, GUID: "guid-a", DeliveredAt: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC), StatusCode: 200, Redelivery: true},
	}, page.Deliveries)
	require.Equal(t, "v1_29", page.Next, "the next page is the rel=next cursor, not the rel=prev one")

	_, err = admin.ListDeliveries(context.Background(), "v1_29")
	require.NoError(t, err)

	calls := stub.recorded()
	require.Len(t, calls, 2)
	for _, c := range calls {
		require.Equal(t, http.MethodGet, c.method)
		require.Equal(t, "/orgs/octo-org/hooks/9/deliveries", c.path)
		require.Equal(t, "Bearer "+testHookToken, c.auth)
		require.Equal(t, "100", c.query.Get("per_page"))
	}
	require.False(t, calls[0].query.Has("cursor"), "the first page has no cursor")
	require.Equal(t, "v1_29", calls[1].query.Get("cursor"))
}

func TestGitHubHookAdminLastPageHasNoNext(t *testing.T) {
	t.Parallel()

	stub := newHookStub(t, reply(http.StatusOK, `[]`))
	page, err := newHookAdmin(t, stub, orgHook).ListDeliveries(context.Background(), "")
	require.NoError(t, err)
	require.Empty(t, page.Deliveries)
	require.Empty(t, page.Next)
}

func TestGitHubHookAdminUsesTheRepoEndpointsForARepoHook(t *testing.T) {
	t.Parallel()

	stub := newHookStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		reply(http.StatusOK, `[]`)(w, r)
	})
	admin := newHookAdmin(t, stub, faucet.HookTarget{Repo: "octo/the.repo", HookID: 12})

	_, err := admin.ListDeliveries(context.Background(), "")
	require.NoError(t, err)
	require.NoError(t, admin.Redeliver(context.Background(), 77))

	calls := stub.recorded()
	require.Equal(t, http.MethodGet, calls[0].method)
	require.Equal(t, "/repos/octo/the.repo/hooks/12/deliveries", calls[0].path)
	require.Equal(t, http.MethodPost, calls[1].method)
	require.Equal(t, "/repos/octo/the.repo/hooks/12/deliveries/77/attempts", calls[1].path)
}

func TestGitHubHookAdminTouchesOnlyTheTwoDeliveryEndpoints(t *testing.T) {
	t.Parallel()

	stub := newHookStub(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/orgs/octo-org/hooks/9/deliveries":
			reply(http.StatusOK, deliveriesBody)(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/orgs/octo-org/hooks/9/deliveries/31/attempts":
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	})
	admin := newHookAdmin(t, stub, orgHook)

	_, err := admin.ListDeliveries(context.Background(), "")
	require.NoError(t, err)
	require.NoError(t, admin.Redeliver(context.Background(), 31))

	var seen []string
	for _, c := range stub.recorded() {
		seen = append(seen, c.method+" "+c.path)
	}
	require.Equal(t, []string{
		"GET /orgs/octo-org/hooks/9/deliveries",
		"POST /orgs/octo-org/hooks/9/deliveries/31/attempts",
	}, seen, "a hook admin token can edit and delete the hook, so nothing else may be called")
}

func TestGitHubHookAdminSendsAHostileLinkAndCursorNowhereElse(t *testing.T) {
	t.Parallel()

	elsewhere := newGitHubStub(t, reply(http.StatusOK, `[]`))
	stub := newHookStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			w.Header().Set("Link", fmt.Sprintf(`<%s/steal?cursor=a%%20b%%26c%%3Dd>; rel="next"`, elsewhere.URL))
		}
		reply(http.StatusOK, `[]`)(w, r)
	})
	admin := newHookAdmin(t, stub, orgHook)

	first, err := admin.ListDeliveries(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, "a b&c=d", first.Next)
	_, err = admin.ListDeliveries(context.Background(), first.Next)
	require.NoError(t, err)

	require.Zero(t, elsewhere.requests.Load(), "the Link header names a URL, and faucet must not call it")
	second := stub.recorded()[1]
	require.Equal(t, "/orgs/octo-org/hooks/9/deliveries", second.path)
	require.Equal(t, url.Values{"per_page": {"100"}, "cursor": {"a b&c=d"}}, second.query, "the cursor is one escaped value, not new parameters")
}

func TestGitHubHookAdminRefusesAPaginationLinkItCannotUse(t *testing.T) {
	t.Parallel()

	for name, link := range map[string]string{
		"a next link without a cursor": `<https://api.github.com/x?page=2>; rel="next"`,
		"a cursor with a newline":      `<https://api.github.com/x?cursor=a%0Ab>; rel="next"`,
		"an enormous cursor":           `<https://api.github.com/x?cursor=` + strings.Repeat("a", 600) + `>; rel="next"`,
	} {
		stub := newHookStub(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Link", link)
			reply(http.StatusOK, `[]`)(w, r)
		})
		_, err := newHookAdmin(t, stub, orgHook).ListDeliveries(context.Background(), "")
		require.Error(t, err, name)
	}
}

func TestGitHubHookAdminRedeliverNeedsAccepted(t *testing.T) {
	t.Parallel()

	for status, wantErr := range map[int]bool{
		http.StatusAccepted:            false,
		http.StatusOK:                  true,
		http.StatusBadRequest:          true,
		http.StatusUnauthorized:        true,
		http.StatusNotFound:            true,
		http.StatusUnprocessableEntity: true,
		http.StatusInternalServerError: true,
	} {
		stub := newHookStub(t, reply(status, `{"message":"body that must not reach an error"}`))
		err := newHookAdmin(t, stub, orgHook).Redeliver(context.Background(), 5)
		if !wantErr {
			require.NoError(t, err, "status %d", status)
			continue
		}
		require.Error(t, err, "status %d", status)
		require.Contains(t, err.Error(), fmt.Sprint(status))
		require.NotContains(t, err.Error(), "must not reach", "an error carries the status, not the response body")
		requireNoHookToken(t, err.Error())
	}
}

func TestGitHubHookAdminListErrors(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"unauthorized":              {http.StatusUnauthorized, `{"message":"Bad credentials"}`},
		"not found":                 {http.StatusNotFound, `{"message":"Not Found"}`},
		"garbage":                   {http.StatusOK, `<html>`},
		"not an array":              {http.StatusOK, `{"id":1}`},
		"a delivery without an id":  {http.StatusOK, `[{"guid":"g","delivered_at":"2026-11-13T10:00:00Z","status_code":500}]`},
		"a delivery without a guid": {http.StatusOK, `[{"id":1,"delivered_at":"2026-11-13T10:00:00Z","status_code":500}]`},
		"a delivery without a time": {http.StatusOK, `[{"id":1,"guid":"g","status_code":500}]`},
		"a body over 1 MiB":         {http.StatusOK, `[` + strings.Repeat(`{"id":1,"guid":"g","delivered_at":"2026-11-13T10:00:00Z","status_code":500},`, 20000) + `{}]`},
	} {
		stub := newHookStub(t, reply(tc.status, tc.body))
		page, err := newHookAdmin(t, stub, orgHook).ListDeliveries(context.Background(), "")
		require.Error(t, err, name)
		require.Empty(t, page.Deliveries, name)
		requireNoHookToken(t, err.Error())
	}
}

func TestGitHubHookAdminSendsTheTokenOnlyToBaseURL(t *testing.T) {
	t.Parallel()

	for name, call := range map[string]func(faucet.GitHubHookAdmin) error{
		"list": func(a faucet.GitHubHookAdmin) error {
			_, err := a.ListDeliveries(context.Background(), "")
			return err
		},
		"redeliver": func(a faucet.GitHubHookAdmin) error { return a.Redeliver(context.Background(), 5) },
	} {
		t.Run("CrossHostRedirectIsRefused/"+name, func(t *testing.T) {
			t.Parallel()

			elsewhere := newGitHubStub(t, reply(http.StatusAccepted, `[]`))
			stub := newHookStub(t, func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
			})

			err := call(newHookAdmin(t, stub, orgHook))
			require.Error(t, err)
			requireNoHookToken(t, err.Error())
			require.Zero(t, elsewhere.requests.Load(), "the redirect target must not be called")
		})

		t.Run("SameHostRedirectIsFollowed/"+name, func(t *testing.T) {
			t.Parallel()

			stub := newHookStub(t, func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.URL.Path, "/moved") {
					http.Redirect(w, r, "/moved"+r.URL.Path, http.StatusTemporaryRedirect)
					return
				}
				if r.Method == http.MethodPost {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				reply(http.StatusOK, `[]`)(w, r)
			})

			require.NoError(t, call(newHookAdmin(t, stub, orgHook)))
			for _, c := range stub.recorded() {
				require.Equal(t, "Bearer "+testHookToken, c.auth)
			}
		})
	}
}

func TestGitHubHookAdminRefusesAnUnsafeBaseURL(t *testing.T) {
	t.Parallel()

	const userinfo = "hunter2-not-a-real-secret"
	for _, base := range []string{
		"", "api.github.com", "http://api.github.com", "https://user:" + userinfo + "@api.github.com",
		"https://api.github.com?x=1", "https://api.github.com#frag",
	} {
		_, err := faucet.NewGitHubHookAdmin(base, testHookToken, orgHook, nil)
		require.Error(t, err, base)
		require.NotContains(t, err.Error(), userinfo, "the error must not echo the URL")
	}
}

func TestNewGitHubHookAdminValidatesItsInputs(t *testing.T) {
	t.Parallel()

	good := func() (string, faucet.HookTarget) { return testHookToken, orgHook }
	for name, mutate := range map[string]func(token *string, target *faucet.HookTarget){
		"no token":                func(token *string, _ *faucet.HookTarget) { *token = "" },
		"a token with a space":    func(token *string, _ *faucet.HookTarget) { *token = "ghp_a b" },
		"a token with a newline":  func(token *string, _ *faucet.HookTarget) { *token = "ghp_a\nb" },
		"a token over 1024 bytes": func(token *string, _ *faucet.HookTarget) { *token = strings.Repeat("a", 1025) },
		"neither org nor repo":    func(_ *string, target *faucet.HookTarget) { target.Org = "" },
		"both org and repo":       func(_ *string, target *faucet.HookTarget) { target.Repo = "octo/repo" },
		"an org with a slash":     func(_ *string, target *faucet.HookTarget) { target.Org = "octo/org" },
		"an org that is dots":     func(_ *string, target *faucet.HookTarget) { target.Org = ".." },
		"a repo without an owner": func(_ *string, target *faucet.HookTarget) { target.Org, target.Repo = "", "repo" },
		"a repo that is dots":     func(_ *string, target *faucet.HookTarget) { target.Org, target.Repo = "", "octo/.." },
		"hook id zero":            func(_ *string, target *faucet.HookTarget) { target.HookID = 0 },
		"a negative hook id":      func(_ *string, target *faucet.HookTarget) { target.HookID = -3 },
	} {
		token, target := good()
		mutate(&token, &target)
		_, err := faucet.NewGitHubHookAdmin("https://api.github.com", token, target, nil)
		require.Error(t, err, name)
		if token != "" {
			require.NotContains(t, err.Error(), token, "%s: the error must not echo the token", name)
		}
	}
	_, err := faucet.NewGitHubHookAdmin("https://api.github.com", testHookToken, orgHook, nil)
	require.NoError(t, err)
	_, err = faucet.NewGitHubHookAdmin("https://api.github.com", testHookToken, faucet.HookTarget{Repo: "octo/repo", HookID: 1}, nil)
	require.NoError(t, err)
}

func TestGitHubHookAdminNeverPrintsItsToken(t *testing.T) {
	t.Parallel()

	admin, err := faucet.NewGitHubHookAdmin("https://api.github.com", testHookToken, orgHook, nil)
	require.NoError(t, err)

	for _, v := range []any{admin, &admin, struct{ Admin faucet.GitHubHookAdmin }{admin}} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
			requireNoHookToken(t, fmt.Sprintf(verb, v))
		}
	}
}

func requireNoHookToken(t *testing.T, texts ...string) {
	t.Helper()
	for _, text := range texts {
		require.Zero(t, strings.Count(text, testHookToken), "the hook token is in %q", text)
	}
}
