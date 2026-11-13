package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"cdr.dev/slog/v3/sloggers/sloghuman"
	"github.com/coder/faucet/faucet"
	"github.com/coder/quartz"
)

const (
	testCronSecret = "cron-test-0123456789abcdefghij"
	testHookToken  = "ghp_hook0123456789abcdefghijklmnopqrstuvwx"
)

type cronStore struct {
	faucet.Store
	mu      sync.Mutex
	purges  []time.Time
	stored  []string
	failing error
}

func (s *cronStore) Purge(_ context.Context, before time.Time) (faucet.PurgeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing != nil {
		return faucet.PurgeResult{}, s.failing
	}
	s.purges = append(s.purges, before)
	return faucet.PurgeResult{Deleted: 3, PurgedThrough: 11}, nil
}

func (s *cronStore) StoredDeliveries(_ context.Context, ids []string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing != nil {
		return nil, s.failing
	}
	var out []string
	for _, id := range ids {
		for _, stored := range s.stored {
			if id == stored {
				out = append(out, id)
			}
		}
	}
	return out, nil
}

func (s *cronStore) purgeTimes() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.purges...)
}

type hookStub struct {
	*httptest.Server
	mu     sync.Mutex
	calls  []string
	tokens []string
	status int
}

const hookDeliveries = `[
  {"id": 31, "guid": "guid-failed", "delivered_at": %q, "status_code": 500},
  {"id": 30, "guid": "guid-stored", "delivered_at": %q, "status_code": 401},
  {"id": 29, "guid": "guid-fine", "delivered_at": %q, "status_code": 200}
]`

func newHookStub(t *testing.T, now time.Time) *hookStub {
	t.Helper()
	stub := &hookStub{status: http.StatusAccepted}
	stamp := now.Add(-10 * time.Minute).Format(time.RFC3339)
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		stub.calls = append(stub.calls, r.Method+" "+r.URL.Path)
		stub.tokens = append(stub.tokens, r.Header.Get("Authorization"))
		status := stub.status
		stub.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/orgs/octo-org/hooks/5/deliveries":
			_, _ = fmt.Fprintf(w, hookDeliveries, stamp, stamp, stamp)
		case r.Method == http.MethodPost && r.URL.Path == "/orgs/octo-org/hooks/5/deliveries/31/attempts":
			w.WriteHeader(status)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(stub.Close)
	return stub
}

func (s *hookStub) seen() (calls, tokens []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...), append([]string(nil), s.tokens...)
}

type cronServer struct {
	*httptest.Server
	store *cronStore
	clock *quartz.Mock
	hooks *hookStub
	logs  *syncBuffer
}

type cronOptions struct {
	secret     string
	retention  time.Duration
	hookConfig bool
}

func newCronServer(t *testing.T, opts cronOptions) *cronServer {
	t.Helper()
	clock := quartz.NewMock(t)
	store := &cronStore{stored: []string{"guid-stored"}}
	hooks := newHookStub(t, clock.Now())
	logs := &syncBuffer{}
	logger := slog.Make(sloghuman.Sink(logs))
	retention := opts.retention
	if retention == 0 {
		retention = 30 * 24 * time.Hour
	}

	deps := faucet.Deps{
		Store: store, Secret: []byte(testSecret), Logger: logger, Clock: clock,
		Authorizer:    faucet.TokenAuthorizer{BaseURL: hooks.URL},
		PrincipalFrom: principalFrom,
	}
	if opts.hookConfig {
		admin, err := faucet.NewGitHubHookAdmin(hooks.URL, testHookToken, faucet.HookTarget{Org: "octo-org", HookID: 5}, hooks.Client())
		require.NoError(t, err)
		deps.HookAdmin = admin
	}
	svc, err := faucet.New(deps)
	require.NoError(t, err)

	cron := cronRoutes{
		secret: []byte(opts.secret), svc: svc, retention: retention, lookback: redeliverLookback,
		redelivers: opts.hookConfig, clock: clock, logger: logger,
	}
	srv := httptest.NewServer(newRouter(svc, faucet.TokenAuthorizer{BaseURL: hooks.URL}, cron, logger))
	t.Cleanup(srv.Close)
	return &cronServer{Server: srv, store: store, clock: clock, hooks: hooks, logs: logs}
}

