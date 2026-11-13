package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	purgePath      = "/cron/purge"
	deliverPath    = "/cron/deliver"
)

type cronStore struct {
	faucet.Store
	mu      sync.Mutex
	purges  []time.Time
	claims  []faucet.DeliveryClaim
	failing error
}

func (s *cronStore) ScanTargets(context.Context, time.Time, int32, int32) ([]faucet.ScanTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return nil, s.failing
}

func (s *cronStore) ExpireDeliveries(context.Context, time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return 0, s.failing
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
	return 0, nil
}
func (s *cronStore) purgeTimes() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.purges...)
}

type cronServer struct {
	*httptest.Server
	store *cronStore
	clock *quartz.Mock
	logs  *syncBuffer
}

type cronOptions struct {
	secret    string
	retention time.Duration
	webhook   bool
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
	svc, err := faucet.New(deps)
	require.NoError(t, err)

	cron := cronRoutes{secret: []byte(opts.secret), svc: svc, retention: retention, delivers: opts.webhook, clock: clock, logger: logger}
	srv := httptest.NewServer(newRouter(svc, authorizer, cron, logger))
	t.Cleanup(srv.Close)
	return &cronServer{Server: srv, store: store, clock: clock, logs: logs}
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

	srv := newCronServer(t, cronOptions{secret: testCronSecret})
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
	for _, path := range []string{purgePath, deliverPath} {
		for name, header := range wrong {
			status, challenge, body := srv.get(t, http.MethodGet, path, header)
			require.Equal(t, http.StatusUnauthorized, status, "%s %s", path, name)
			require.Equal(t, "Bearer", challenge, "%s %s", path, name)
			require.Equal(t, "unauthorized\n", body, "%s %s: one body for every refusal, so there is no oracle", path, name)
			srv.requireNoSecrets(t, body)
		}
	}
	require.Empty(t, srv.store.purgeTimes(), "a refused request must not purge")
}

func ptr(s string) *string { return &s }

func TestCronRoutesRunForTheRightSecretAndOnlyForGet(t *testing.T) {
	t.Parallel()

	srv := newCronServer(t, cronOptions{secret: testCronSecret, webhook: true})
	for _, path := range []string{purgePath, deliverPath} {
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

	srv := newCronServer(t, cronOptions{secret: ""})
	for _, path := range []string{purgePath, deliverPath} {
		for name, header := range map[string]*string{
			"no header": nil, "an empty bearer": ptr("Bearer "), "a guess": bearer("anything"),
		} {
			status, _, body := srv.get(t, http.MethodGet, path, header)
			require.Equal(t, http.StatusServiceUnavailable, status, "%s %s", path, name)
			srv.requireNoSecrets(t, body)
		}
	}
	require.Empty(t, srv.store.purgeTimes())

	status, _, _ := srv.get(t, http.MethodGet, "/healthz", nil)
	require.Equal(t, http.StatusOK, status, "the rest of the server runs")
}

func TestCronPurgeDeletesWhatIsOlderThanTheRetention(t *testing.T) {
	t.Parallel()

	for _, retention := range []time.Duration{30 * 24 * time.Hour, 7 * 24 * time.Hour} {
		srv := newCronServer(t, cronOptions{secret: testCronSecret, retention: retention})
		status, _, body := srv.get(t, http.MethodGet, purgePath, bearer(testCronSecret))
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

	status, _, body := srv.get(t, http.MethodGet, purgePath, bearer(testCronSecret))
	require.Equal(t, http.StatusInternalServerError, status)
	require.NotContains(t, body, "db.internal", "the response does not repeat the error")
	require.Contains(t, srv.logs.String(), "db.internal", "the log keeps it")
}

func TestCronDeliverReportsWhatTheRunDid(t *testing.T) {
	t.Parallel()

	srv := newCronServer(t, cronOptions{secret: testCronSecret, webhook: true})
	srv.store.claims = []faucet.DeliveryClaim{{Ref: faucet.DeliveryRef{EventID: 4, Attempt: 1}, MessageID: "msg_x", EnqueuedAt: srv.clock.Now()}}

	status, _, body := srv.get(t, http.MethodGet, deliverPath, bearer(testCronSecret))
	require.Equal(t, http.StatusOK, status, body)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &got), body)
	require.Equal(t, map[string]any{
		"enqueued": 0.0, "gaps": 0.0, "claimed": 1.0, "sent": 0.0, "delivered": 0.0, "retried": 0.0,
		"gave_up": map[string]any{"event_purged": 1.0}, "superseded": 0.0, "incomplete": false,
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
