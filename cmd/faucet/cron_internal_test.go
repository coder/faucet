package main

import (
	"bytes"
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
	purgePath      = "/cron/purge"
	deliverPath    = "/cron/deliver"
	redeliverPath  = "/cron/redeliver"
)

// cronRedeliveryStore answers a redelivery run with what the test set, so a
// test reads what the route reports without a database.
type cronRedeliveryStore struct {
	mu        sync.Mutex
	busy      bool
	settle    faucet.SettleResult
	settleErr error
	purgeErr  error
	acquired  int
}

var _ faucet.RedeliveryStore = (*cronRedeliveryStore)(nil)

func (s *cronRedeliveryStore) AcquireRedeliveryLease(context.Context, string, time.Time, time.Duration) (faucet.RedeliveryScan, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		return faucet.RedeliveryScan{}, false, nil
	}
	s.acquired++
	return faucet.RedeliveryScan{Epoch: 1}, true, nil
}

func (*cronRedeliveryStore) ReleaseRedeliveryLease(context.Context, int64) error { return nil }

func (*cronRedeliveryStore) ApplyRedeliveryScan(context.Context, faucet.ScanStep) (faucet.ScanResult, error) {
	return faucet.ScanResult{}, nil
}

func (s *cronRedeliveryStore) SettleRedeliveries(context.Context, faucet.SettleRequest) (faucet.SettleResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settle, s.settleErr
}

func (*cronRedeliveryStore) ClaimRedelivery(context.Context, faucet.ClaimRequest) (faucet.RedeliveryClaim, bool, error) {
	return faucet.RedeliveryClaim{}, false, nil
}

func (s *cronRedeliveryStore) PurgeRedeliveries(context.Context, time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return 0, s.purgeErr
}

func (s *cronRedeliveryStore) runs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acquired
}

type cronHooks struct {
	mu    sync.Mutex
	lists int
}

func (*cronHooks) Hook() string { return "/orgs/octo-org/hooks/9" }

func (h *cronHooks) ListDeliveries(context.Context, string) (faucet.DeliveryPage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lists++
	return faucet.DeliveryPage{}, nil
}

func (*cronHooks) Redeliver(context.Context, int64) error { return nil }

type cronStore struct {
	faucet.Store
	mu      sync.Mutex
	purges  []time.Time
	claims  []faucet.DeliveryClaim
	failing error
	cleared int64
	expired int64
}

func (s *cronStore) ScanTargets(context.Context, time.Time, int32, int32) ([]faucet.ScanTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return nil, s.failing
}

func (s *cronStore) ExpireDeliveries(context.Context, time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expired, s.failing
}

func (s *cronStore) ClearExpiredPreviousSecrets(context.Context, time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleared, s.failing
}

func (s *cronStore) ClaimDelivery(context.Context, time.Time, time.Duration) (faucet.DeliveryClaim, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing != nil || len(s.claims) == 0 {
		return faucet.DeliveryClaim{}, false, s.failing
	}
	claim := s.claims[0]
	s.claims = s.claims[1:]
	return claim, true, nil
}

func (*cronStore) FinishDelivery(context.Context, faucet.DeliveryRef, time.Time, faucet.Finish) (faucet.FinishResult, error) {
	return faucet.FinishResult{Accepted: true, Removed: 1}, nil
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

func (*cronStore) DeleteExpiredSubscriptions(context.Context, time.Time) (int64, error) {
	return 2, nil
}
func (s *cronStore) purgeTimes() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.purges...)
}

type cronServer struct {
	*httptest.Server
	store      *cronStore
	redelivery *cronRedeliveryStore
	hooks      *cronHooks
	clock      *quartz.Mock
	logs       *syncBuffer
}

type cronOptions struct {
	secret    string
	retention time.Duration
	webhook   bool
	// redelivery turns on redelivery, with the stores above.
	redelivery bool
}

