package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"cdr.dev/slog/v3/sloggers/sloghuman"
	"github.com/coder/quartz"
	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/pgstore"
)

// ../../faucet/oauth/testdata/k8s-* hold a token that a k3s v1.33.5 cluster
// minted for the service account apps/digest-bot with the audience
// https://faucet.example.com, and the cluster's key set. The trust rule pastes
// that key set, so the test needs no cluster, and the one that minted the
// token may be gone.
const (
	k8sTestdata = "../../faucet/oauth/testdata/"
	k8sIssuer   = "https://k8s.wi-test.invalid"
	k8sIAT      = 1795005407
	k8sEXP      = 1795009007
)

type k8sServer struct {
	*testServer
	store *pgstore.Store
}

func newK8sServer(t *testing.T) *k8sServer {
	t.Helper()
	adminURL := os.Getenv(testDatabaseURLEnv)
	if adminURL == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s must be set in CI", testDatabaseURLEnv)
		}
		t.Skipf("set %s to a Postgres URL", testDatabaseURLEnv)
	}
	db := newScratchDatabase(t, adminURL)
	require.NoError(t, pgstore.Migrate(context.Background(), db))
	store := pgstore.New(db)
	clock := quartz.NewMock(t)
	clock.Set(time.Unix(k8sIAT, 0).Add(30 * time.Minute))
	logs := &syncBuffer{}
	logger := slog.Make(sloghuman.Sink(logs)).Leveled(slog.LevelDebug)

	publicURL, err := parseOrigin("https://faucet.example.com")
	require.NoError(t, err)
	tokens, login, checks, err := newLogin(publicURL, nil, store, nil, clock, logger)
	require.NoError(t, err)
	svc, err := faucet.New(faucet.Deps{
		Store: store, Secret: []byte(testSecret), Logger: logger, Authorizer: checks.authorizer, ServiceGrants: checks.grants,
		PrincipalFrom: principalFrom, Clock: clock,
	})
	require.NoError(t, err)
	srv := httptest.NewServer(newRouter(svc, tokens, login, cronRoutes{}, logger))
	t.Cleanup(func() { closeServer(srv, clock) })
	return &k8sServer{testServer: &testServer{Server: srv, clock: clock, logs: logs, mcpPath: clientEndpoint}, store: store}
}

func (s *k8sServer) operator(t *testing.T, args ...string) string {
	t.Helper()
	publicURL, err := parseOrigin("https://faucet.example.com")
	require.NoError(t, err)
	var stdout, stderr bytes.Buffer
	err = clientsCommand(context.Background(), args, s.store, publicURL, publicURL, s.clock.Now(), &stdout, &stderr)
	require.NoError(t, err, "faucet clients %s: %s", strings.Join(args, " "), stderr.String())
	return stdout.String()
}

func (s *k8sServer) exchange(t *testing.T, clientID, subjectToken string, extra ...string) (int, map[string]any) {
	t.Helper()
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:token-exchange"}, "client_id": {clientID},
		"subject_token": {subjectToken}, "subject_token_type": {"urn:ietf:params:oauth:token-type:jwt"},
	}
	for i := 0; i < len(extra); i += 2 {
		form.Set(extra[i], extra[i+1])
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, s.URL+"/oauth/token", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body), "%s", raw)
	return resp.StatusCode, body
}

func TestARealKubernetesTokenLogsInAndReadsEvents(t *testing.T) {
	t.Parallel()
	srv := newK8sServer(t)
	raw, err := os.ReadFile(k8sTestdata + "k8s-token.jwt")
	require.NoError(t, err)
	subject := strings.TrimSpace(string(raw))
	for _, account := range []string{"digest-bot", "other-bot"} {
		require.Equal(t, "client_id: "+account+"\n", srv.operator(t, "create", "-no-secret", "-grant", "repo:octo-org/octo-repo", account))
		require.Regexp(t, `^rule_id: \d+\n$`, srv.operator(t, "trust", account, "-issuer", k8sIssuer,
			"-claim", "sub=system:serviceaccount:apps:"+account, "-jwks-file", k8sTestdata+"k8s-jwks.json"))
	}

	status, body := srv.exchange(t, "digest-bot", subject)
	require.Equal(t, http.StatusOK, status, body)
	token, _ := body["access_token"].(string)
	require.Equal(t, map[string]any{
		"access_token": token, "token_type": "Bearer", "expires_in": float64(900), "scope": "events:read events:subscribe",
		"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
	}, body)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	list, err := callEvents(ctx, srv.connectEvents(ctx, t, token), "events/list", nil)
	require.NoError(t, err)
	require.Contains(t, list, "events")

	for name, c := range map[string]struct {
		client string
		extra  []string
		want   string
	}{
		"another service account's client": {"other-bot", nil, "the subject token is refused: it matches no trust rule of the client"},
		"an actor":                         {"digest-bot", []string{"actor_token", subject, "actor_token_type", "urn:ietf:params:oauth:token-type:jwt"}, "faucet takes no actor_token"},
		"an access token as the subject":   {"digest-bot", []string{"subject_token_type", "urn:ietf:params:oauth:token-type:access_token"}, "subject_token_type must be"},
	} {
		status, body := srv.exchange(t, c.client, subject, c.extra...)
		require.Equal(t, http.StatusBadRequest, status, name)
		require.Equal(t, "invalid_request", body["error"], name)
		require.Contains(t, body["error_description"], c.want, name)
	}
	srv.clock.Set(time.Unix(k8sEXP, 0))
	status, body = srv.exchange(t, "digest-bot", subject)
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "the subject token is refused: it has expired, or expires in less than a second", body["error_description"])

	logs := srv.logs.String()
	require.Contains(t, logs, "a token exchange was refused")
	require.NotContains(t, logs, subject)
	require.NotContains(t, logs, strings.Split(subject, ".")[2], "no log holds the subject token's signature")
	require.NotContains(t, logs, token)
}