func (s *cronServer) get(t *testing.T, method, path string, authHeader *string) (status int, challenge, body string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, s.URL+path, nil)
	require.NoError(t, err)
	if authHeader != nil {
		req.Header["Authorization"] = []string{*authHeader}
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header.Get("WWW-Authenticate"), string(raw)
}

func bearer(token string) *string {
	h := "Bearer " + token
	return &h
}

func (s *cronServer) requireNoSecrets(t *testing.T, texts ...string) {
	t.Helper()
	for _, secret := range []string{testCronSecret, testHookToken} {
		for _, text := range append(texts, s.logs.String()) {
			require.Zero(t, strings.Count(text, secret), "a secret appears in a response or in the logs")
		}
	}
}

var cronPaths = []string{"/cron/purge", "/cron/redeliver"}

func TestCronRoutesRefuseARequestWithoutTheRightSecret(t *testing.T) {
	t.Parallel()

	srv := newCronServer(t, cronOptions{secret: testCronSecret, hookConfig: true})
	wrong := map[string]*string{
		"no header":                       nil,
		"empty header":                    new(string),
		"basic auth":                      ptr("Basic " + testCronSecret),
		"a bare token":                    ptr(testCronSecret),
		"two tokens":                      ptr("Bearer " + testCronSecret + " " + testCronSecret),
		"a wrong token":                   bearer("cron-test-wrong-secret-0123456789"),
		"the secret with a character off": bearer(testCronSecret[:len(testCronSecret)-1]),
		"the secret and one more":         bearer(testCronSecret + "x"),
		"a prefix of the secret":          bearer(testCronSecret[:8]),
		"the secret in another case":      bearer(strings.ToUpper(testCronSecret)),
		"the hook token":                  bearer(testHookToken),
		"an empty token":                  ptr("Bearer "),
		"a token over 1024 bytes":         bearer(strings.Repeat("a", 1025)),
	}
	for _, path := range cronPaths {
		for name, header := range wrong {
			status, challenge, body := srv.get(t, http.MethodGet, path, header)
			require.Equal(t, http.StatusUnauthorized, status, "%s %s", path, name)
			require.Equal(t, "Bearer", challenge, "%s %s", path, name)
			require.Equal(t, "unauthorized\n", body, "%s %s: one body for every refusal, so there is no oracle", path, name)
			srv.requireNoSecrets(t, body)
		}
	}
	require.Empty(t, srv.store.purgeTimes(), "a refused request must not purge")
	calls, _ := srv.hooks.seen()
	require.Empty(t, calls, "a refused request must not call GitHub")
}

func ptr(s string) *string { return &s }

func TestCronRoutesRunForTheRightSecretAndOnlyForGet(t *testing.T) {
	t.Parallel()

	srv := newCronServer(t, cronOptions{secret: testCronSecret, hookConfig: true})
	for _, path := range cronPaths {
		status, _, body := srv.get(t, http.MethodGet, path, bearer(testCronSecret))
		require.Equal(t, http.StatusOK, status, "%s: %s", path, body)
		srv.requireNoSecrets(t, body)

		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			status, _, _ := srv.get(t, method, path, bearer(testCronSecret))
			require.Equal(t, http.StatusMethodNotAllowed, status, "%s %s", method, path)
		}
	}
	require.Len(t, srv.store.purgeTimes(), 1, "only the GET ran the purge")
}

func TestCronRoutesAnswer503AndRunNothingWithoutACronSecret(t *testing.T) {
	t.Parallel()

	srv := newCronServer(t, cronOptions{secret: "", hookConfig: true})
	for _, path := range cronPaths {
		for name, header := range map[string]*string{
			"no header": nil, "an empty bearer": ptr("Bearer "), "a guess": bearer("anything"), "the hook token": bearer(testHookToken),
		} {
			status, _, body := srv.get(t, http.MethodGet, path, header)
			require.Equal(t, http.StatusServiceUnavailable, status, "%s %s", path, name)
			srv.requireNoSecrets(t, body)
		}
	}
	require.Empty(t, srv.store.purgeTimes())
	calls, _ := srv.hooks.seen()
	require.Empty(t, calls)

	status, _, _ := srv.get(t, http.MethodGet, "/healthz", nil)
	require.Equal(t, http.StatusOK, status, "the rest of the server runs")
}