func newCronServer(t *testing.T, opts cronOptions) *cronServer {
	t.Helper()
	clock := quartz.NewMock(t)
	store := &cronStore{}
	logs := &syncBuffer{}
	logger := slog.Make(sloghuman.Sink(logs))
	retention := opts.retention
	if retention == 0 {
		retention = 30 * 24 * time.Hour
	}

	authorizer := faucet.TokenAuthorizer{BaseURL: "http://127.0.0.1:1"}
	deps := faucet.Deps{
		Store: store, Secret: []byte(testSecret), Logger: logger, Clock: clock,
		Authorizer:    authorizer,
		PrincipalFrom: principalFrom,
	}
	if opts.webhook {
		key, err := faucet.NewWebhookKey(bytes.Repeat([]byte{7}, 32))
		require.NoError(t, err)
		callbacks, err := faucet.NewCallbackAllowlist([]string{"https://hooks.example.test"}, false)
		require.NoError(t, err)
		deps.Webhook = &faucet.WebhookDeps{Key: key, Callbacks: callbacks}
	}
	redelivery, hooks := &cronRedeliveryStore{}, &cronHooks{}
	if opts.redelivery {
		deps.Redelivery = &faucet.RedeliveryDeps{Store: redelivery, Hooks: hooks}
	}
	svc, err := faucet.New(deps)
	require.NoError(t, err)

	cron := cronRoutes{secret: []byte(opts.secret), svc: svc, retention: retention, delivers: opts.webhook, redelivers: opts.redelivery, clock: clock, logger: logger}
	srv := httptest.NewServer(newRouter(svc, authorizer, cron, logger))
	t.Cleanup(srv.Close)
	return &cronServer{Server: srv, store: store, redelivery: redelivery, hooks: hooks, clock: clock, logs: logs}
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
	for _, text := range append(texts, s.logs.String()) {
		require.Zero(t, strings.Count(text, testCronSecret), "a secret appears in a response or in the logs")
	}
}

func TestCronRoutesRefuseARequestWithoutTheRightSecret(t *testing.T) {
	t.Parallel()

	srv := newCronServer(t, cronOptions{secret: testCronSecret, redelivery: true})
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
		"an empty token":                  ptr("Bearer "),
		"a token over 1024 bytes":         bearer(strings.Repeat("a", 1025)),
	}
	for _, path := range []string{purgePath, deliverPath, redeliverPath} {
		for name, header := range wrong {
			status, challenge, body := srv.get(t, http.MethodGet, path, header)
			require.Equal(t, http.StatusUnauthorized, status, "%s %s", path, name)
			require.Equal(t, "Bearer", challenge, "%s %s", path, name)
			require.Equal(t, "unauthorized\n", body, "%s %s: one body for every refusal, so there is no oracle", path, name)
			srv.requireNoSecrets(t, body)
		}
	}
	require.Empty(t, srv.store.purgeTimes(), "a refused request must not purge")
	require.Zero(t, srv.redelivery.runs(), "a refused request must not start a redelivery run")
	require.Zero(t, srv.hooks.lists, "and must not call GitHub")
}

func ptr(s string) *string { return &s }

func TestCronRoutesRunForTheRightSecretAndOnlyForGet(t *testing.T) {
	t.Parallel()

	srv := newCronServer(t, cronOptions{secret: testCronSecret, webhook: true, redelivery: true})
	for _, path := range []string{purgePath, deliverPath, redeliverPath} {
		status, _, body := srv.get(t, http.MethodGet, path, bearer(testCronSecret))
		require.Equal(t, http.StatusOK, status, "%s: %s", path, body)
		srv.requireNoSecrets(t, body)

		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			status, _, _ := srv.get(t, method, path, bearer(testCronSecret))
			require.Equal(t, http.StatusMethodNotAllowed, status, "%s %s", method, path)
		}
	}
	require.Len(t, srv.store.purgeTimes(), 1, "only the GET ran the purge")
	require.Equal(t, 1, srv.redelivery.runs(), "only the GET ran redelivery")
}

func TestCronRoutesAnswer503AndRunNothingWithoutACronSecret(t *testing.T) {
	t.Parallel()

	srv := newCronServer(t, cronOptions{secret: "", redelivery: true})
	for _, path := range []string{purgePath, deliverPath, redeliverPath} {
		for name, header := range map[string]*string{
			"no header": nil, "an empty bearer": ptr("Bearer "), "a guess": bearer("anything"),
		} {
			status, _, body := srv.get(t, http.MethodGet, path, header)
			require.Equal(t, http.StatusServiceUnavailable, status, "%s %s", path, name)
			srv.requireNoSecrets(t, body)
		}
	}
	require.Empty(t, srv.store.purgeTimes())
	require.Zero(t, srv.redelivery.runs())

	status, _, _ := srv.get(t, http.MethodGet, "/healthz", nil)
	require.Equal(t, http.StatusOK, status, "the rest of the server runs")
}

