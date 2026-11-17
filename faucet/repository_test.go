package faucet_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
	"github.com/coder/quartz"
)

func newRepositoryHarness(t *testing.T, mutate ...func(*faucet.Deps)) (*fakeStore, *quartz.Mock, http.Handler) {
	t.Helper()
	store := &fakeStore{}
	clock := quartz.NewMock(t)
	clock.Set(time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC))
	deps := testDeps(store)
	deps.Clock = clock
	deps.RecordRepositories = true
	for _, m := range mutate {
		m(&deps)
	}
	svc, err := faucet.New(deps)
	require.NoError(t, err)
	return store, clock, svc.WebhookHandler()
}

func serveWebhook(handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// A service is granted a repository by name, and faucet learns the name's ID
// from deliveries. Every signed delivery that names a repository records it,
// whether faucet stores the event or not, so a grant works before the first
// stored event.
func TestWebhookRecordsTheRepositoryOfEverySignedDelivery(t *testing.T) {
	t.Parallel()

	octo := faucet.Repository{ID: 300029405, OwnerID: 41548062, OwnerLogin: "octo-org", FullName: "octo-org/octo-repo"}
	hello := faucet.Repository{ID: 186853002, OwnerID: 21031067, OwnerLogin: "Codertocat", FullName: "Codertocat/Hello-World"}
	for _, c := range []struct {
		name, event string
		body        func(t *testing.T) []byte
		status      int
		want        faucet.Repository
	}{
		{"Stored", "workflow_run", func(t *testing.T) []byte { return fixture(t, "workflow_run_completed") }, http.StatusOK, octo},
		{"IgnoredEvent", "push", func(*testing.T) []byte {
			return []byte(`{"ref":"refs/heads/main","repository":{"id":5,"full_name":"coder/faucet","owner":{"id":9,"login":"coder"}}}`)
		}, http.StatusAccepted, faucet.Repository{ID: 5, OwnerID: 9, OwnerLogin: "coder", FullName: "coder/faucet"}},
		{"IgnoredAction", "check_run", func(t *testing.T) []byte {
			return mutate(t, "check_run_completed", func(doc map[string]any) { doc["action"] = "rerequested" })
		}, http.StatusAccepted, hello},
		{"Ping", "ping", func(*testing.T) []byte {
			return []byte(`{"zen":"Keep it logically awesome.","hook_id":1,"repository":{"id":5,"full_name":"coder/faucet","owner":{"id":9,"login":"coder"}}}`)
		}, http.StatusOK, faucet.Repository{ID: 5, OwnerID: 9, OwnerLogin: "coder", FullName: "coder/faucet"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			store, clock, handler := newRepositoryHarness(t)

			rec := serveWebhook(handler, newRequest(http.MethodPost, c.body(t), c.event, "d-1"))
			require.Equal(t, c.status, rec.Code, rec.Body.String())
			require.Equal(t, []fakeRepo{{repo: c.want, seenAt: clock.Now()}}, store.repos)
		})
	}
}

func TestWebhookRecordsNoRepositoryThatADeliveryDoesNotName(t *testing.T) {
	t.Parallel()

	body := fixture(t, "workflow_run_completed")
	unsigned := newRequest(http.MethodPost, body, "workflow_run", "d-1")
	unsigned.Header.Set("X-Hub-Signature-256", sign("another-secret-0123456789", body))
	for name, req := range map[string]*http.Request{
		"Unsigned":        unsigned,
		"NoRepository":    newRequest(http.MethodPost, []byte(`{"action":"member_added","organization":{"id":1}}`), "organization", "d-1"),
		"PingOfAnOrgHook": newRequest(http.MethodPost, []byte(`{"zen":"Speak like a human.","hook_id":1}`), "ping", "d-1"),
		"RepositoryWithoutAnID": newRequest(http.MethodPost,
			[]byte(`{"ref":"refs/heads/main","repository":{"full_name":"coder/faucet","owner":{"id":9,"login":"coder"}}}`), "push", "d-1"),
		"RepositoryOfAnotherShape": newRequest(http.MethodPost, []byte(`{"ref":"refs/heads/main","repository":"coder/faucet"}`), "push", "d-1"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store, _, handler := newRepositoryHarness(t)
			serveWebhook(handler, req)
			require.Empty(t, store.repos)
		})
	}
}

