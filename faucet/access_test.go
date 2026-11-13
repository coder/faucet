package faucet_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

const testToken = "ghp_test0123456789abcdefghijklmnopqrstuv"

type githubStub struct {
	*httptest.Server
	requests atomic.Int64
	tokens   chan string
}

func newGitHubStub(t *testing.T, handler http.HandlerFunc) *githubStub {
	t.Helper()
	stub := &githubStub{tokens: make(chan string, 64)}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.requests.Add(1)
		stub.tokens <- r.Header.Get("Authorization")
		handler(w, r)
	}))
	t.Cleanup(stub.Close)
	return stub
}

func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}
}

func principal(t *testing.T) faucet.Principal {
	t.Helper()
	p, err := faucet.NewPrincipal("octocat", testToken)
	require.NoError(t, err)
	return p
}

func requireNoToken(t *testing.T, what string, texts ...string) {
	t.Helper()
	for _, text := range texts {
		require.Zero(t, strings.Count(text, testToken), "token in %s", what)
	}
}

func TestTokenAuthorizerRepoID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		body    string
		wantID  int64
		wantErr error
		anyErr  bool
	}{
		{name: "OK", status: http.StatusOK, body: `{"id": 1296269, "name": "r"}`, wantID: 1296269},
		{name: "NotFound", status: http.StatusNotFound, body: `{"message":"Not Found"}`, wantErr: faucet.ErrNoAccess},
		{name: "Forbidden", status: http.StatusForbidden, body: `{"message":"no"}`, wantErr: faucet.ErrNoAccess},
		{name: "Unauthorized", status: http.StatusUnauthorized, body: `{"message":"Bad credentials"}`, wantErr: faucet.ErrInvalidToken},
		{name: "ServerError", status: http.StatusInternalServerError, body: `boom`, anyErr: true},
		{name: "BadGateway", status: http.StatusBadGateway, body: ``, anyErr: true},
		{name: "OKWithGarbage", status: http.StatusOK, body: `<html>`, anyErr: true},
		{name: "OKWithoutID", status: http.StatusOK, body: `{"name":"r"}`, anyErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stub := newGitHubStub(t, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/repos/octo-org/hello.world", r.URL.Path)
				require.Equal(t, http.MethodGet, r.Method)
				reply(tt.status, tt.body)(w, r)
			})
			authz := faucet.TokenAuthorizer{BaseURL: stub.URL, Client: stub.Client()}

			id, err := authz.RepoID(context.Background(), principal(t), "octo-org/hello.world")
			switch {
			case tt.wantErr != nil:
				require.ErrorIs(t, err, tt.wantErr)
				require.Zero(t, id)
			case tt.anyErr:
				require.Error(t, err)
				require.NotErrorIs(t, err, faucet.ErrNoAccess)
				require.Zero(t, id)
			default:
				require.NoError(t, err)
				require.Equal(t, tt.wantID, id)
			}
			if err != nil {
				requireNoToken(t, "error", err.Error())
			}
			require.Equal(t, "Bearer "+testToken, <-stub.tokens)
		})
	}
}

func TestTokenAuthorizerRejectsBadRepoBeforeCallingGitHub(t *testing.T) {
	t.Parallel()

	stub := newGitHubStub(t, reply(http.StatusOK, `{"id": 1}`))
	authz := faucet.TokenAuthorizer{BaseURL: stub.URL, Client: stub.Client()}

	bad := []string{
		"", "o", "o/", "/r", "o/r/x", "o r/x", "o/r x", "o/r?x=1", "o/r#f", "o/r\n", "o/.", "o/..",
		"../r", "o/%2e%2e", "o\\r/x", "-o/r", "_o/r", "o/r%2fx", "ö/r", "o/ü",
		strings.Repeat("o", 40) + "/r", "o/" + strings.Repeat("r", 101), "http://evil.example/x/y",
	}
	for _, repo := range bad {
		_, err := authz.RepoID(context.Background(), principal(t), repo)
		require.Error(t, err, "%q", repo)
		require.NotErrorIs(t, err, faucet.ErrNoAccess, "%q", repo)
	}
	require.Zero(t, stub.requests.Load(), "a bad repo must never reach GitHub")

	for _, repo := range []string{"a/b", "octocat/Hello-World", "o_x/r.js", "o/.github", "o/_x", strings.Repeat("o", 39) + "/" + strings.Repeat("r", 100)} {
		_, err := authz.RepoID(context.Background(), principal(t), repo)
		require.NoError(t, err, "%q", repo)
	}
}

