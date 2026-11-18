package faucet_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"cdr.dev/slog/v3/sloggers/sloghuman"
	"github.com/coder/faucet/faucet"
	"github.com/coder/quartz"
)

// ingestHarness posts signed webhooks to a service whose RawStore is the
// fake, and writes and reads interests through the store's own methods.
type ingestHarness struct {
	*harness
	svc    *faucet.Service
	clock  *quartz.Mock
	ctx    context.Context
	leases atomic.Uint64
}

func newIngestHarness(t *testing.T, mutate ...func(*faucet.Deps)) *ingestHarness {
	t.Helper()
	clock := quartz.NewMock(t)
	store := &fakeStore{now: func() time.Time { return clock.Now() }}
	logs := &bytes.Buffer{}
	deps := testDeps(store)
	deps.RawStore, deps.RawEvents = store, faucet.RawEventNames()
	deps.Clock = clock
	deps.Logger = slog.Make(sloghuman.Sink(logs)).Leveled(slog.LevelDebug)
	for _, m := range mutate {
		m(&deps)
	}
	svc, err := faucet.New(deps)
	require.NoError(t, err)
	return &ingestHarness{
		harness: &harness{t: t, store: store, logs: logs, handler: svc.WebhookHandler()},
		svc:     svc,
		clock:   clock,
		ctx:     context.Background(),
	}
}

// ingestInterest is what a lease or a subscription asks for.
type ingestInterest struct {
	event   string
	repoID  int64
	actions []string
	fields  []string
}

func (w ingestInterest) withDefaults() ingestInterest {
	if w.event == "" {
		w.event = "push"
	}
	if w.repoID == 0 {
		w.repoID = repoID
	}
	return w
}

func (w ingestInterest) arguments() json.RawMessage {
	args := map[string]any{"repo": testRepo}
	if len(w.actions) > 0 {
		args["actions"] = w.actions
	}
	if len(w.fields) > 0 {
		args["fields"] = w.fields
	}
	b, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return b
}

// ingestLease is a lease that the harness opened, with the read that polls it.
type ingestLease struct {
	h     *ingestHarness
	read  faucet.RawRead
	after int64
}

func (h *ingestHarness) openLease(w ingestInterest) *ingestLease {
	h.t.Helper()
	w = w.withDefaults()
	lease := faucet.RawLease{
		Key:       faucet.NewSubscriptionKeyHash("octocat", "", "github."+w.event, w.arguments()),
		Principal: "octocat",
		Event:     w.event,
		Actions:   w.actions,
		Fields:    w.fields,
		RepoID:    w.repoID,
		ExpiresAt: h.clock.Now().Add(time.Hour),
	}
	binary.BigEndian.PutUint64(lease.ID[8:], h.leases.Add(1))
	head, err := h.store.OpenRawLease(h.ctx, lease, h.clock.Now(), faucet.RawLimits{Live: 100, PerTarget: 1000, PerPrincipalTarget: 1000})
	require.NoError(h.t, err)
	return &ingestLease{h: h, after: head, read: faucet.RawRead{
		Lease: lease.ID, Key: lease.Key, RepoID: lease.RepoID, Limit: 2000, MaxBytes: 64 << 20,
	}}
}

// poll reads the lease from where the last poll ended, as a poller does.
func (l *ingestLease) poll() faucet.RawPage {
	l.h.t.Helper()
	q := l.read
	q.After, q.Now, q.ExpiresAt = l.after, l.h.clock.Now(), l.h.clock.Now().Add(time.Hour)
	page, found, err := l.h.store.ReadRawLease(l.h.ctx, q)
	require.NoError(l.h.t, err)
	require.True(l.h.t, found, "lease is live")
	l.after = page.Next
	return page
}

func (h *ingestHarness) subscribe(w ingestInterest) faucet.Subscription {
	h.t.Helper()
	w = w.withDefaults()
	name := "github." + w.event
	url := "https://hooks.example.test/raw/" + strconv.FormatUint(h.leases.Add(1), 10)
	sub, err := h.store.UpsertSubscription(h.ctx, faucet.Subscription{
		KeyHash:     faucet.NewSubscriptionKeyHash("octocat", url, name, w.arguments()),
		Principal:   "octocat",
		CallbackURL: url,
		EventType:   name,
		EventTypes:  []string{name},
		Arguments:   w.arguments(),
		RepoID:      w.repoID,
		SecretEnc:   []byte("sealed"),
		Cursor:      math.MaxInt64,
		ExpiresAt:   h.clock.Now().Add(time.Hour),
	}, h.clock.Now(), 100)
	require.NoError(h.t, err)
	return sub
}

