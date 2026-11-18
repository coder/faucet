package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth/extauth"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"cdr.dev/slog/v3/sloggers/sloghuman"
	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/pgstore"
	"github.com/coder/faucet/faucet/redelivery"
	"github.com/coder/quartz"
)

const rawPush = `{"ref":"refs/heads/main","repository":{"id":42,"full_name":"o/r","owner":{"id":7,"login":"o"}}}`

// rawHooks is a webhook whose delivery list holds one push of repository 42
// that faucet failed a minute ago.
type rawHooks struct {
	failed redelivery.HookDelivery
	mu     sync.Mutex
	asked  []int64
}

func newRawHooks() *rawHooks {
	return &rawHooks{failed: redelivery.HookDelivery{
		ID: 31, GUID: "failed-push", DeliveredAt: time.Now().Add(-time.Minute),
		StatusCode: http.StatusInternalServerError, Event: "push", RepositoryID: 42,
	}}
}

func (*rawHooks) Hook() string { return "/repos/o/r/hooks/7" }

func (h *rawHooks) ListDeliveries(context.Context, string) (redelivery.DeliveryPage, error) {
	return redelivery.DeliveryPage{Deliveries: []redelivery.HookDelivery{h.failed}, At: time.Now()}, nil
}

func (h *rawHooks) Redeliver(_ context.Context, attemptID int64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.asked = append(h.asked, attemptID)
	return nil
}

func (h *rawHooks) askedFor() []int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int64(nil), h.asked...)
}

// rawServer is the server as run wires it, over a Postgres database of its
// own: newServices builds the service and the redelivery runner.
type rawServer struct {
	*testServer
	db          *sql.DB
	redeliverer *redelivery.Runner
	hooks       *rawHooks
}

func newRawServer(t *testing.T) *rawServer {
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

	github := newGitHubStub(t)
	authz := faucet.TokenAuthorizer{BaseURL: github.URL, Client: github.Client()}
	hooks := newRawHooks()
	cfg := config{secret: testSecret, cronSecret: testCronSecret, retention: 30 * 24 * time.Hour, hookAdmin: hooks, rawEvents: []string{"push"}}
	clock := quartz.NewReal()
	logger := slog.Make(sloghuman.Sink(&syncBuffer{}))
	svc, redeliverer, err := newServices(cfg, pgstore.New(db), authz, clock, logger)
	require.NoError(t, err)
	cron := cronRoutes{secret: []byte(cfg.cronSecret), svc: svc, retention: cfg.retention, redeliverer: redeliverer, clock: clock, logger: logger}
	srv := httptest.NewServer(newRouter(svc, bearers{github: authz}, nil, cron, logger))
	t.Cleanup(srv.Close)
	return &rawServer{
		testServer:  &testServer{Server: srv, github: github, mcpPath: "/github/mcp"},
		db:          db,
		redeliverer: redeliverer,
		hooks:       hooks,
	}
}

func (s *rawServer) do(t *testing.T, req *http.Request) (status int, body string) {
	t.Helper()
	resp, err := s.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(b)
}

// deliver posts a push of repository 42 with GitHub's signature.
func (s *rawServer) deliver(ctx context.Context, t *testing.T, delivery string) (status int, body string) {
	t.Helper()
	return deliverPush(ctx, t, s.Server, delivery)
}

// deliverPush posts rawPush to srv with GitHub's signature.
func deliverPush(ctx context.Context, t *testing.T, srv *httptest.Server, delivery string) (status int, body string) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(testSecret))
	_, _ = mac.Write([]byte(rawPush))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/webhook/github", bytes.NewReader([]byte(rawPush)))
	require.NoError(t, err)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", delivery)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(b)
}