func TestTokenAuthorizerNeedsAToken(t *testing.T) {
	t.Parallel()

	stub := newGitHubStub(t, reply(http.StatusOK, `{"id": 1, "login": "x"}`))
	authz := faucet.TokenAuthorizer{BaseURL: stub.URL, Client: stub.Client()}

	noToken, err := faucet.NewPrincipal("octocat", "")
	require.NoError(t, err)
	_, err = authz.RepoID(context.Background(), noToken, "o/r")
	require.Error(t, err)
	_, err = authz.RepoID(context.Background(), faucet.Principal{}, "o/r")
	require.Error(t, err)
	_, err = authz.Authenticate(context.Background(), "")
	require.Error(t, err)
	require.Zero(t, stub.requests.Load(), "an unauthenticated request would be answered for public repositories")
}

func TestTokenAuthorizerSendsTheTokenOnlyToBaseURL(t *testing.T) {
	t.Parallel()

	t.Run("CrossHostRedirectIsRefused", func(t *testing.T) {
		t.Parallel()

		elsewhere := newGitHubStub(t, reply(http.StatusOK, `{"id": 99}`))
		stub := newGitHubStub(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusMovedPermanently)
		})
		authz := faucet.TokenAuthorizer{BaseURL: stub.URL, Client: stub.Client()}

		id, err := authz.RepoID(context.Background(), principal(t), "o/r")
		require.Error(t, err)
		require.Zero(t, id)
		requireNoToken(t, "error", err.Error())
		require.Zero(t, elsewhere.requests.Load(), "the redirect target must not be called")
	})

	t.Run("SameHostRedirectIsFollowed", func(t *testing.T) {
		t.Parallel()

		stub := newGitHubStub(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/repos/old/name" {
				http.Redirect(w, r, "/repositories/1296269", http.StatusMovedPermanently)
				return
			}
			require.Equal(t, "/repositories/1296269", r.URL.Path)
			reply(http.StatusOK, `{"id": 1296269}`)(w, r)
		})
		authz := faucet.TokenAuthorizer{BaseURL: stub.URL, Client: stub.Client()}

		id, err := authz.RepoID(context.Background(), principal(t), "old/name")
		require.NoError(t, err)
		require.EqualValues(t, 1296269, id)
		require.Equal(t, "Bearer "+testToken, <-stub.tokens)
		require.Equal(t, "Bearer "+testToken, <-stub.tokens)
	})

	t.Run("RedirectLoopStops", func(t *testing.T) {
		t.Parallel()

		stub := newGitHubStub(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, r.URL.Path, http.StatusFound)
		})
		authz := faucet.TokenAuthorizer{BaseURL: stub.URL, Client: stub.Client()}

		_, err := authz.RepoID(context.Background(), principal(t), "o/r")
		require.Error(t, err)
		require.LessOrEqual(t, stub.requests.Load(), int64(5))
	})

	t.Run("CallerClientIsNotModified", func(t *testing.T) {
		t.Parallel()

		stub := newGitHubStub(t, reply(http.StatusOK, `{"id": 1}`))
		client := stub.Client()
		authz := faucet.TokenAuthorizer{BaseURL: stub.URL, Client: client}

		_, err := authz.RepoID(context.Background(), principal(t), "o/r")
		require.NoError(t, err)
		require.Nil(t, client.CheckRedirect)
	})
}

