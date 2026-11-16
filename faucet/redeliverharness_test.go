package faucet_test

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"github.com/coder/faucet/faucet"
	"github.com/coder/quartz"
)

type ghAttempt struct {
	id            int64
	guid          string
	at            time.Time
	status        int
	event, action string
}

type cursorStyle int

const (
	// keysetCursor continues after one attempt, as GitHub does.
	keysetCursor cursorStyle = iota
	// offsetCursor counts from the newest attempt, so every new attempt moves
	// a stored cursor up the list. It is the worst case for a cursor that is
	// kept between runs.
	offsetCursor
)

// fakeGitHub plays the deliveries API of one webhook and faucet's answers to
// it. A redelivery adds an attempt that is dated when it was asked for. It
// lists only attempts that have happened and that GitHub still keeps.
type fakeGitHub struct {
	mu       sync.Mutex
	clock    quartz.Clock
	store    *fakeRedeliveryStore
	attempts []ghAttempt // ascending by time, then ID
	nextID   int64
	pageSize int
	style    cursorStyle
	name     string
	// skew is GitHub's clock minus faucet's. Every time that GitHub gives is
	// on its own clock, and a test that records an attempt gives GitHub's time.
	skew time.Duration
	// answer is the status that faucet gives a delivery of guid at that time.
	// A 2xx also stores the event. The default is 200.
	answer func(guid string, at time.Time) int

	asks      map[string][]time.Time
	listCalls int
	// listFault and askFault inject a GitHub failure.
	listFault func(call int, cursor string) error
	askFault  func(guid string) error
	// listGate, when set, runs inside every list call, before the answer.
	listGate func()
	// listDelay and askDelay make every list call and every redelivery request
	// take that long, or until its context ends.
	listDelay, askDelay time.Duration
	// date gives the Date header of a list response (DeliveryPage.At), from the
	// number of the list call, its cursor and GitHub's clock when it answered.
	// A zero time is a response without a Date. Nil sends GitHub's clock.
	date func(call int, cursor string, at time.Time) time.Time
	// refusesStaleCursors makes GitHub answer a cursor whose attempt it has
	// deleted with an error. By default it lists on from where the attempt was.
	refusesStaleCursors bool
}

var _ faucet.HookAdmin = (*fakeGitHub)(nil)

func newFakeGitHub(clock quartz.Clock, store *fakeRedeliveryStore) *fakeGitHub {
	return &fakeGitHub{clock: clock, store: store, pageSize: 100, name: "/orgs/acme/hooks/7", asks: map[string][]time.Time{}}
}

func (g *fakeGitHub) Hook() string { return g.name }

// now is GitHub's clock.
func (g *fakeGitHub) now() time.Time { return g.clock.Now().Add(g.skew) }

const (
	handledEvent  = "workflow_run"
	handledAction = "completed"
)

// deliver records one attempt of an event that faucet stores.
func (g *fakeGitHub) deliver(guid string, at time.Time) ghAttempt {
	return g.deliverEvent(guid, at, handledEvent, handledAction)
}

func (g *fakeGitHub) deliverEvent(guid string, at time.Time, event, action string) ghAttempt {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.deliverLocked(guid, at, event, action)
}

func (g *fakeGitHub) deliverLocked(guid string, at time.Time, event, action string) ghAttempt {
	status := http200
	if g.answer != nil {
		status = g.answer(guid, at)
	}
	if status >= 200 && status < 300 && event == handledEvent {
		_, _, err := g.store.InsertEvent(context.Background(), faucet.Event{
			Source: "github", DeliveryID: guid, Type: "ci.run_completed", RepoID: repoID, Data: []byte(`{}`),
		})
		if err != nil {
			panic(err)
		}
	}
	return g.recordLocked(guid, at, status, event, action)
}

const http200 = 200

// record adds an attempt with the given status, whatever faucet would answer.
// It stores nothing, so a failed attempt leaves its event unstored.
func (g *fakeGitHub) record(guid string, at time.Time, status int) ghAttempt {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.recordLocked(guid, at, status, handledEvent, handledAction)
}

func (g *fakeGitHub) recordEvent(guid string, at time.Time, status int, event, action string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.recordLocked(guid, at, status, event, action)
}

