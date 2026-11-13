package faucet_test

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
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
			retry := faucet.HookDelivery{ID: attemptID + 10_000, GUID: d.GUID, DeliveredAt: h.clock.Now(), StatusCode: 200, Redelivery: true}
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

func TestRedeliverAsksForADeliveryOnlyWhileItsOriginalIsInTheWindowAndItHasFewRedeliveries(t *testing.T) {
	t.Parallel()

	h := newRedeliverHarness(t)
	minute := time.Minute
	redelivery := func(id int64, guid string, ago time.Duration) faucet.HookDelivery {
		d := h.delivery(id, guid, ago, 400)
		d.Redelivery = true
		return d
	}
	h.hooks.pages[""] = faucet.DeliveryPage{Deliveries: []faucet.HookDelivery{
		h.delivery(101, "never-redelivered", 10*minute, 500),
		redelivery(204, "two-redeliveries", 20*minute),
		redelivery(203, "two-redeliveries", 30*minute),
		h.delivery(202, "two-redeliveries", 50*minute, 500),
		redelivery(304, "three-redeliveries", 5*minute),
		redelivery(303, "three-redeliveries", 15*minute),
		redelivery(302, "three-redeliveries", 25*minute),
		h.delivery(301, "three-redeliveries", 55*minute, 500),
		redelivery(402, "original-left-the-window", 20*minute),
		h.delivery(401, "original-left-the-window", 70*minute, 500),
	}}

	res, err := h.svc.Redeliver(context.Background(), h.since)
	require.NoError(t, err)

	require.Equal(t, []int64{101, 204}, h.hooks.requested(),
		"a delivery that has had fewer than MaxRedeliveriesPerDelivery redeliveries is asked for, and the ones with the most are asked for last")
	require.Equal(t, 2, res.Pending, "a delivery that faucet must not ask for is not pending")
	require.Equal(t, 2, res.GaveUp, "the delivery with three redeliveries and the one whose first attempt left the window")
}

func TestRedeliverAsksForDeliveriesThatWereNotRedeliveredBeforeTheOnesThatWere(t *testing.T) {
	t.Parallel()

	h := newRedeliverHarness(t)
	const retried, fresh = faucet.MaxRedeliveries + 5, 5
	var page []faucet.HookDelivery
	for i := range fresh {
		page = append(page, h.delivery(int64(3000+i), fmt.Sprintf("fresh-%d", i), time.Duration(5-i)*time.Minute, 500))
	}
	for i := range retried {
		guid := fmt.Sprintf("retried-%02d", i)
		redelivery := h.delivery(int64(2000+i), guid, 30*time.Minute+time.Duration(i)*time.Second, 400)
		redelivery.Redelivery = true
		page = append(page, redelivery, h.delivery(int64(1000+i), guid, 40*time.Minute, 500))
	}
	h.hooks.pages[""] = faucet.DeliveryPage{Deliveries: page}

	_, err := h.svc.Redeliver(context.Background(), h.since)
	require.NoError(t, err)

	requested := h.hooks.requested()
	require.Len(t, requested, faucet.MaxRedeliveries)
	require.Equal(t, []int64{3000, 3001, 3002, 3003, 3004}, requested[:fresh],
		"deliveries nobody has redelivered come first, or deliveries that keep failing use up every run")
}

const (
	documentedEvery = 30 * time.Minute
	// documentedWindow is how far back a host that calls Redeliver every documentedEvery looks.
	documentedWindow = (faucet.MaxRedeliveriesPerDelivery + 1) * documentedEvery
)

// githubLike plays GitHub more closely than fakeHookAdmin. It lists the attempts that have happened, newest first,
// and a redelivery request adds an attempt with the same GUID, flagged as a redelivery and dated when it happens.
// Faucet answers 503 and stores nothing until downUntil. After that the attempt succeeds, and faucet stores the event,
// for the GUIDs in fixable. Faucet answers 400 for the others, as it does for a payload it cannot read.
type githubLike struct {
	mu        sync.Mutex
	clock     quartz.Clock
	store     *fakeStore
	attempts  []faucet.HookDelivery
	fixable   map[string]bool
	downUntil time.Time
	asked     map[string]int
	logs      *bytes.Buffer
}

func (g *githubLike) add(guid string, at time.Time, status int, redelivery bool) {
	g.attempts = append(g.attempts, faucet.HookDelivery{
		ID: int64(len(g.attempts) + 1), GUID: guid, DeliveredAt: at, StatusCode: status, Redelivery: redelivery,
	})
}