func (s *rawServer) rawEvents(ctx context.Context, t *testing.T) (count int) {
	t.Helper()
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM faucet_raw_events`).Scan(&count))
	return count
}

type rawPollPage struct {
	Events []struct {
		EventID string          `json:"eventId"`
		Name    string          `json:"name"`
		Data    json.RawMessage `json:"data"`
	}
	Cursor    string
	Truncated bool
}

func pollRawPush(ctx context.Context, t *testing.T, session *mcp.ClientSession, cursor *string) rawPollPage {
	t.Helper()
	fields, err := callEvents(ctx, session, "events/poll", &eventsParams{Name: "github.push", Arguments: map[string]any{"repo": "o/r"}, Cursor: cursor})
	require.NoError(t, err)
	var page rawPollPage
	require.NoError(t, json.Unmarshal(fields["events"], &page.Events))
	require.NoError(t, json.Unmarshal(fields["cursor"], &page.Cursor))
	require.NoError(t, json.Unmarshal(fields["truncated"], &page.Truncated))
	return page
}

func TestTheServerStoresARawEventOnlyWhileAPollerWantsIt(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	srv := newRawServer(t)

	status, body := srv.deliver(ctx, t, "nobody-asked")
	require.Equal(t, http.StatusAccepted, status, body)
	require.Zero(t, srv.rawEvents(ctx, t), "a push that no interest wants is not stored")

	session := srv.connectEvents(ctx, t, aliceToken)
	list, err := callEvents(ctx, session, "events/list", nil)
	require.NoError(t, err)
	var events []struct {
		Name string `json:"name"`
	}
	require.NoError(t, json.Unmarshal(list["events"], &events))
	require.Len(t, events, 15+1, "the server offers the raw names that FAUCET_RAW_EVENTS lists after the typed ones")
	require.Equal(t, "pr.review_submitted", events[14].Name)
	require.Equal(t, "github.push", events[15].Name)

	opened := pollRawPush(ctx, t, session, nil)
	require.Empty(t, opened.Events)
	require.False(t, opened.Truncated)

	status, body = srv.deliver(ctx, t, "wanted")
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "stored\n", body)
	status, body = srv.deliver(ctx, t, "wanted")
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "duplicate\n", body)

	page := pollRawPush(ctx, t, session, &opened.Cursor)
	require.Len(t, page.Events, 1)
	require.Equal(t, "wanted", page.Events[0].EventID)
	require.Equal(t, "github.push", page.Events[0].Name)
	require.JSONEq(t, rawPush, string(page.Events[0].Data))
	require.False(t, page.Truncated)
	require.Empty(t, pollRawPush(ctx, t, session, &page.Cursor).Events, "the second poll acknowledges the event")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+purgePath, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testCronSecret)
	status, body = srv.do(t, req)
	require.Equal(t, http.StatusOK, status, body)
	var reply map[string]int64
	require.NoError(t, json.Unmarshal([]byte(body), &reply), body)
	require.Equal(t, map[string]int64{
		"deleted": 0, "purged_through": 0, "expired_subscriptions": 0, "ended_deliveries": 0,
		"raw_events_cleared": 1, "raw_events_deleted": 0, "expired_leases": 0,
	}, reply)
	var held int
	require.NoError(t, srv.db.QueryRowContext(ctx, `SELECT count(*) FROM faucet_raw_events WHERE body IS NOT NULL`).Scan(&held))
	require.Zero(t, held, "the payload is gone once its only reader has it")
	require.Equal(t, 1, srv.rawEvents(ctx, t), "the delivery ID stays, so a redelivery is a duplicate")
}

func TestTheServerAsksAgainForAFailedDeliveryOnlyWhileAnInterestWantsIt(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	unwanted := newRawServer(t)
	res, err := unwanted.redeliverer.Run(ctx)
	require.NoError(t, err)
	require.Zero(t, res.Tracked, "no interest wants pushes, so a failed one is not tracked")
	require.Empty(t, unwanted.hooks.askedFor())

	wanted := newRawServer(t)
	pollRawPush(ctx, t, wanted.connectEvents(ctx, t, aliceToken), nil)
	res, err = wanted.redeliverer.Run(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, res.Tracked, "a poll lease wants the pushes of the repository")
	require.Equal(t, 1, res.Asked)
	require.Equal(t, []int64{31}, wanted.hooks.askedFor())
}

// connectServiceEvents logs in as connectService does, with a client that
// sends events/poll.
func (s *loginServer) connectServiceEvents(ctx context.Context, t *testing.T, id, secret string) *mcp.ClientSession {
	t.Helper()
	handler, err := extauth.NewClientCredentialsHandler(&extauth.ClientCredentialsHandlerConfig{
		Credentials: &oauthex.ClientCredentials{ClientID: id, ClientSecretAuth: &oauthex.ClientSecretAuth{ClientSecret: secret}},
		HTTPClient:  s.Client(),
	})
	require.NoError(t, err)
	client := mcp.NewClient(&mcp.Implementation{Name: "service-agent", Version: "0"}, nil)
	require.NoError(t, mcp.AddSendingCustomMethod[*eventsParams, *eventsResult](client, "events/poll"))
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             s.URL + "/github/mcp",
		HTTPClient:           &http.Client{Transport: eventNameTransport{next: s.Client().Transport}},
		OAuthHandler:         handler,
		DisableStandaloneSSE: true,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// A service is granted a repository by name, and faucet learns the name's ID
// from the deliveries of the repository. A raw delivery that nobody wants is
// not stored, and it still names the repository, so a service that reads only
// raw events can start: its first poll after that delivery opens the lease
// that makes faucet store the next one.
func TestAServiceReadsRawEventsOnceAnIgnoredDeliveryNamedTheRepository(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	srv := newLoginServer(t)
	secret := srv.client(t, "push-watcher", "/github/mcp", []faucet.Scope{faucet.ScopeEventsRead}, "repo:o/r")
	session := srv.connectServiceEvents(ctx, t, "push-watcher", secret)

	_, err := callEvents(ctx, session, "events/poll", &eventsParams{Name: "github.push", Arguments: map[string]any{"repo": "o/r"}})
	var denied *jsonrpc.Error
	require.ErrorAs(t, err, &denied)
	require.EqualValues(t, -32024, denied.Code)
	require.Contains(t, denied.Message, "faucet has received no webhook delivery from o/r yet")

	status, body := deliverPush(ctx, t, srv.Server, "nobody-asked")
	require.Equal(t, http.StatusAccepted, status, body)
	require.Equal(t, "ignored\n", body)

	opened := pollRawPush(ctx, t, session, nil)
	require.Empty(t, opened.Events, "the lease starts after the delivery that nobody wanted")

	status, body = deliverPush(ctx, t, srv.Server, "wanted")
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "stored\n", body)

	page := pollRawPush(ctx, t, session, &opened.Cursor)
	require.Len(t, page.Events, 1)
	require.Equal(t, "wanted", page.Events[0].EventID)
	require.JSONEq(t, rawPush, string(page.Events[0].Data))
	require.False(t, page.Truncated)

	paths, _ := srv.github.seen()
	require.Empty(t, paths, "a service never reaches GitHub")
	requireNoTokenPiece(t, srv.logs.String(), []string{secret})
}

func TestLoadConfigRawEvents(t *testing.T) {
	t.Parallel()

	load := func(events string) (config, error) {
		return loadConfig(func(name string) string {
			return map[string]string{envDatabaseURL: "postgres://localhost/faucet", envSecret: testSecret, envRawEvents: events}[name]
		})
	}
	cfg, err := load("")
	require.NoError(t, err)
	require.Empty(t, cfg.rawEvents, "unset offers no raw event")

	cfg, err = load("push, release,issues")
	require.NoError(t, err)
	require.Equal(t, []string{"push", "release", "issues"}, cfg.rawEvents)

	for _, events := range []string{"push,repository_advisory", "secret_scanning_alert", "member"} {
		_, err = load(events)
		require.ErrorContains(t, err, envRawEvents, events)
		require.ErrorContains(t, err, "more than read access", events)
	}
	for _, events := range []string{"pushes", "push,", "github.push", "pull_request"} {
		_, err = load(events)
		require.ErrorContains(t, err, envRawEvents, events)
		require.ErrorContains(t, err, "not a GitHub event that faucet can offer raw", events)
	}
}
