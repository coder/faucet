package faucet_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
// renames or transfers it from the name that faucet holds gives it new
// names, so a replayed old rename cannot either.
func TestWebhookRenamesARepositoryOnlyFromTheNameItHolds(t *testing.T) {
	t.Parallel()
	store, clock, handler := newRepositoryHarness(t)
	first := clock.Now()
	n := 0
	deliver := func(event, action, fullName, owner, changes string) {
		t.Helper()
		body := `{"action":"` + action + `","ref":"refs/heads/main",` + changes +
			`"repository":{"id":5,"name":"` + fullName[strings.Index(fullName, "/")+1:] + `","full_name":"` + fullName + `","owner":{"id":9,"login":"` + owner + `"}}}`
		n++
		serveWebhook(handler, newRequest(http.MethodPost, []byte(body), event, fmt.Sprintf("d-%d", n)))
		clock.Advance(time.Minute)
	}
	renamedFrom := func(name string) string { return `"changes":{"repository":{"name":{"from":"` + name + `"}}},` }
	movedFrom := func(org string) string {
		return `"changes":{"owner":{"from":{"organization":{"login":"` + org + `"}}}},`
	}
	named := func(fullName, owner string, at time.Time) []fakeRepo {
		return []fakeRepo{{repo: faucet.Repository{ID: 5, OwnerID: 9, OwnerLogin: owner, FullName: fullName}, seenAt: at}}
	}

	deliver("push", "", "coder/faucet", "coder", "")
	require.Equal(t, named("coder/faucet", "coder", first), store.repos)
	deliver("push", "", "coder/old-name", "coder", "")
	require.Equal(t, named("coder/faucet", "coder", first), store.repos, "a replayed delivery leaves the names alone")
	deliver("repository", "edited", "coder/other", "coder", "")
	require.Equal(t, named("coder/faucet", "coder", first), store.repos, "a repository event that neither renames nor transfers leaves them too")
	deliver("repository", "renamed", "coder/tap", "coder", "")
	require.Equal(t, named("coder/faucet", "coder", first), store.repos, "a rename that does not say from what changes nothing")

	renamedAt := clock.Now()
	deliver("repository", "renamed", "coder/tap", "coder", renamedFrom("Faucet"))
	require.Equal(t, named("coder/tap", "coder", renamedAt), store.repos)
	secondAt := clock.Now()
	deliver("repository", "renamed", "coder/spout", "coder", renamedFrom("tap"))
	require.Equal(t, named("coder/spout", "coder", secondAt), store.repos)
	deliver("repository", "renamed", "coder/tap", "coder", renamedFrom("faucet"))
	require.Equal(t, named("coder/spout", "coder", secondAt), store.repos, "a redelivered older rename does not revert the newer one")

	movedAt := clock.Now()
	deliver("repository", "transferred", "acme/spout", "acme", movedFrom("coder"))
	require.Equal(t, named("acme/spout", "acme", movedAt), store.repos)
	deliver("repository", "transferred", "acme/spout", "acme", movedFrom("coder"))
	require.Equal(t, named("acme/spout", "acme", movedAt), store.repos, "a replayed transfer finds the new name and changes nothing")
}

// A delivery that faucet's redelivery asked GitHub to send again names an
// account as it was when the delivery first came. It adds an account that
// faucet has not seen and moves no login, so a replay from before a rename
// never gives the old login back. When faucet cannot tell whether a delivery
// is such a replay, it moves no login either.
func TestARedeliveredDeliveryMovesNoLogin(t *testing.T) {
	t.Parallel()
	redelivered := map[string]bool{"before-rename": true, "first-of-octo": true}
	store, _, handler := newRepositoryHarness(t, func(d *faucet.Deps) {
		d.Redelivered = func(_ context.Context, id string) (bool, error) {
			if id == "unknown" {
				return false, xerrors.New("read redeliveries: connection reset")
			}
			return redelivered[id], nil
		}
	})
	deliver := func(event, delivery, body string) {
		t.Helper()
		rec := serveWebhook(handler, newRequest(http.MethodPost, []byte(body), event, delivery))
		require.Less(t, rec.Code, 300, delivery)
	}
	push := func(delivery string, ownerID int64, login string) {
		t.Helper()
		deliver("push", delivery, fmt.Sprintf(`{"ref":"refs/heads/main","repository":{"id":%d,"full_name":%q,"owner":{"id":%d,"login":%q}}}`, ownerID*10, login+"/private", ownerID, login))
	}
	owner := func(login string) (faucet.Owner, error) {
		t.Helper()
		return store.OwnerByLogin(context.Background(), login)
	}

	push("first", 7, "acme")
	deliver("organization", "renamed", `{"action":"renamed","organization":{"id":7,"login":"acme-legacy"},"changes":{"login":{"from":"acme"}}}`)
	push("before-rename", 7, "acme")
	push("unknown", 7, "acme")
	_, err := owner("acme")
	require.ErrorIs(t, err, faucet.ErrUnknownOwner, "a replay from before the rename does not give the old login back")
	got, err := owner("acme-legacy")
	require.NoError(t, err)
	require.Equal(t, faucet.Owner{ID: 7, Login: "acme-legacy"}, got)

	push("first-of-octo", 9, "octo")
	got, err = owner("octo")
	require.NoError(t, err)
	require.Equal(t, faucet.Owner{ID: 9, Login: "octo"}, got, "a replay adds an account that faucet has not seen")
}