// claims takes every raw delivery that is due, as a delivery run would.
func (h *ingestHarness) claims() []faucet.RawDeliveryClaim {
	h.t.Helper()
	var out []faucet.RawDeliveryClaim
	for {
		c, ok, err := h.store.ClaimRawDelivery(h.ctx, h.clock.Now(), time.Minute, nil)
		require.NoError(h.t, err)
		if !ok {
			return out
		}
		out = append(out, c)
	}
}

func (h *ingestHarness) head() int64 {
	h.t.Helper()
	head, err := h.store.RawHead(h.ctx)
	require.NoError(h.t, err)
	return head
}

func (h *ingestHarness) post(event, delivery string, body []byte) (int, string) {
	h.t.Helper()
	rec := h.serve(newRequest(http.MethodPost, body, event, delivery))
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

// rawPayload is a payload of repoID with action, when it is not empty, and
// the extra members.
func rawPayload(repo int64, action string, extra map[string]any) []byte {
	doc := map[string]any{"repository": map[string]any{"id": repo, "full_name": testRepo}}
	if action != "" {
		doc["action"] = action
	}
	for k, v := range extra {
		doc[k] = v
	}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return b
}

func TestRawIngestMatchOrDrop(t *testing.T) {
	t.Parallel()

	t.Run("NoInterest", func(t *testing.T) {
		t.Parallel()
		h := newIngestHarness(t)
		code, msg := h.post("push", "d-1", rawPayload(repoID, "", nil))
		require.Equal(t, http.StatusAccepted, code)
		require.Equal(t, "ignored", msg)
		require.Zero(t, h.head())
		require.Contains(t, h.logs.String(), "webhook ignored")

		lease := h.openLease(ingestInterest{})
		require.Empty(t, lease.poll().Events)
		code, _ = h.post("push", "d-1", rawPayload(repoID, "", nil))
		require.Equal(t, http.StatusOK, code, "the delivery was never stored, so it is no duplicate")
		require.Len(t, lease.poll().Events, 1)
	})

	t.Run("Lease", func(t *testing.T) {
		t.Parallel()
		h := newIngestHarness(t)
		lease := h.openLease(ingestInterest{})
		body := rawPayload(repoID, "", map[string]any{"ref": "refs/heads/main", "after": "<>&\u0000"})
		code, msg := h.post("push", "d-1", body)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "stored", msg)

		page := lease.poll()
		require.Len(t, page.Events, 1)
		ev := page.Events[0]
		require.Equal(t, "d-1", ev.DeliveryID)
		require.Equal(t, "push", ev.Event)
		require.Equal(t, int64(repoID), ev.RepoID)
		require.Equal(t, h.clock.Now(), ev.ReceivedAt)
		require.Equal(t, body, []byte(ev.Body), "the payload is stored byte for byte")
		require.Nil(t, ev.Fields)
		require.Empty(t, h.store.stored(), "a raw event is no typed event")
	})

	t.Run("Subscription", func(t *testing.T) {
		t.Parallel()
		h := newIngestHarness(t)
		sub := h.subscribe(ingestInterest{})
		body := rawPayload(repoID, "", nil)
		code, _ := h.post("push", "d-1", body)
		require.Equal(t, http.StatusOK, code)

		claims := h.claims()
		require.Len(t, claims, 1)
		require.Equal(t, sub.KeyHash, claims[0].Ref.Key)
		require.NotNil(t, claims[0].Event)
		require.Equal(t, body, []byte(claims[0].Event.Body))
	})

	t.Run("RawOff", func(t *testing.T) {
		t.Parallel()
		h := newIngestHarness(t, func(d *faucet.Deps) { d.RawStore, d.RawEvents = nil, nil })
		h.openLease(ingestInterest{})
		h.subscribe(ingestInterest{})
		code, msg := h.post("push", "d-1", rawPayload(repoID, "", nil))
		require.Equal(t, http.StatusAccepted, code)
		require.Equal(t, "ignored", msg)
		require.Zero(t, h.head())
	})
}