func (g *githubLike) ListDeliveries(_ context.Context, cursor string) (faucet.DeliveryPage, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var sorted []faucet.HookDelivery
	for _, d := range g.attempts {
		if !d.DeliveredAt.After(g.clock.Now()) {
			sorted = append(sorted, d)
		}
	}
	slices.SortFunc(sorted, func(a, b faucet.HookDelivery) int {
		return cmp.Or(b.DeliveredAt.Compare(a.DeliveredAt), cmp.Compare(b.ID, a.ID))
	})
	const perPage = 100
	offset := 0
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil {
			return faucet.DeliveryPage{}, err
		}
		offset = n
	}
	end := min(offset+perPage, len(sorted))
	page := faucet.DeliveryPage{Deliveries: sorted[offset:end]}
	if end < len(sorted) {
		page.Next = strconv.Itoa(end)
	}
	return page, nil
}

func (g *githubLike) Redeliver(ctx context.Context, attemptID int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, d := range g.attempts {
		if d.ID != attemptID {
			continue
		}
		g.asked[d.GUID]++
		now := g.clock.Now()
		switch {
		case now.Before(g.downUntil):
			g.add(d.GUID, now, 503, true)
		case !g.fixable[d.GUID]:
			g.add(d.GUID, now, 400, true)
		default:
			_, _, err := g.store.InsertEvent(ctx, faucet.Event{
				Source: "github", DeliveryID: d.GUID, Type: "pr.closed", RepoID: repoID, Data: []byte(`{"merged":true}`),
			})
			if err != nil {
				return err
			}
			g.add(d.GUID, now, 200, true)
		}
		return nil
	}
	return xerrors.Errorf("no attempt %d", attemptID)
}

func (g *githubLike) stored(t *testing.T, guid string) bool {
	t.Helper()
	stored, err := g.store.StoredDeliveries(context.Background(), []string{guid})
	require.NoError(t, err)
	return len(stored) == 1
}

func newGitHubLike(t *testing.T) (*faucet.Service, *githubLike, *quartz.Mock) {
	t.Helper()
	clock := quartz.NewMock(t)
	store := &fakeStore{now: func() time.Time { return clock.Now() }}
	logs := &bytes.Buffer{}
	hooks := &githubLike{clock: clock, store: store, fixable: map[string]bool{}, asked: map[string]int{}, logs: logs}
	deps := testDeps(store)
	deps.Clock = clock
	deps.HookAdmin = hooks
	deps.Logger = slog.Make(sloghuman.Sink(logs))
	svc, err := faucet.New(deps)
	require.NoError(t, err)
	return svc, hooks, clock
}

// sweepEvery calls Redeliver with the given window every `every` for `count` calls, and returns the results. before runs
// ahead of each call with its number.
func sweepEvery(t *testing.T, svc *faucet.Service, clock *quartz.Mock, every, window time.Duration, count int, before func(i int)) []faucet.RedeliverResult {
	t.Helper()
	results := make([]faucet.RedeliverResult, 0, count)
	for i := range count {
		if before != nil {
			before(i)
		}
		res, err := svc.Redeliver(context.Background(), clock.Now().Add(-window))
		require.NoError(t, err)
		results = append(results, res)
		clock.Advance(every).MustWait(context.Background())
	}
	return results
}

func TestRedeliverStopsAskingForADeliveryThatFaucetKeepsRejecting(t *testing.T) {
	t.Parallel()

	for name, every := range map[string]time.Duration{
		"a sweep every 5 minutes":  5 * time.Minute,
		"a sweep every 30 minutes": documentedEvery,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			svc, hooks, clock := newGitHubLike(t)
			hooks.add("always-400", clock.Now().Add(-5*time.Minute), 400, false)

			sweepEvery(t, svc, clock, every, documentedWindow, int(6*time.Hour/every), nil)

			require.Equal(t, faucet.MaxRedeliveriesPerDelivery, hooks.asked["always-400"],
				"each redelivery is a new attempt with a new time, so the window alone never ends it")
		})
	}
}

func TestRedeliverCountsAndLogsEveryGiveUpAtTheDocumentedSchedule(t *testing.T) {
	t.Parallel()

	svc, hooks, clock := newGitHubLike(t)
	hooks.add("always-400", clock.Now().Add(-5*time.Minute), 400, false)

	results := sweepEvery(t, svc, clock, documentedEvery, documentedWindow, 8, nil)

	type counts struct{ pending, gaveUp, requested int }
	var got []counts
	for _, r := range results {
		got = append(got, counts{r.Pending, r.GaveUp, r.Requested})
	}
	require.Equal(t, []counts{
		{1, 0, 1}, {1, 0, 1}, {1, 0, 1}, // three redeliveries
		{0, 1, 0}, // the limit is reached while the first attempt is inside the window
		{0, 1, 0}, // the first attempt has left the window, and the three redeliveries are still in it
		{0, 1, 0}, // the first redelivery has left the window too, so fewer than three remain
		{0, 1, 0},
		{0, 0, 0}, // no attempt is inside the window any more
	}, got, "a delivery is asked for, then counted as given up for as long as an attempt of it is visible, and never dropped silently")
	require.Equal(t, 4, strings.Count(hooks.logs.String(), "redelivery gave up"))
	require.Contains(t, hooks.logs.String(), "reason=limit")
	require.Contains(t, hooks.logs.String(), "reason=window")
}