func (g *fakeGitHub) recordLocked(guid string, at time.Time, status int, event, action string) ghAttempt {
	g.nextID++
	a := ghAttempt{id: g.nextID, guid: guid, at: at, status: status, event: event, action: action}
	i := sort.Search(len(g.attempts), func(i int) bool { return g.attempts[i].at.After(at) })
	g.attempts = slices.Insert(g.attempts, i, a)
	return a
}

func (g *fakeGitHub) visibleLocked() []ghAttempt {
	now := g.now()
	hi := sort.Search(len(g.attempts), func(i int) bool { return g.attempts[i].at.After(now) })
	lo := sort.Search(len(g.attempts), func(i int) bool { return g.attempts[i].at.After(now.Add(-faucet.GitHubDeliveryRetention)) })
	return g.attempts[lo:hi]
}

func (g *fakeGitHub) ListDeliveries(ctx context.Context, cursor string) (faucet.DeliveryPage, error) {
	if gate := g.listGate; gate != nil {
		gate()
	}
	if g.listDelay > 0 {
		select {
		case <-time.After(g.listDelay):
		case <-ctx.Done():
			return faucet.DeliveryPage{}, ctx.Err()
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.listCalls++
	if g.listFault != nil {
		if err := g.listFault(g.listCalls, cursor); err != nil {
			return faucet.DeliveryPage{}, err
		}
	}
	visible := g.visibleLocked()
	start := len(visible) // index just above the first attempt of the page, counting down
	switch {
	case cursor == "":
	case g.style == offsetCursor:
		off, err := strconv.Atoi(cursor)
		if err != nil {
			return faucet.DeliveryPage{}, fmt.Errorf("GitHub rejected the cursor")
		}
		start = len(visible) - off
	default:
		id, err := strconv.ParseInt(strings.TrimPrefix(cursor, "v1_"), 10, 64)
		if err != nil {
			return faucet.DeliveryPage{}, fmt.Errorf("GitHub rejected the cursor")
		}
		key, ok := g.attemptLocked(id)
		if !ok {
			return faucet.DeliveryPage{}, fmt.Errorf("GitHub rejected the cursor")
		}
		if g.refusesStaleCursors && !key.at.After(g.now().Add(-faucet.GitHubDeliveryRetention)) {
			return faucet.DeliveryPage{}, fmt.Errorf("GitHub answered the delivery list with status 422")
		}
		start = sort.Search(len(visible), func(i int) bool { return !olderThan(visible[i], key) })
	}
	start = max(start, 0)
	end := max(start-g.pageSize, 0)
	page := faucet.DeliveryPage{At: g.now()}
	if g.date != nil {
		page.At = g.date(g.listCalls, cursor, g.now())
	}
	for i := start - 1; i >= end; i-- {
		a := visible[i]
		page.Deliveries = append(page.Deliveries, faucet.HookDelivery{
			ID: a.id, GUID: a.guid, DeliveredAt: a.at, StatusCode: a.status, Event: a.event, Action: a.action,
		})
	}
	if end > 0 {
		last := visible[end]
		page.Next = "v1_" + strconv.FormatInt(last.id, 10)
		if g.style == offsetCursor {
			page.Next = strconv.Itoa(len(visible) - end)
		}
	}
	return page, nil
}

// lists says whether GitHub's list holds an attempt at guid now.
func (g *fakeGitHub) lists(guid string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.ContainsFunc(g.visibleLocked(), func(a ghAttempt) bool { return a.guid == guid })
}

func (g *fakeGitHub) attemptLocked(id int64) (ghAttempt, bool) {
	for i := range g.attempts {
		if g.attempts[i].id == id {
			return g.attempts[i], true
		}
	}
	return ghAttempt{}, false
}

func olderThan(a, b ghAttempt) bool {
	return a.at.Before(b.at) || (a.at.Equal(b.at) && a.id < b.id)
}

func (g *fakeGitHub) Redeliver(ctx context.Context, attemptID int64) error {
	if g.askDelay > 0 {
		select {
		case <-time.After(g.askDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, a := range g.attempts {
		if a.id != attemptID {
			continue
		}
		if g.askFault != nil {
			if err := g.askFault(a.guid); err != nil {
				return err
			}
		}
		now := g.now()
		g.asks[a.guid] = append(g.asks[a.guid], now)
		g.deliverLocked(a.guid, now, a.event, a.action)
		return nil
	}
	return fmt.Errorf("GitHub answered the redelivery request with status 404")
}

func (g *fakeGitHub) askedTimes(guid string) []time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.asks[guid])
}

func (g *fakeGitHub) totalAsks() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, times := range g.asks {
		n += len(times)
	}
	return n
}

// The clock tests lean on what the fake GitHub does with a Date, a slow page
// and a stale cursor, so this pins each of them.
func TestFakeGitHubSendsTheDateTakesTheTimeAndRefusesTheCursorThatATestSets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("the Date is GitHub's clock unless the test gives one", func(t *testing.T) {
		t.Parallel()
		env := newRedeliveryEnv(t, func(o *envOptions) { o.pageSize = 1 })
		env.gh.skew = time.Hour
		for i := range 2 {
			env.gh.record(fmt.Sprintf("g-%d", i), env.gh.now().Add(-time.Duration(i+1)*time.Minute), http200)
		}

		first, err := env.gh.ListDeliveries(ctx, "")
		require.NoError(t, err)
		require.True(t, env.now().Add(time.Hour).Equal(first.At), "the Date is on GitHub's clock, not faucet's")

		env.gh.date = func(call int, cursor string, at time.Time) time.Time {
			if cursor != "" {
				return time.Time{}
			}
			return at.Add(time.Duration(call) * time.Minute)
		}
		head, err := env.gh.ListDeliveries(ctx, "")
		require.NoError(t, err)
		require.True(t, env.gh.now().Add(2*time.Minute).Equal(head.At), "the second list call got the Date of call 2")
		next, err := env.gh.ListDeliveries(ctx, head.Next)
		require.NoError(t, err)
		require.True(t, next.At.IsZero(), "a zero time is a response without a Date")

		env.gh.date = dateOff(-time.Second)
		behind, err := env.gh.ListDeliveries(ctx, "")
		require.NoError(t, err)
		require.True(t, env.gh.now().Add(-time.Second).Equal(behind.At))
	})

	t.Run("a slow page is read after GitHub's clock has moved", func(t *testing.T) {
		t.Parallel()
		env := newRedeliveryEnv(t)
		leaving := env.gh.now().Add(-faucet.GitHubDeliveryRetention + time.Second)
		env.gh.record("leaving", leaving, 503)
		env.gh.record("staying", env.gh.now().Add(-time.Minute), http200)
		asked := env.gh.now()
		env.slowPages(2 * time.Second)

		page, err := env.gh.ListDeliveries(ctx, "")
		require.NoError(t, err)
		require.Len(t, page.Deliveries, 1, "GitHub deleted the old attempt while the request was on its way")
		require.Equal(t, "staying", page.Deliveries[0].GUID)
		require.True(t, asked.Add(2*time.Second).Equal(page.At), "the Date is the time of the answer, not of the request")
	})

	t.Run("a cursor of a deleted attempt lists on, or is refused when the test says so", func(t *testing.T) {
		t.Parallel()
		env := newRedeliveryEnv(t, func(o *envOptions) { o.pageSize = 1 })
		old := env.gh.now().Add(-faucet.GitHubDeliveryRetention + time.Minute)
		env.gh.record("older", old, http200)
		env.gh.record("old", old.Add(time.Second), http200)
		env.gh.record("new", env.gh.now().Add(-time.Minute), http200)
		head, err := env.gh.ListDeliveries(ctx, "")
		require.NoError(t, err)
		middle, err := env.gh.ListDeliveries(ctx, head.Next)
		require.NoError(t, err)
		require.Equal(t, "old", middle.Deliveries[0].GUID)
		require.NotEmpty(t, middle.Next)

		env.advance(2 * time.Minute)
		rest, err := env.gh.ListDeliveries(ctx, middle.Next)
		require.NoError(t, err, "by default the list goes on from where the deleted attempt was")
		require.Empty(t, rest.Deliveries)

		env.gh.refusesStaleCursors = true
		_, err = env.gh.ListDeliveries(ctx, middle.Next)
		require.ErrorContains(t, err, "422")
		_, err = env.gh.ListDeliveries(ctx, head.Next)
		require.NoError(t, err, "a cursor of an attempt that GitHub still lists is not stale")
	})
}

// captureSink keeps the structure of every log entry.
type captureSink struct {
	mu      sync.Mutex
	entries []slog.SinkEntry
}

func (s *captureSink) LogEntry(_ context.Context, e slog.SinkEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, e)
}

