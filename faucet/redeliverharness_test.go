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
}

var _ faucet.HookAdmin = (*fakeGitHub)(nil)

func newFakeGitHub(clock quartz.Clock, store *fakeRedeliveryStore) *fakeGitHub {
	return &fakeGitHub{clock: clock, store: store, pageSize: 100, name: "/orgs/acme/hooks/7", asks: map[string][]time.Time{}}
}

func (g *fakeGitHub) Hook() string { return g.name }

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
	now := g.clock.Now()
	hi := sort.Search(len(g.attempts), func(i int) bool { return g.attempts[i].at.After(now) })
	lo := sort.Search(len(g.attempts), func(i int) bool { return g.attempts[i].at.After(now.Add(-faucet.GitHubDeliveryRetention)) })
	return g.attempts[lo:hi]
}

func (g *fakeGitHub) ListDeliveries(_ context.Context, cursor string) (faucet.DeliveryPage, error) {
	if gate := g.listGate; gate != nil {
		gate()
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
		start = sort.Search(len(visible), func(i int) bool { return !olderThan(visible[i], key) })
	}
	start = max(start, 0)
	end := max(start-g.pageSize, 0)
	page := faucet.DeliveryPage{}
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

func (g *fakeGitHub) attemptLocked(id int64) (ghAttempt, bool) {
	for _, a := range g.attempts {
		if a.id == id {
			return a, true
		}
	}
	return ghAttempt{}, false
}

func olderThan(a, b ghAttempt) bool {
	return a.at.Before(b.at) || (a.at.Equal(b.at) && a.id < b.id)
}

func (g *fakeGitHub) Redeliver(_ context.Context, attemptID int64) error {
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
		now := g.clock.Now()
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
