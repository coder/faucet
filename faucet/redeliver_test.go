package faucet_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
	"github.com/coder/quartz"
)

type fakeHookAdmin struct {
	mu          sync.Mutex
	clock       quartz.Clock
	pages       map[string]faucet.DeliveryPage
	endless     *faucet.HookDelivery
	listCalls   []string
	redelivered []int64
	failOn      int
	listErr     error
	// githubRedelivers makes a redelivery request add a new successful attempt
	// with the same GUID, the way GitHub does once faucet answers 200.
	githubRedelivers bool
}

func (h *fakeHookAdmin) ListDeliveries(_ context.Context, cursor string) (faucet.DeliveryPage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.listCalls = append(h.listCalls, cursor)
	if h.listErr != nil {
		return faucet.DeliveryPage{}, h.listErr
	}
	if h.endless != nil {
		if len(h.listCalls) > 5*faucet.MaxRedeliverPages {
			return faucet.DeliveryPage{}, xerrors.New("the run did not stop paging")
		}
		d := *h.endless
		d.ID += int64(len(h.listCalls))
		d.GUID = fmt.Sprintf("%s-%d", d.GUID, d.ID)
		return faucet.DeliveryPage{Deliveries: []faucet.HookDelivery{d}, Next: fmt.Sprintf("page-%d", len(h.listCalls))}, nil
	}
	page, ok := h.pages[cursor]
	if !ok {
		return faucet.DeliveryPage{}, xerrors.Errorf("no page for cursor %q", cursor)
	}
	return page, nil
}

func (h *fakeHookAdmin) Redeliver(_ context.Context, attemptID int64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.redelivered = append(h.redelivered, attemptID)
	if h.failOn != 0 && len(h.redelivered) == h.failOn {
		return xerrors.New("GitHub answered the redelivery request with status 422")
	}
	if h.githubRedelivers {
		h.addSuccessLocked(attemptID)
	}
	return nil
}

func (h *fakeHookAdmin) addSuccessLocked(attemptID int64) {
	for cursor, page := range h.pages {
		for _, d := range page.Deliveries {
			if d.ID != attemptID {
				continue
			}
			retry := faucet.HookDelivery{ID: attemptID + 10_000, GUID: d.GUID, DeliveredAt: h.clock.Now(), StatusCode: 200}
			page.Deliveries = append([]faucet.HookDelivery{retry}, page.Deliveries...)
			h.pages[cursor] = page
			return
		}
	}
}

func (h *fakeHookAdmin) requested() []int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int64(nil), h.redelivered...)
}

func (h *fakeHookAdmin) lists() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.listCalls...)
}

type redeliverHarness struct {
	svc   *faucet.Service
	store *fakeStore
	clock *quartz.Mock
	hooks *fakeHookAdmin
	since time.Time
}

func newRedeliverHarness(t *testing.T) *redeliverHarness {
	t.Helper()
	clock := quartz.NewMock(t)
	store := &fakeStore{now: func() time.Time { return clock.Now() }}
	hooks := &fakeHookAdmin{clock: clock, pages: map[string]faucet.DeliveryPage{}}
	deps := testDeps(store)
	deps.Clock = clock
	deps.HookAdmin = hooks
	svc, err := faucet.New(deps)
	require.NoError(t, err)
	return &redeliverHarness{svc: svc, store: store, clock: clock, hooks: hooks, since: clock.Now().Add(-time.Hour)}
}

func (h *redeliverHarness) delivery(id int64, guid string, ago time.Duration, status int) faucet.HookDelivery {
	return faucet.HookDelivery{ID: id, GUID: guid, DeliveredAt: h.clock.Now().Add(-ago), StatusCode: status}
}

func (h *redeliverHarness) store1(t *testing.T, guid string) {
	t.Helper()
	_, inserted, err := h.store.InsertEvent(context.Background(), faucet.Event{
		Source: "github", DeliveryID: guid, Type: "pr.closed", RepoID: repoID, Data: []byte(`{"merged":true}`),
	})
	require.NoError(t, err)
	require.True(t, inserted)
}