func (*captureSink) Sync() {}

// lines returns the fields of every entry with the message, as strings.
func (s *captureSink) lines(message string) []map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]string
	for _, e := range s.entries {
		if e.Message != message {
			continue
		}
		fields := map[string]string{}
		for _, f := range e.Fields {
			fields[f.Name] = fmt.Sprint(f.Value)
		}
		out = append(out, fields)
	}
	return out
}

func (s *captureSink) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for _, e := range s.entries {
		_, _ = fmt.Fprintf(&b, "%s %v\n", e.Message, e.Fields)
	}
	return b.String()
}

type envOptions struct {
	maxPages, maxAsks int
	pageSize          int
	style             cursorStyle
}

type redeliveryEnv struct {
	t     *testing.T
	clock *quartz.Mock
	store *fakeRedeliveryStore
	gh    *fakeGitHub
	svc   *faucet.Service
	logs  *captureSink
}

func newRedeliveryEnv(t *testing.T, opts ...func(*envOptions)) *redeliveryEnv {
	t.Helper()
	o := envOptions{pageSize: 100}
	for _, opt := range opts {
		opt(&o)
	}
	clock := quartz.NewMock(t)
	store := newFakeRedeliveryStore(func() time.Time { return clock.Now() })
	gh := newFakeGitHub(clock, store)
	gh.pageSize, gh.style = o.pageSize, o.style
	logs := &captureSink{}

	deps := testDeps(store)
	deps.Clock = clock
	deps.Logger = slog.Make(logs)
	deps.Redelivery = &faucet.RedeliveryDeps{Store: store, Hooks: gh, MaxPagesPerRun: o.maxPages, MaxAsksPerRun: o.maxAsks}
	svc, err := faucet.New(deps)
	require.NoError(t, err)
	return &redeliveryEnv{t: t, clock: clock, store: store, gh: gh, svc: svc, logs: logs}
}