func TestCronPurgeDeletesWhatIsOlderThanTheRetention(t *testing.T) {
	t.Parallel()

	for _, retention := range []time.Duration{30 * 24 * time.Hour, 7 * 24 * time.Hour} {
		srv := newCronServer(t, cronOptions{secret: testCronSecret, retention: retention})
		status, _, body := srv.get(t, http.MethodGet, "/cron/purge", bearer(testCronSecret))
		require.Equal(t, http.StatusOK, status)

		require.Equal(t, []time.Time{srv.clock.Now().Add(-retention)}, srv.store.purgeTimes())
		var got struct {
			Deleted       int64 `json:"deleted"`
			PurgedThrough int64 `json:"purged_through"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &got), body)
		require.Equal(t, int64(3), got.Deleted)
		require.Equal(t, int64(11), got.PurgedThrough)
	}
}

func TestCronPurgeFailureIsAGenericServerError(t *testing.T) {
	t.Parallel()

	srv := newCronServer(t, cronOptions{secret: testCronSecret})
	srv.store.failing = xerrors.New("connection to db.internal:5432 refused")

	status, _, body := srv.get(t, http.MethodGet, "/cron/purge", bearer(testCronSecret))
	require.Equal(t, http.StatusInternalServerError, status)
	require.NotContains(t, body, "db.internal", "the response does not repeat the error")
	require.Contains(t, srv.logs.String(), "db.internal", "the log keeps it")
}

func TestCronRedeliverAsksGitHubForTheFailedUnstoredDeliveryOnly(t *testing.T) {
	t.Parallel()

	srv := newCronServer(t, cronOptions{secret: testCronSecret, hookConfig: true})
	status, _, body := srv.get(t, http.MethodGet, "/cron/redeliver", bearer(testCronSecret))
	require.Equal(t, http.StatusOK, status, body)

	var got struct {
		Examined   int  `json:"examined"`
		Pending    int  `json:"pending"`
		Requested  int  `json:"requested"`
		Incomplete bool `json:"incomplete"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &got), body)
	require.Equal(t, 3, got.Examined)
	require.Equal(t, 1, got.Pending, "guid-stored is stored and guid-fine succeeded")
	require.Equal(t, 1, got.Requested)
	require.False(t, got.Incomplete)

	calls, tokens := srv.hooks.seen()
	require.Equal(t, []string{
		"GET /orgs/octo-org/hooks/5/deliveries",
		"POST /orgs/octo-org/hooks/5/deliveries/31/attempts",
	}, calls)
	for _, token := range tokens {
		require.Equal(t, "Bearer "+testHookToken, token, "the hook token goes to the API base")
	}
	srv.requireNoSecrets(t, body)
}

// outageGitHub plays GitHub for a delivery that failed while faucet was down: a redelivery adds an attempt that is
// flagged as one and dated when it happens, and faucet stores the event only once the outage is over.
type outageGitHub struct {
	mu        sync.Mutex
	clock     quartz.Clock
	store     *cronStore
	attempts  []faucet.HookDelivery
	downUntil time.Time
}

func (g *outageGitHub) ListDeliveries(context.Context, string) (faucet.DeliveryPage, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var newestFirst []faucet.HookDelivery
	for i := len(g.attempts) - 1; i >= 0; i-- {
		newestFirst = append(newestFirst, g.attempts[i])
	}
	return faucet.DeliveryPage{Deliveries: newestFirst}, nil
}

func (g *outageGitHub) Redeliver(context.Context, int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock.Now()
	status := http.StatusServiceUnavailable
	if !now.Before(g.downUntil) {
		status = http.StatusOK
		g.store.mu.Lock()
		g.store.stored = append(g.store.stored, "G")
		g.store.mu.Unlock()
	}
	g.attempts = append(g.attempts, faucet.HookDelivery{ID: int64(len(g.attempts) + 1), GUID: "G", DeliveredAt: now, StatusCode: status, Redelivery: true})
	return nil
}