func TestRedeliverRequestsExactlyTheFailedUnstoredDeliveriesWithoutALaterSuccess(t *testing.T) {
	t.Parallel()

	h := newRedeliverHarness(t)
	minute := time.Minute
	h.hooks.pages[""] = faucet.DeliveryPage{Deliveries: []faucet.HookDelivery{
		h.delivery(100, "two-failures", 5*minute, 502),
		h.delivery(99, "retried", 10*minute, 200),
		h.delivery(98, "fine", 12*minute, 200),
		h.delivery(97, "stored", 15*minute, 401),
		h.delivery(96, "no-response", 20*minute, 0),
		h.delivery(95, "malformed", 25*minute, 400),
		h.delivery(90, "two-failures", 30*minute, 500),
		h.delivery(94, "retried", 40*minute, 500),
		h.delivery(93, "success-then-failure", 42*minute, 500),
		h.delivery(92, "success-then-failure", 50*minute, 200),
		h.delivery(80, "too-old", 2*time.Hour, 500),
	}}
	h.store1(t, "stored")

	res, err := h.svc.Redeliver(context.Background(), h.since)
	require.NoError(t, err)

	require.Equal(t, []int64{93, 95, 96, 100}, h.hooks.requested(),
		"oldest first, one request per delivery, the newest failed attempt of each")
	require.Equal(t, faucet.RedeliverResult{Examined: 10, Pending: 4, Requested: 4}, res)
}

func TestRedeliverReadsPagesUntilTheWindowEnds(t *testing.T) {
	t.Parallel()

	h := newRedeliverHarness(t)
	h.hooks.pages[""] = faucet.DeliveryPage{Deliveries: []faucet.HookDelivery{h.delivery(30, "newest", time.Minute, 500)}, Next: "c1"}
	h.hooks.pages["c1"] = faucet.DeliveryPage{Deliveries: []faucet.HookDelivery{
		h.delivery(20, "inside", 50*time.Minute, 500),
		h.delivery(10, "outside", 70*time.Minute, 500),
	}, Next: "c2"}

	res, err := h.svc.Redeliver(context.Background(), h.since)
	require.NoError(t, err)

	require.Equal(t, []string{"", "c1"}, h.hooks.lists(), "page c2 is older than the window and is never read")
	require.Equal(t, []int64{20, 30}, h.hooks.requested())
	require.False(t, res.Incomplete)
}

func TestRedeliverReadsToTheLastPageWhenNothingIsOlderThanTheWindow(t *testing.T) {
	t.Parallel()

	h := newRedeliverHarness(t)
	h.hooks.pages[""] = faucet.DeliveryPage{Deliveries: []faucet.HookDelivery{h.delivery(3, "a", time.Minute, 500)}, Next: "c1"}
	h.hooks.pages["c1"] = faucet.DeliveryPage{Deliveries: []faucet.HookDelivery{h.delivery(2, "b", 2*time.Minute, 500)}}

	res, err := h.svc.Redeliver(context.Background(), h.since)
	require.NoError(t, err)
	require.Equal(t, []string{"", "c1"}, h.hooks.lists())
	require.Equal(t, []int64{2, 3}, h.hooks.requested())
	require.False(t, res.Incomplete, "the last page has no next cursor, so the whole window was read")
}

func TestRedeliverStopsAtItsPageBound(t *testing.T) {
	t.Parallel()

	h := newRedeliverHarness(t)
	ok := h.delivery(1000, "ok", time.Minute, 200)
	h.hooks.endless = &ok

	res, err := h.svc.Redeliver(context.Background(), h.since)
	require.NoError(t, err)
	require.Len(t, h.hooks.lists(), faucet.MaxRedeliverPages)
	require.True(t, res.Incomplete, "a run that stopped before the window ended says so")
	require.Zero(t, res.Requested)
}