func TestRawIngestMatching(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		interest ingestInterest
		event    string
		body     []byte
		want     bool
	}{
		"OtherRepository":       {ingestInterest{}, "push", rawPayload(otherRepoID, "", nil), false},
		"OtherEvent":            {ingestInterest{event: "issues"}, "push", rawPayload(repoID, "", nil), false},
		"ActionListed":          {ingestInterest{event: "issues", actions: []string{"closed", "opened"}}, "issues", rawPayload(repoID, "opened", nil), true},
		"ActionNotListed":       {ingestInterest{event: "issues", actions: []string{"closed"}}, "issues", rawPayload(repoID, "opened", nil), false},
		"NoActionFilter":        {ingestInterest{event: "issues"}, "issues", rawPayload(repoID, "opened", nil), true},
		"PayloadWithoutAction":  {ingestInterest{actions: []string{"created"}}, "push", rawPayload(repoID, "", nil), false},
		"NoFilterWithoutAction": {ingestInterest{}, "push", rawPayload(repoID, "", nil), true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, viaLease := range []bool{true, false} {
				h := newIngestHarness(t)
				var lease *ingestLease
				if viaLease {
					lease = h.openLease(tt.interest)
				} else {
					h.subscribe(tt.interest)
				}
				code, _ := h.post(tt.event, "d-1", tt.body)
				if !tt.want {
					require.Equal(t, http.StatusAccepted, code)
					require.Zero(t, h.head())
					continue
				}
				require.Equal(t, http.StatusOK, code)
				if viaLease {
					require.Len(t, lease.poll().Events, 1)
				} else {
					require.Len(t, h.claims(), 1)
				}
			}
		})
	}
}

func TestRawIngestTypedStaysTyped(t *testing.T) {
	t.Parallel()

	t.Run("PullRequest", func(t *testing.T) {
		t.Parallel()
		h := newIngestHarness(t)
		h.openLease(ingestInterest{event: "issue_comment", repoID: 186853002})
		h.openLease(ingestInterest{event: "pull_request", repoID: 186853002})
		code, msg := h.post("pull_request", "d-1", fixture(t, "pull_request_opened"))
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "stored", msg)
		require.Len(t, h.store.stored(), 1)
		require.Zero(t, h.head())
	})

	t.Run("CommentOnPullRequest", func(t *testing.T) {
		t.Parallel()
		h := newIngestHarness(t)
		h.openLease(ingestInterest{event: "issue_comment", repoID: 186853002})
		code, _ := h.post("issue_comment", "d-1", fixture(t, "issue_comment_created_on_pr"))
		require.Equal(t, http.StatusOK, code)
		require.Len(t, h.store.stored(), 1)
		require.Equal(t, "pr.comment_created", h.store.stored()[0].Type)
		require.Zero(t, h.head())
	})

	t.Run("CommentOnPlainIssue", func(t *testing.T) {
		t.Parallel()
		h := newIngestHarness(t)
		lease := h.openLease(ingestInterest{event: "issue_comment", repoID: 186853002, actions: []string{"created"}})
		body := fixture(t, "issue_comment_created")
		code, msg := h.post("issue_comment", "d-1", body)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "stored", msg)
		require.Empty(t, h.store.stored())
		page := lease.poll()
		require.Len(t, page.Events, 1)
		require.Equal(t, "created", page.Events[0].Action)
		require.Equal(t, body, []byte(page.Events[0].Body))
	})
}