func TestCronRedeliverRecoversADeliveryThatFailedDuringAnOutageAtTheDocumentedSchedule(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		outage    time.Duration
		recovered bool
	}{
		"an outage of 35 minutes is recovered on the third request":  {35 * time.Minute, true},
		"an outage of 100 minutes is counted and logged as given up": {100 * time.Minute, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			clock := quartz.NewMock(t)
			store := &cronStore{}
			logs := &syncBuffer{}
			logger := slog.Make(sloghuman.Sink(logs))
			hooks := &outageGitHub{clock: clock, store: store, downUntil: clock.Now().Add(tc.outage - time.Minute)}
			hooks.attempts = []faucet.HookDelivery{{ID: 1, GUID: "G", DeliveredAt: clock.Now().Add(-time.Minute), StatusCode: http.StatusServiceUnavailable}}
			svc, err := faucet.New(faucet.Deps{
				Store: store, Secret: []byte(testSecret), Logger: logger, Clock: clock, HookAdmin: hooks,
				Authorizer:    faucet.TokenAuthorizer{BaseURL: "http://127.0.0.1:1"},
				PrincipalFrom: principalFrom,
			})
			require.NoError(t, err)
			cron := cronRoutes{svc: svc, lookback: redeliverLookback, redelivers: true, clock: clock, logger: logger}

			gaveUp := 0
			for range 8 {
				rec := httptest.NewRecorder()
				cron.redeliver(rec, httptest.NewRequest(http.MethodGet, "/cron/redeliver", nil))
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				var got struct {
					GaveUp int `json:"gave_up"`
				}
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), rec.Body.String())
				gaveUp += got.GaveUp
				clock.Advance(redeliverEvery).MustWait(context.Background())
			}

			stored, err := store.StoredDeliveries(context.Background(), []string{"G"})
			require.NoError(t, err)
			require.Equal(t, tc.recovered, len(stored) == 1, "a sweep every %s with the lookback of /cron/redeliver", redeliverEvery)
			if tc.recovered {
				require.Zero(t, gaveUp)
			} else {
				require.Positive(t, gaveUp, "the result of /cron/redeliver counts a delivery that is lost")
				require.Contains(t, logs.String(), "redelivery gave up")
			}
		})
	}
}

func TestCronRedeliverIs503WithoutHookConfigAfterTheSecretIsChecked(t *testing.T) {
	t.Parallel()

	srv := newCronServer(t, cronOptions{secret: testCronSecret, hookConfig: false})

	status, _, body := srv.get(t, http.MethodGet, "/cron/redeliver", bearer(testCronSecret))
	require.Equal(t, http.StatusServiceUnavailable, status)
	srv.requireNoSecrets(t, body)
	status, _, _ = srv.get(t, http.MethodGet, "/cron/redeliver", bearer("wrong-secret-0123456789abcdef"))
	require.Equal(t, http.StatusUnauthorized, status, "a caller without the secret learns nothing about the config")
	status, _, _ = srv.get(t, http.MethodGet, "/cron/purge", bearer(testCronSecret))
	require.Equal(t, http.StatusOK, status, "purge does not need the hook config")

	calls, _ := srv.hooks.seen()
	require.Empty(t, calls)
}

func TestCronRedeliverFailureIsAGenericBadGateway(t *testing.T) {
	t.Parallel()

	srv := newCronServer(t, cronOptions{secret: testCronSecret, hookConfig: true})
	srv.hooks.mu.Lock()
	srv.hooks.status = http.StatusUnprocessableEntity
	srv.hooks.mu.Unlock()

	status, _, body := srv.get(t, http.MethodGet, "/cron/redeliver", bearer(testCronSecret))
	require.Equal(t, http.StatusBadGateway, status)
	require.NotContains(t, body, "422")
	require.Contains(t, srv.logs.String(), "422")
	srv.requireNoSecrets(t, body)
}