func TestRedeliverStopsAtItsRequestBoundAndTheNextRunContinues(t *testing.T) {
	t.Parallel()

	h := newRedeliverHarness(t)
	h.hooks.githubRedelivers = true
	total := faucet.MaxRedeliveries + 20
	deliveries := make([]faucet.HookDelivery, total)
	for i := range deliveries {
		deliveries[i] = h.delivery(int64(total-i), fmt.Sprintf("guid-%03d", total-i), time.Duration(i+1)*time.Second, 500)
	}
	h.hooks.pages[""] = faucet.DeliveryPage{Deliveries: deliveries}

	first, err := h.svc.Redeliver(context.Background(), h.since)
	require.NoError(t, err)
	require.Len(t, h.hooks.requested(), faucet.MaxRedeliveries)
	require.Equal(t, faucet.RedeliverResult{Examined: total, Pending: total, Requested: faucet.MaxRedeliveries, Incomplete: true}, first)
	oldest := make([]int64, faucet.MaxRedeliveries)
	for i := range oldest {
		oldest[i] = int64(i + 1)
	}
	require.Equal(t, oldest, h.hooks.requested(), "the oldest deliveries go first")

	second, err := h.svc.Redeliver(context.Background(), h.since)
	require.NoError(t, err)
	require.Equal(t, 20, second.Requested, "the next run takes what the first left")
	require.False(t, second.Incomplete)
	require.Len(t, h.hooks.requested(), total)

	third, err := h.svc.Redeliver(context.Background(), h.since)
	require.NoError(t, err)
	require.Zero(t, third.Requested, "every delivery now has a later success")
}

func TestRedeliverStopsAtTheFirstFailedRequest(t *testing.T) {
	t.Parallel()

	h := newRedeliverHarness(t)
	h.hooks.failOn = 2
	h.hooks.pages[""] = faucet.DeliveryPage{Deliveries: []faucet.HookDelivery{
		h.delivery(3, "c", time.Minute, 500), h.delivery(2, "b", 2*time.Minute, 500), h.delivery(1, "a", 3*time.Minute, 500),
	}}

	res, err := h.svc.Redeliver(context.Background(), h.since)
	require.Error(t, err)
	require.Equal(t, []int64{1, 2}, h.hooks.requested(), "a wrong token must not fail once per delivery")
	require.Equal(t, 1, res.Requested, "only the request GitHub accepted counts")
}

func TestRedeliverRefusesWhatItCannotDo(t *testing.T) {
	t.Parallel()

	h := newRedeliverHarness(t)
	for name, since := range map[string]time.Time{"the zero time": {}, "a time in the future": h.clock.Now().Add(time.Second)} {
		_, err := h.svc.Redeliver(context.Background(), since)
		require.Error(t, err, name)
		require.Empty(t, h.hooks.lists(), "%s: GitHub is not called", name)
	}

	deps := testDeps(&fakeStore{})
	svc, err := faucet.New(deps)
	require.NoError(t, err)
	_, err = svc.Redeliver(context.Background(), time.Now().Add(-time.Hour))
	require.ErrorIs(t, err, faucet.ErrNoHookAdmin)
}

func TestRedeliverReturnsGitHubAndStoreErrors(t *testing.T) {
	t.Parallel()

	h := newRedeliverHarness(t)
	h.hooks.listErr = xerrors.New("GitHub answered the delivery list with status 401")
	_, err := h.svc.Redeliver(context.Background(), h.since)
	require.ErrorIs(t, err, h.hooks.listErr)

	h = newRedeliverHarness(t)
	h.hooks.pages[""] = faucet.DeliveryPage{Deliveries: []faucet.HookDelivery{h.delivery(1, "a", time.Minute, 500)}}
	h.store.err = xerrors.New("database is down")
	_, err = h.svc.Redeliver(context.Background(), h.since)
	require.ErrorIs(t, err, h.store.err)
	require.Empty(t, h.hooks.requested(), "without the stored set a delivery is not redelivered")
}