func TestCronPurgeDeletesWhatIsOlderThanTheRetention(t *testing.T) {
	t.Parallel()

	for _, retention := range []time.Duration{30 * 24 * time.Hour, 7 * 24 * time.Hour} {
		srv := newCronServer(t, cronOptions{secret: testCronSecret, retention: retention})
		srv.store.expired = 5
		status, _, body := srv.get(t, http.MethodGet, purgePath, bearer(testCronSecret))
		require.Equal(t, http.StatusOK, status)

		require.Equal(t, []time.Time{srv.clock.Now().Add(-retention)}, srv.store.purgeTimes())
		var got struct {
			Deleted              int64 `json:"deleted"`
			PurgedThrough        int64 `json:"purged_through"`
			ExpiredSubscriptions int64 `json:"expired_subscriptions"`
			EndedDeliveries      int64 `json:"ended_deliveries"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &got), body)
		require.Equal(t, int64(3), got.Deleted)
		require.Equal(t, int64(11), got.PurgedThrough)
		require.Equal(t, int64(2), got.ExpiredSubscriptions, "the reply counts the expired subscriptions")
		require.Equal(t, int64(5), got.EndedDeliveries, "and the deliveries that ended with them")
	}
}

func TestCronPurgeFailureIsAGenericServerError(t *testing.T) {
	t.Parallel()

	srv := newCronServer(t, cronOptions{secret: testCronSecret})
	srv.store.failing = xerrors.New("connection to db.internal:5432 refused")

	status, _, body := srv.get(t, http.MethodGet, purgePath, bearer(testCronSecret))
	require.Equal(t, http.StatusInternalServerError, status)
	require.NotContains(t, body, "db.internal", "the response does not repeat the error")
	require.Contains(t, srv.logs.String(), "db.internal", "the log keeps it")
}

func TestCronRedeliverReportsEveryOutcome(t *testing.T) {
	t.Parallel()

	srv := newCronServer(t, cronOptions{secret: testCronSecret, redelivery: true})
	srv.redelivery.settle = faucet.SettleResult{
		Recovered: []string{"g-1", "g-2"},
		GaveUp: []faucet.GaveUp{
			{GUID: "g-3", Reason: faucet.GiveUpLimit, Asks: faucet.MaxAsksPerDelivery},
			{GUID: "g-4", Reason: faucet.GiveUpExpired},
		},
		Pending: 3,
	}

	status, _, body := srv.get(t, http.MethodGet, redeliverPath, bearer(testCronSecret))

	require.Equal(t, http.StatusOK, status, body)
	require.JSONEq(t, `{
		"busy": false, "pages": 1, "attempts": 0, "tracked": 0, "asked": 0,
		"recovered": 2, "gave_up": {"limit": 1, "expired": 1}, "gaps": 1, "pending": 3, "ranges": 0
	}`, body, "recoveries, both kinds of give-up and the gap before tracking are counted in the reply")
	require.Equal(t, 1, strings.Count(srv.logs.String(), "g-3"), "a give-up is logged once")
	srv.requireNoSecrets(t, body)

	srv.redelivery.mu.Lock()
	srv.redelivery.busy = true
	srv.redelivery.mu.Unlock()
	status, _, body = srv.get(t, http.MethodGet, redeliverPath, bearer(testCronSecret))
	require.Equal(t, http.StatusOK, status)
	require.JSONEq(t, `{
		"busy": true, "pages": 0, "attempts": 0, "tracked": 0, "asked": 0,
		"recovered": 0, "gave_up": {"limit": 0, "expired": 0}, "gaps": 0, "pending": 0, "ranges": 0
	}`, body, "a run that finds another one holding the lease says so and does nothing")
}

func TestCronRedeliverIs503WithoutHookConfigAfterTheSecretIsChecked(t *testing.T) {
	t.Parallel()

	srv := newCronServer(t, cronOptions{secret: testCronSecret})

	status, _, body := srv.get(t, http.MethodGet, redeliverPath, bearer(testCronSecret))
	require.Equal(t, http.StatusServiceUnavailable, status)
	srv.requireNoSecrets(t, body)
	status, _, _ = srv.get(t, http.MethodGet, redeliverPath, bearer("wrong-secret-0123456789abcdef"))
	require.Equal(t, http.StatusUnauthorized, status, "a caller without the secret learns nothing about the config")
	status, _, _ = srv.get(t, http.MethodGet, purgePath, bearer(testCronSecret))
	require.Equal(t, http.StatusOK, status, "purge does not need the hook config")
}

func TestCronRedeliverFailureIsAGenericServerError(t *testing.T) {
	t.Parallel()

	srv := newCronServer(t, cronOptions{secret: testCronSecret, redelivery: true})
	srv.redelivery.settleErr = xerrors.New("connection to db.internal:5432 refused")

	status, _, body := srv.get(t, http.MethodGet, redeliverPath, bearer(testCronSecret))

	require.Equal(t, http.StatusInternalServerError, status)
	require.NotContains(t, body, "db.internal", "the response does not repeat the error")
	require.Contains(t, srv.logs.String(), "db.internal", "the log keeps it")
	srv.requireNoSecrets(t, body)
}

func TestCronRedeliverFailureStillReportsTheOutcomesItRecorded(t *testing.T) {
	t.Parallel()

	srv := newCronServer(t, cronOptions{secret: testCronSecret, redelivery: true})
	srv.redelivery.settle = faucet.SettleResult{
		Recovered: []string{"g-1", "g-2"},
		GaveUp:    []faucet.GaveUp{{GUID: "g-3", Reason: faucet.GiveUpLimit, Asks: faucet.MaxAsksPerDelivery}},
		Pending:   3,
	}
	srv.redelivery.purgeErr = xerrors.New("connection to db.internal:5432 refused")

	status, _, body := srv.get(t, http.MethodGet, redeliverPath, bearer(testCronSecret))

	require.Equal(t, http.StatusInternalServerError, status, "a failed run is still a failure")
	require.JSONEq(t, `{
		"busy": false, "pages": 1, "attempts": 0, "tracked": 0, "asked": 0,
		"recovered": 2, "gave_up": {"limit": 1, "expired": 0}, "gaps": 1, "pending": 3, "ranges": 0,
		"error": "redelivery failed"
	}`, body, "the outcomes that the run recorded before it failed are in the reply, and the log has them too")
	require.NotContains(t, body, "db.internal")
	srv.requireNoSecrets(t, body)
}

func TestCronDeliverReportsWhatTheRunDid(t *testing.T) {
	t.Parallel()

	srv := newCronServer(t, cronOptions{secret: testCronSecret, webhook: true})
	srv.store.claims = []faucet.DeliveryClaim{{Ref: faucet.DeliveryRef{EventID: 4, Attempt: 1}, MessageID: "msg_x", EnqueuedAt: srv.clock.Now()}}
	srv.store.cleared = 2

	status, _, body := srv.get(t, http.MethodGet, deliverPath, bearer(testCronSecret))
	require.Equal(t, http.StatusOK, status, body)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &got), body)
	require.Equal(t, map[string]any{
		"enqueued": 0.0, "gaps": 0.0, "gaps_sent": 0.0, "gaps_failed": 0.0, "claimed": 1.0, "sent": 0.0, "delivered": 0.0, "retried": 0.0,
		"gave_up": map[string]any{"event_purged": 1.0}, "superseded": 0.0, "previous_secrets_cleared": 2.0, "incomplete": false,
	}, got, "the reply holds counts and reasons, nothing else")
	srv.requireNoSecrets(t, body)
}

func TestCronDeliverIs503WithoutWebhookDeliveryAfterTheSecretIsChecked(t *testing.T) {
	t.Parallel()

	srv := newCronServer(t, cronOptions{secret: testCronSecret})
	status, _, _ := srv.get(t, http.MethodGet, deliverPath, bearer(testCronSecret))
	require.Equal(t, http.StatusServiceUnavailable, status)
	status, _, _ = srv.get(t, http.MethodGet, deliverPath, bearer("wrong-secret-0123456789abcdef"))
	require.Equal(t, http.StatusUnauthorized, status, "a caller without the secret learns nothing about the config")
}

func TestCronDeliverFailureIsAGenericServerError(t *testing.T) {
	t.Parallel()

	srv := newCronServer(t, cronOptions{secret: testCronSecret, webhook: true})
	srv.store.failing = xerrors.New("connection to db.internal:5432 refused")

	status, _, body := srv.get(t, http.MethodGet, deliverPath, bearer(testCronSecret))
	require.Equal(t, http.StatusInternalServerError, status)
	require.NotContains(t, body, "db.internal", "the response does not repeat the error")
	require.Contains(t, srv.logs.String(), "db.internal", "the log keeps it")
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