// A failed write of the repository fails no delivery: the event is stored or
// ignored as before, and the next delivery tries again.
func TestWebhookGoesOnWhenTheRepositoryWriteFails(t *testing.T) {
	t.Parallel()

	repoOf := `"repository":{"id":5,"full_name":"coder/faucet","owner":{"id":9,"login":"coder"}}`
	for _, c := range []struct {
		name, event string
		body        func(t *testing.T) []byte
		status      int
		reply       string
		stored      int
	}{
		{"Stored", "workflow_run", func(t *testing.T) []byte { return fixture(t, "workflow_run_completed") }, http.StatusOK, "stored\n", 1},
		{"Ping", "ping", func(*testing.T) []byte { return []byte(`{"zen":"z","hook_id":1,` + repoOf + `}`) }, http.StatusOK, "pong\n", 0},
		{"Ignored", "push", func(*testing.T) []byte { return []byte(`{"ref":"refs/heads/main",` + repoOf + `}`) }, http.StatusAccepted, "ignored\n", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			store, _, handler := newRepositoryHarness(t)
			store.repoErr = xerrors.New("connection to db-host-canary refused")

			rec := serveWebhook(handler, newRequest(http.MethodPost, c.body(t), c.event, "d-1"))
			require.Equal(t, c.status, rec.Code)
			require.Equal(t, c.reply, rec.Body.String())
			require.Len(t, store.stored(), c.stored)
		})
	}
}

// Only a host that checks services by their grants needs the repositories, so
// a host that does not record them writes nothing.
func TestWebhookRecordsNoRepositoryUnlessTheHostAsks(t *testing.T) {
	t.Parallel()
	store, _, handler := newRepositoryHarness(t, func(d *faucet.Deps) { d.RecordRepositories = false })
	rec := serveWebhook(handler, newRequest(http.MethodPost, fixture(t, "workflow_run_completed"), "workflow_run", "d-1"))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, store.repos)
	require.Zero(t, store.repoWrites)
}

// A repository keeps the names it was first seen under, so a replayed or late
// delivery cannot move a name back to it. Only a repository event that
// renames or transfers it gives it new names.
func TestWebhookRenamesARepositoryOnlyOnARenameOrTransfer(t *testing.T) {
	t.Parallel()
	store, clock, handler := newRepositoryHarness(t)
	first := clock.Now()
	deliver := func(event, action, fullName, owner string) {
		t.Helper()
		body := `{"action":"` + action + `","ref":"refs/heads/main","repository":{"id":5,"full_name":"` + fullName + `","owner":{"id":9,"login":"` + owner + `"}}}`
		serveWebhook(handler, newRequest(http.MethodPost, []byte(body), event, "d-"+action+fullName))
		clock.Advance(time.Minute)
	}
	named := func(fullName, owner string, at time.Time) []fakeRepo {
		return []fakeRepo{{repo: faucet.Repository{ID: 5, OwnerID: 9, OwnerLogin: owner, FullName: fullName}, seenAt: at}}
	}

	deliver("push", "", "coder/faucet", "coder")
	require.Equal(t, named("coder/faucet", "coder", first), store.repos)
	deliver("push", "", "coder/old-name", "coder")
	require.Equal(t, named("coder/faucet", "coder", first), store.repos, "a replayed delivery leaves the names alone")
	deliver("repository", "edited", "coder/other", "coder")
	require.Equal(t, named("coder/faucet", "coder", first), store.repos, "a repository event that neither renames nor transfers leaves them too")

	renamedAt := clock.Now()
	deliver("repository", "renamed", "coder/tap", "coder")
	require.Equal(t, named("coder/tap", "coder", renamedAt), store.repos)
	movedAt := clock.Now()
	deliver("repository", "transferred", "acme/tap", "acme")
	require.Equal(t, named("acme/tap", "acme", movedAt), store.repos)
}