func TestLoadConfigCron(t *testing.T) {
	t.Parallel()

	base := map[string]string{envDatabaseURL: "postgres://localhost/faucet", envSecret: testSecret}
	load := func(extra map[string]string) (config, error) {
		return loadConfig(func(name string) string {
			if v, ok := extra[name]; ok {
				return v
			}
			return base[name]
		})
	}

	cfg, err := load(nil)
	require.NoError(t, err)
	require.Empty(t, cfg.cronSecret, "cron is off without CRON_SECRET")
	require.Equal(t, 30*24*time.Hour, cfg.retention, "retention defaults to 30 days")
	require.Nil(t, cfg.hookAdmin, "redelivery is off without the hook variables")

	cfg, err = load(map[string]string{envCronSecret: testCronSecret, envRetentionDays: "7"})
	require.NoError(t, err)
	require.Equal(t, testCronSecret, cfg.cronSecret)
	require.Equal(t, 7*24*time.Hour, cfg.retention)

	for name, extra := range map[string]map[string]string{
		"a short cron secret":     {envCronSecret: "short-secret"},
		"retention zero":          {envRetentionDays: "0"},
		"retention negative":      {envRetentionDays: "-5"},
		"retention not a number":  {envRetentionDays: "a month"},
		"retention fractional":    {envRetentionDays: "1.5"},
		"retention over a decade": {envRetentionDays: "3651"},
	} {
		_, err := load(extra)
		require.Error(t, err, name)
		for _, value := range extra {
			if len(value) >= 8 {
				require.NotContains(t, err.Error(), value, "%s: the error does not echo the value", name)
			}
		}
	}
}

func TestLoadConfigHookAdmin(t *testing.T) {
	t.Parallel()

	load := func(extra map[string]string) (config, error) {
		env := map[string]string{envDatabaseURL: "postgres://localhost/faucet", envSecret: testSecret}
		for k, v := range extra {
			env[k] = v
		}
		return loadConfig(func(name string) string { return env[name] })
	}
	org := map[string]string{envHookToken: testHookToken, envHookOrg: "octo-org", envHookID: "5"}
	repo := map[string]string{envHookToken: testHookToken, envHookRepo: "octo/repo", envHookID: "5"}

	for name, env := range map[string]map[string]string{"an org hook": org, "a repo hook": repo} {
		cfg, err := load(env)
		require.NoError(t, err, name)
		require.NotNil(t, cfg.hookAdmin, name)
		require.NotContains(t, fmt.Sprintf("%+v %#v", cfg.hookAdmin, cfg.hookAdmin), testHookToken, name)
	}

	partial := func(drop string) map[string]string {
		env := map[string]string{}
		for k, v := range org {
			if k != drop {
				env[k] = v
			}
		}
		return env
	}
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"a token without an id":      {partial(envHookID), envHookID},
		"an id without a token":      {partial(envHookToken), envHookToken},
		"a token and an id only":     {partial(envHookOrg), envHookOrg},
		"an org and a repo":          {map[string]string{envHookToken: testHookToken, envHookOrg: "o", envHookRepo: "o/r", envHookID: "5"}, envHookRepo},
		"an id that is not a number": {map[string]string{envHookToken: testHookToken, envHookOrg: "o", envHookID: "five"}, envHookID},
		"an id of zero":              {map[string]string{envHookToken: testHookToken, envHookOrg: "o", envHookID: "0"}, envHookID},
		"a token with a space":       {map[string]string{envHookToken: "ghp_a b", envHookOrg: "o", envHookID: "5"}, "FAUCET_HOOK_"},
		"an invalid org":             {map[string]string{envHookToken: testHookToken, envHookOrg: "o/../x", envHookID: "5"}, "FAUCET_HOOK_"},
		"an invalid repo":            {map[string]string{envHookToken: testHookToken, envHookRepo: "norepo", envHookID: "5"}, "FAUCET_HOOK_"},
	} {
		_, err := load(tc.env)
		require.ErrorContains(t, err, tc.want, name)
		for _, value := range tc.env {
			if len(value) > 3 {
				require.NotContains(t, err.Error(), value, "%s: the error names variables, never values", name)
			}
		}
	}
}