func TestTokenAuthorizerStopsOnCancel(t *testing.T) {
	t.Parallel()

	stub := newGitHubStub(t, reply(http.StatusOK, `{"id": 1}`))
	authz := faucet.TokenAuthorizer{BaseURL: stub.URL, Client: stub.Client()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := authz.RepoID(ctx, principal(t), "o/r")
	require.ErrorIs(t, err, context.Canceled)
	requireNoToken(t, "error", err.Error())
}

func TestTokenAuthorizerValidatesBaseURL(t *testing.T) {
	t.Parallel()

	good := []string{
		"https://api.github.com", "https://api.github.com/", "https://ghe.example.com/api/v3/",
		"http://127.0.0.1:8080", "http://localhost:1", "http://[::1]:8080",
	}
	for _, u := range good {
		require.NoError(t, faucet.TokenAuthorizer{BaseURL: u}.Validate(), u)
	}

	const password = "hunter2-not-a-real-password"
	bad := []string{
		"", "api.github.com", "/relative", "https://", "ftp://api.github.com", "http://api.github.com",
		"http://127.0.0.1.example.com", "https://user:" + password + "@api.github.com",
		"https://api.github.com?x=1", "https://api.github.com?", "https://api.github.com#frag",
		"http://" + password + "%zz",
	}
	for _, u := range bad {
		err := faucet.TokenAuthorizer{BaseURL: u}.Validate()
		require.Error(t, err, u)
		require.Zero(t, strings.Count(err.Error(), password), "the error must not echo the URL")
	}

	_, err := faucet.TokenAuthorizer{BaseURL: "http://example.com"}.RepoID(context.Background(), principal(t), "o/r")
	require.Error(t, err, "RepoID must refuse to send a token over plain http to a remote host")
}

func TestTokenAuthorizerBaseURLPathIsKept(t *testing.T) {
	t.Parallel()

	stub := newGitHubStub(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v3/repos/o/r", r.URL.Path)
		reply(http.StatusOK, `{"id": 5}`)(w, r)
	})
	authz := faucet.TokenAuthorizer{BaseURL: stub.URL + "/api/v3/", Client: stub.Client()}

	id, err := authz.RepoID(context.Background(), principal(t), "o/r")
	require.NoError(t, err)
	require.EqualValues(t, 5, id)
}

func TestTokenAuthorizerAuthenticate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		body    string
		want    string
		wantErr error
		anyErr  bool
	}{
		{name: "OK", status: http.StatusOK, body: `{"login": "octocat"}`, want: "octocat"},
		{name: "Unauthorized", status: http.StatusUnauthorized, body: `{}`, wantErr: faucet.ErrInvalidToken},
		{name: "Forbidden", status: http.StatusForbidden, body: `{}`, wantErr: faucet.ErrInvalidToken},
		{name: "ServerError", status: http.StatusInternalServerError, body: `x`, anyErr: true},
		{name: "OKWithoutLogin", status: http.StatusOK, body: `{}`, anyErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stub := newGitHubStub(t, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/user", r.URL.Path)
				reply(tt.status, tt.body)(w, r)
			})
			authz := faucet.TokenAuthorizer{BaseURL: stub.URL, Client: stub.Client()}

			p, err := authz.Authenticate(context.Background(), testToken)
			if tt.want != "" {
				require.NoError(t, err)
				require.Equal(t, tt.want, p.Login)
				return
			}
			require.Error(t, err)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
			}
			require.Empty(t, p.Login)
			requireNoToken(t, "error", err.Error())
		})
	}
}

func TestAuthenticatedPrincipalCarriesTheTokenToRepoID(t *testing.T) {
	t.Parallel()

	stub := newGitHubStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			reply(http.StatusOK, `{"login": "octocat"}`)(w, r)
			return
		}
		reply(http.StatusOK, `{"id": 7}`)(w, r)
	})
	authz := faucet.TokenAuthorizer{BaseURL: stub.URL, Client: stub.Client()}

	p, err := authz.Authenticate(context.Background(), testToken)
	require.NoError(t, err)
	id, err := authz.RepoID(context.Background(), p, "o/r")
	require.NoError(t, err)
	require.EqualValues(t, 7, id)
	require.Equal(t, "Bearer "+testToken, <-stub.tokens)
	require.Equal(t, "Bearer "+testToken, <-stub.tokens)
}

func TestPrincipalNeverPrintsItsToken(t *testing.T) {
	t.Parallel()

	_, err := faucet.NewPrincipal("", testToken)
	require.Error(t, err)

	p := principal(t)
	require.Equal(t, "octocat", p.Login)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x"} {
		requireNoToken(t, verb, fmt.Sprintf(verb, p), fmt.Sprintf(verb, &p), fmt.Sprintf(verb, []faucet.Principal{p}), fmt.Sprintf(verb, map[string]faucet.Principal{"k": p}))
	}
	asJSON, err := json.Marshal(p)
	require.NoError(t, err)
	requireNoToken(t, "json", string(asJSON))
	require.Contains(t, fmt.Sprint(p), "octocat")
}