func TestRedeliverRecoversADeliveryThatFailedDuringAnOutage(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		outage    time.Duration
		recovered bool
	}{
		"an outage of 35 minutes recovers on the third request":        {35 * time.Minute, true},
		"an outage of 100 minutes is given up on after three requests": {100 * time.Minute, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			svc, hooks, clock := newGitHubLike(t)
			t0 := clock.Now()
			hooks.fixable["G"] = true
			hooks.downUntil = t0.Add(tc.outage - time.Minute)
			hooks.add("G", t0.Add(-time.Minute), 503, false)

			results := sweepEvery(t, svc, clock, documentedEvery, documentedWindow, 8, nil)

			require.Equal(t, 3, hooks.asked["G"])
			require.Equal(t, tc.recovered, hooks.stored(t, "G"))
			gaveUp := 0
			for _, r := range results {
				gaveUp += r.GaveUp
			}
			if tc.recovered {
				require.Zero(t, gaveUp)
				require.NotContains(t, hooks.logs.String(), "redelivery gave up")
			} else {
				require.Positive(t, gaveUp, "a delivery that is lost is counted")
				require.Contains(t, hooks.logs.String(), "redelivery gave up")
			}
		})
	}
}

func TestRedeliverCountsAGiveUpWhenTheFirstAttemptLeftTheWindow(t *testing.T) {
	t.Parallel()

	svc, hooks, clock := newGitHubLike(t)
	t0 := clock.Now()
	hooks.fixable["G"] = true
	hooks.downUntil = t0.Add(34 * time.Minute)
	hooks.add("G", t0.Add(-time.Minute), 503, false)

	results := sweepEvery(t, svc, clock, documentedEvery, time.Hour, 3, nil)

	require.Equal(t, 0, results[0].GaveUp)
	require.Equal(t, 1, results[2].GaveUp, "at t0+60m the first attempt is an hour old and G has had two redeliveries")
	require.False(t, hooks.stored(t, "G"))
	require.Contains(t, hooks.logs.String(), "reason=window")
}

func TestRedeliverRecoversAnOrdinaryFailureBesideDeliveriesThatFaucetKeepsRejecting(t *testing.T) {
	t.Parallel()

	svc, hooks, clock := newGitHubLike(t)
	t0 := clock.Now()
	const permanent = 100
	for i := range permanent {
		hooks.add(fmt.Sprintf("always-400-%03d", i), t0.Add(-29*time.Minute).Add(time.Duration(i)*16*time.Second), 400, false)
	}
	hooks.fixable["ordinary"] = true

	sweepEvery(t, svc, clock, documentedEvery, documentedWindow, 7, func(i int) {
		if i == 2 {
			hooks.add("ordinary", clock.Now().Add(-20*time.Minute), 500, false)
		}
	})

	require.True(t, hooks.stored(t, "ordinary"), "a delivery that faucet rejects must not use up the requests that an ordinary failure needs")
	for guid, asks := range hooks.asked {
		require.LessOrEqual(t, asks, faucet.MaxRedeliveriesPerDelivery, guid)
	}
}

// A run reads the newest MaxRedeliverPages*100 attempts and keeps no state. The n-th request for a delivery needs its
// first attempt among them, and it comes less than n sweeps after that attempt. So the third request is safe while the
// hook records at most 1000 / (3 * 30 minutes) = 666 attempts an hour, and the first request while it records at most
// 1000 / 30 minutes = 2000 an hour.
func TestRedeliverNeedsTheFirstAttemptAmongTheNewestThousand(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		perHour   int
		recovered bool
	}{
		"600 attempts an hour, below 666":  {600, true},
		"1500 attempts an hour, above 666": {1500, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			svc, hooks, clock := newGitHubLike(t)
			t0 := clock.Now()
			spacing := time.Hour / time.Duration(tc.perHour)
			for i := range 4 * tc.perHour {
				hooks.add(fmt.Sprintf("ok-%d", i), t0.Add(-time.Hour).Add(time.Duration(i)*spacing), 200, false)
			}
			hooks.fixable["G"] = true
			hooks.downUntil = t0.Add(34 * time.Minute)
			hooks.add("G", t0.Add(-time.Minute), 503, false)

			results := sweepEvery(t, svc, clock, documentedEvery, documentedWindow, 4, nil)

			require.Equal(t, tc.recovered, hooks.stored(t, "G"))
			require.True(t, results[2].Incomplete, "at t0+60m the window holds more than 1000 attempts, and the result says that it was not read to its end")
			if !tc.recovered {
				require.Contains(t, hooks.logs.String(), "redelivery gave up", "a delivery that falls out of view is counted as given up, not dropped silently")
			}
		})
	}
}