// A host that checks services by repo grants records the repository of every
// signed delivery, and a service can ask for the raw events of a repository
// only once the store knows it. So a delivery names its repository before the
// handler looks for an interest: one that nobody wants is recorded too, and
// the first poll after it opens the lease that stores the next one.
func TestRawIngestRecordsTheRepository(t *testing.T) {
	t.Parallel()

	recording := func(d *faucet.Deps) { d.RecordRepositories = true }
	named := func(fullName string) map[string]any {
		return map[string]any{"repository": map[string]any{"id": repoID, "full_name": fullName, "owner": map[string]any{"id": 9, "login": "octo"}}}
	}
	recorded := func(fullName string, at time.Time) []fakeRepo {
		return []fakeRepo{{repo: faucet.Repository{ID: repoID, OwnerID: 9, OwnerLogin: "octo", FullName: fullName}, seenAt: at}}
	}

	t.Run("IgnoredThenStored", func(t *testing.T) {
		t.Parallel()
		h := newIngestHarness(t, recording)
		body := rawPayload(repoID, "", named(testRepo))
		first := h.clock.Now()
		code, msg := h.post("push", "d-1", body)
		require.Equal(t, http.StatusAccepted, code)
		require.Equal(t, "ignored", msg)
		require.Zero(t, h.head())
		require.Equal(t, recorded(testRepo, first), h.store.repos, "a delivery that no interest wants names its repository too")

		h.clock.Advance(time.Minute)
		lease := h.openLease(ingestInterest{})
		code, msg = h.post("push", "d-2", body)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "stored", msg)
		page := lease.poll()
		require.Len(t, page.Events, 1)
		require.Equal(t, body, []byte(page.Events[0].Body))
		require.Equal(t, recorded(testRepo, first), h.store.repos, "the repository keeps the row of its first delivery")
	})

	// The Repositories event that renames a granted repository is a raw event
	// too: it gives the row its new name, and a lease of github.repository
	// reads it.
	t.Run("RenameIsRecordedAndStored", func(t *testing.T) {
		t.Parallel()
		h := newIngestHarness(t, recording)
		lease := h.openLease(ingestInterest{event: "repository"})
		code, _ := h.post("push", "d-1", rawPayload(repoID, "", named(testRepo)))
		require.Equal(t, http.StatusAccepted, code, "the lease wants repository events, not pushes")

		h.clock.Advance(time.Minute)
		renamed := named("octo/tap")
		renamed["changes"] = map[string]any{"repository": map[string]any{"name": map[string]any{"from": "repo"}}}
		body := rawPayload(repoID, "renamed", renamed)
		code, msg := h.post("repository", "d-2", body)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "stored", msg)
		require.Equal(t, recorded("octo/tap", h.clock.Now()), h.store.repos)
		page := lease.poll()
		require.Len(t, page.Events, 1)
		require.Equal(t, "renamed", page.Events[0].Action)
		require.Equal(t, body, []byte(page.Events[0].Body))
	})

	t.Run("StoredWhenTheRepositoryWriteFails", func(t *testing.T) {
		t.Parallel()
		h := newIngestHarness(t, recording)
		h.store.repoErr = xerrors.New("connection to db-host-canary refused")
		lease := h.openLease(ingestInterest{})
		code, msg := h.post("push", "d-1", rawPayload(repoID, "", named(testRepo)))
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "stored", msg)
		require.Len(t, lease.poll().Events, 1)
		require.Empty(t, h.store.repos)
	})
}

func TestRawIngestDuplicate(t *testing.T) {
	t.Parallel()
	h := newIngestHarness(t)
	lease := h.openLease(ingestInterest{})
	h.subscribe(ingestInterest{})
	body := rawPayload(repoID, "", nil)
	code, msg := h.post("push", "d-1", body)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "stored", msg)
	code, msg = h.post("push", "d-1", body)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "duplicate", msg)

	require.Equal(t, int64(1), h.head())
	require.Len(t, lease.poll().Events, 1)
	require.Len(t, h.claims(), 1)
}

func TestRawIngestIgnoresPayloadWithoutRepository(t *testing.T) {
	t.Parallel()

	for name, body := range map[string][]byte{
		"Array":          []byte(`[{"repository":{"id":42}}]`),
		"String":         []byte(`"push"`),
		"Null":           []byte(`null`),
		"NotJSON":        []byte(`{"repository":`),
		"NoRepository":   []byte(`{"ref":"refs/heads/main"}`),
		"RepositoryNull": []byte(`{"repository":null}`),
		"NoRepositoryID": []byte(`{"repository":{"name":"repo"}}`),
		"ZeroID":         []byte(`{"repository":{"id":0}}`),
		"IDNotANumber":   []byte(`{"repository":{"id":"42"}}`),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newIngestHarness(t)
			h.openLease(ingestInterest{})
			code, msg := h.post("push", "d-1", body)
			require.Equal(t, http.StatusAccepted, code)
			require.Equal(t, "ignored", msg)
			require.Zero(t, h.head())
		})
	}
}