func (e *redeliveryEnv) now() time.Time { return e.clock.Now() }

// useMaxPages replaces the service with one that reads more pages per run, on
// the same store, GitHub and logs.
func (e *redeliveryEnv) useMaxPages(pages int) {
	e.t.Helper()
	deps := testDeps(e.store)
	deps.Clock = e.clock
	deps.Logger = slog.Make(e.logs)
	deps.Redelivery = &faucet.RedeliveryDeps{Store: e.store, Hooks: e.gh, MaxPagesPerRun: pages}
	svc, err := faucet.New(deps)
	require.NoError(e.t, err)
	e.svc = svc
}

func (e *redeliveryEnv) advance(d time.Duration) {
	e.t.Helper()
	e.clock.Advance(d).MustWait(context.Background())
}

// slowPages makes every list request take d to reach GitHub: the clock moves
// forward by d inside the call, before GitHub reads its list.
func (e *redeliveryEnv) slowPages(d time.Duration) {
	e.gh.listGate = func() { e.advance(d) }
}

// dateOff is a Date that is d away from GitHub's clock: ahead of it when d is
// positive, behind it when d is negative.
func dateOff(d time.Duration) func(int, string, time.Time) time.Time {
	return func(_ int, _ string, at time.Time) time.Time { return at.Add(d) }
}

func (e *redeliveryEnv) run() faucet.RedeliverResult {
	e.t.Helper()
	res, err := e.svc.Redeliver(context.Background())
	require.NoError(e.t, err)
	return res
}

func (e *redeliveryEnv) stored(guid string) bool {
	e.store.rmu.Lock()
	defer e.store.rmu.Unlock()
	return e.store.stored[guid]
}

func (e *redeliveryEnv) logged(message string) []map[string]string { return e.logs.lines(message) }

// failing makes faucet answer every attempt of the guids with status.
func failing(status int, guids ...string) func(string, time.Time) int {
	return func(guid string, _ time.Time) int {
		if slices.Contains(guids, guid) {
			return status
		}
		return http200
	}
}