func TestRawIngestStoreError(t *testing.T) {
	t.Parallel()
	h := newIngestHarness(t)
	h.openLease(ingestInterest{})
	h.store.mu.Lock()
	h.store.err = xerrors.New("database unreachable")
	h.store.mu.Unlock()

	code, msg := h.post("push", "d-1", rawPayload(repoID, "", map[string]any{"secret_value": "body-marker"}))
	require.Equal(t, http.StatusInternalServerError, code)
	require.Equal(t, "could not store event", msg)
	logs := h.logs.String()
	require.Contains(t, logs, "store raw event")
	require.Contains(t, logs, "database unreachable")
	require.NotContains(t, logs, "body-marker", "no payload in a log")
}

func TestRawIngestLogs(t *testing.T) {
	t.Parallel()
	h := newIngestHarness(t)
	h.openLease(ingestInterest{})
	h.subscribe(ingestInterest{})
	code, _ := h.post("push", "d-1", rawPayload(repoID, "", map[string]any{"ref": "body-marker"}))
	require.Equal(t, http.StatusOK, code)

	logs := h.logs.String()
	line := logLine(t, logs, "webhook stored")
	for _, want := range []string{"github.push", "raw_id=1", "deliveries=1", "unread=1", "gaps=0", "lost=0", "d-1"} {
		require.Contains(t, line, want)
	}
	require.NotContains(t, logs, "body-marker")
	require.NotContains(t, logs, "hooks.example.test")
	require.NotContains(t, logs, "[warn]")
}

// logLine returns the one line of logs that holds msg.
func logLine(t *testing.T, logs, msg string) string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, msg) {
			found = append(found, line)
		}
	}
	require.Len(t, found, 1, "lines with %q in:\n%s", msg, logs)
	return found[0]
}

func TestRawIngestSize(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 256<<10)
	body := rawPayload(repoID, "", map[string]any{"ref": "refs/heads/main", "big": big})

	t.Run("LeaseWithoutFields", func(t *testing.T) {
		t.Parallel()
		h := newIngestHarness(t)
		lease := h.openLease(ingestInterest{})
		code, _ := h.post("push", "d-1", body)
		require.Equal(t, http.StatusOK, code)

		line := logLine(t, h.logs.String(), "raw event missed by leases")
		require.Contains(t, line, "[warn]")
		require.Contains(t, line, "count=1")
		require.Contains(t, line, "event_too_large")
		page := lease.poll()
		require.Empty(t, page.Events)
		require.Equal(t, 1, page.Skipped)
	})

	t.Run("LeaseWithSmallFields", func(t *testing.T) {
		t.Parallel()
		h := newIngestHarness(t)
		lease := h.openLease(ingestInterest{fields: []string{"ref"}})
		code, _ := h.post("push", "d-1", body)
		require.Equal(t, http.StatusOK, code)
		page := lease.poll()
		require.Len(t, page.Events, 1)
		require.JSONEq(t, `{"ref":"refs/heads/main"}`, string(page.Events[0].Body))
		require.Equal(t, []string{"ref"}, page.Events[0].Fields)
		require.NotContains(t, h.logs.String(), "[warn]")
	})

	for name, interest := range map[string]ingestInterest{
		"SubscriptionWithoutFields":    {},
		"SubscriptionWithLargeFields":  {fields: []string{"big"}},
		"SubscriptionWithLargeAndMore": {fields: []string{"big", "ref"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newIngestHarness(t)
			sub := h.subscribe(interest)
			code, _ := h.post("push", "d-1", body)
			require.Equal(t, http.StatusOK, code)

			require.Empty(t, h.claims(), "no delivery is queued for an event that the subscription cannot get")
			gap, ok, err := h.store.ClaimRawGap(h.ctx, h.clock.Now(), time.Minute, nil)
			require.NoError(t, err)
			require.True(t, ok, "the subscription is owed a gap")
			require.Equal(t, sub.KeyHash, gap.Ref.Key)
			require.Equal(t, h.head(), gap.Ref.Through, "the gap runs through the event")
			line := logLine(t, h.logs.String(), "raw event missed by subscriptions")
			require.Contains(t, line, "count=1")
			require.Contains(t, line, "event_too_large")
		})
	}

	t.Run("UnionTooLargeServesTheFieldsThatFit", func(t *testing.T) {
		t.Parallel()
		h := newIngestHarness(t)
		large := h.subscribe(ingestInterest{fields: []string{"big", "ref"}})
		small := h.subscribe(ingestInterest{fields: []string{"ref"}})
		lease := h.openLease(ingestInterest{fields: []string{"repository.id"}})
		code, _ := h.post("push", "d-1", body)
		require.Equal(t, http.StatusOK, code)

		claims := h.claims()
		require.Len(t, claims, 1)
		require.Equal(t, small.KeyHash, claims[0].Ref.Key)
		require.Equal(t, []string{"ref", "repository.id"}, claims[0].Event.Fields, "the body holds only the fields of the interests that get the event")
		require.JSONEq(t, `{"ref":"refs/heads/main","repository":{"id":42}}`, string(lease.poll().Events[0].Body))
		gap, ok, err := h.store.ClaimRawGap(h.ctx, h.clock.Now(), time.Minute, nil)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, large.KeyHash, gap.Ref.Key)
	})

	t.Run("UnionWhenEveryInterestHasFields", func(t *testing.T) {
		t.Parallel()
		h := newIngestHarness(t)
		h.subscribe(ingestInterest{fields: []string{"repository.id"}})
		lease := h.openLease(ingestInterest{fields: []string{"ref"}})
		code, _ := h.post("push", "d-1", body)
		require.Equal(t, http.StatusOK, code)
		claims := h.claims()
		require.Len(t, claims, 1)
		require.JSONEq(t, `{"ref":"refs/heads/main","repository":{"id":42}}`, string(claims[0].Event.Body))
		require.Equal(t, []string{"ref", "repository.id"}, claims[0].Event.Fields)
		require.Equal(t, claims[0].Event.Body, lease.poll().Events[0].Body)
	})

	t.Run("WholeWhenOneInterestHasNone", func(t *testing.T) {
		t.Parallel()
		h := newIngestHarness(t)
		small := rawPayload(repoID, "", map[string]any{"ref": "refs/heads/main", "size": 3})
		h.openLease(ingestInterest{fields: []string{"ref"}})
		lease := h.openLease(ingestInterest{})
		code, _ := h.post("push", "d-1", small)
		require.Equal(t, http.StatusOK, code)
		ev := lease.poll().Events[0]
		require.Equal(t, small, []byte(ev.Body))
		require.Nil(t, ev.Fields)
	})
}

func TestRawIngestCaps(t *testing.T) {
	t.Parallel()

	t.Run("Subscription", func(t *testing.T) {
		t.Parallel()
		h := newIngestHarness(t)
		sub := h.subscribe(ingestInterest{})
		for i := range faucet.MaxPendingDeliveries {
			code, _ := h.post("push", "d-"+strconv.Itoa(i), rawPayload(repoID, "", nil))
			require.Equal(t, http.StatusOK, code)
		}
		require.NotContains(t, h.logs.String(), "[warn]")
		code, _ := h.post("push", "d-last", rawPayload(repoID, "", nil))
		require.Equal(t, http.StatusOK, code)

		line := logLine(t, h.logs.String(), "raw event missed by subscriptions")
		require.Contains(t, line, "[warn]")
		require.Contains(t, line, "count=1")
		gap, ok, err := h.store.ClaimRawGap(h.ctx, h.clock.Now(), time.Minute, nil)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, sub.KeyHash, gap.Ref.Key)
		require.Equal(t, h.head(), gap.Ref.Through)
		require.Len(t, h.claims(), faucet.MaxPendingDeliveries)
	})

	t.Run("Lease", func(t *testing.T) {
		t.Parallel()
		h := newIngestHarness(t)
		lease := h.openLease(ingestInterest{})
		const unreadCap = 1000
		for i := range unreadCap {
			code, _ := h.post("push", "d-"+strconv.Itoa(i), rawPayload(repoID, "", nil))
			require.Equal(t, http.StatusOK, code)
		}
		require.NotContains(t, h.logs.String(), "[warn]")
		code, _ := h.post("push", "d-last", rawPayload(repoID, "", nil))
		require.Equal(t, http.StatusOK, code)

		line := logLine(t, h.logs.String(), "raw event missed by leases")
		require.Contains(t, line, "[warn]")
		require.Contains(t, line, "count=1")
		require.Contains(t, line, "unread_limit")
		require.NotContains(t, line, "event_too_large")
		page := lease.poll()
		require.Len(t, page.Events, unreadCap)
		require.Equal(t, 1, page.Skipped)
	})
}
