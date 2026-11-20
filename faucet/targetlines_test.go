package faucet_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/coder/quartz"
	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/pgstore"
)

const (
	// privateRepoID is a repository that no caller of these tests may read.
	privateRepoID = 999
	waitTool      = "wait_for_github_events"
)

// TestAQuietRepositorysCursorSaysNothingOfAnotherRepository is the report
// of the leak: a caller of octo/repo read how many events, and when, a
// repository that it cannot read received.
func TestAQuietRepositorysCursorSaysNothingOfAnotherRepository(t *testing.T) {
	t.Parallel()

	db := postgresDB(t)
	store := pgstore.New(db)
	h := newEventsHarness(t, func(d *faucet.Deps) { d.Store = store })
	n := 0
	insert := func(repo int64) {
		t.Helper()
		n++
		_, ok, err := store.InsertEvent(h.ctx, faucet.Event{
			Source: "github", DeliveryID: fmt.Sprintf("d-%d", n), Type: "ci.run_completed", RepoID: repo,
			Data: json.RawMessage(sampleData["ci.run_completed"]),
		})
		require.NoError(t, err)
		require.True(t, ok)
	}
	// The store stamps an event with the database's time, so the service's
	// clock follows the real one.
	now := func() { h.clock.Set(time.Now()) }
	args := map[string]any{"repo": testRepo}

	insert(repoID)
	for range 5 {
		insert(privateRepoID)
	}
	now()
	c0 := h.headCursor(args)
	for range 7 {
		insert(privateRepoID)
	}
	now()
	require.Equal(t, c0, h.headCursor(args), "the head of octo/repo moves with its own events only")

	before := h.headCursor(args)
	insert(privateRepoID)
	time.Sleep(1500 * time.Millisecond)
	now()
	for maxAge := int64(3000); maxAge >= 500; maxAge -= 250 {
		got := h.poll(&rawParams{Name: "ci.run_completed", Arguments: raw(args), Cursor: cursorOf(before), MaxAgeMs: raw(maxAge)})
		require.False(t, got.Truncated, "maxAgeMs %d: octo/repo lost nothing, whenever another repository's event arrived", maxAge)
		require.Equal(t, before, got.Cursor, "maxAgeMs %d", maxAge)
	}
}

// TestAReaderOfOneTargetGetsTheSameAnswersWhateverOtherTargetsDo runs one
// script twice, on a store where only the reader's target receives events and
// on one where other targets receive events too, of which some are purged,
// and compares every answer that the reader gets: polls, waits, subscribes and
// the envelopes that its endpoint receives. Only the parts that differ from
// run to run, times and the HMAC IDs, are left out.
func TestAReaderOfOneTargetGetsTheSameAnswersWhateverOtherTargetsDo(t *testing.T) {
	t.Parallel()
	for storeName, newStore := range map[string]lineStore{"postgres": postgresLines, "fake": fakeLines} {
		for _, kind := range []string{"repo", "owner"} {
			t.Run(storeName+"/"+kind, func(t *testing.T) {
				t.Parallel()
				reader := repoLineReader()
				if kind == "owner" {
					reader = ownerLineReader(t)
				}
				quiet := newLineWorld(t, reader, newStore, false)
				quiet.run()
				busy := newLineWorld(t, reader, newStore, true)
				busy.run()
				require.Equal(t, quiet.seen, busy.seen, "the reader of one target gets the same answers whatever other targets receive")
			})
		}
	}
}

// lineReader is a caller that may read one target, the repositories whose
// events the target covers, and repositories of other targets.
type lineReader struct {
	args   map[string]any
	caller []func(*faucet.Deps)
	own    []ownedRepo
	others []ownedRepo
}

var privateRepo = ownedRepo{id: privateRepoID, fullName: "secret/private"}

func repoLineReader() lineReader {
	return lineReader{
		args:   map[string]any{"repo": testRepo},
		own:    []ownedRepo{{id: repoID, ownerID: 9, fullName: testRepo}},
		others: []ownedRepo{privateRepo, {id: otherRepoID, ownerID: 9, fullName: otherRepo}, octoC},
	}
}

func ownerLineReader(t *testing.T) lineReader {
	return lineReader{
		args:   map[string]any{"owner": "coder"},
		caller: []func(*faucet.Deps){asCaller(newServicePrincipal(t), &fakeGrants{owners: map[string]int64{"coder": ownerID}})},
		own:    []ownedRepo{coderA, coderB},
		others: []ownedRepo{privateRepo, octoC, {id: repoID, ownerID: 9, fullName: testRepo}},
	}
}

// lineWorld is one store and the service in front of it, and what the
// reader saw of it. Only a busy world stores events of other targets.
type lineWorld struct {
	t      *testing.T
	busy   bool
	reader lineReader
	d      *deliverHarness
	store  faucet.Store
	bulk   func(n int)
	secret string
	seen   []string
	owns   []faucet.Event
	others int
	heard  int
}

const linePath = "/hook"

// lineStore makes a store and the function that stores n events of repo in
// it at once.
type lineStore func(t *testing.T, repo ownedRepo) (faucet.Store, func(n int))

func postgresLines(t *testing.T, repo ownedRepo) (faucet.Store, func(n int)) {
	db := postgresDB(t)
	return pgstore.New(db), func(n int) { insertManyOnPostgres(t, db, repo, n) }
}

func fakeLines(_ *testing.T, repo ownedRepo) (faucet.Store, func(n int)) {
	fake := &fakeStore{now: time.Now}
	return fake, func(n int) { fake.appendEventsOf(repo, n) }
}

func newLineWorld(t *testing.T, reader lineReader, newStore lineStore, busy bool) *lineWorld {
	t.Helper()
	w := &lineWorld{t: t, busy: busy, reader: reader, secret: newSecret(t, 32)}
	w.store, w.bulk = newStore(t, reader.others[0])
	useStore := func(d *faucet.Deps) { d.Store, d.Clock = w.store, quartz.NewReal() }
	w.d = newDeliverHarness(t, append([]func(*faucet.Deps){useStore}, reader.caller...)...)
	w.d.ledgerSkip = true
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the %s world saw:\n%s", map[bool]string{true: "busy", false: "quiet"}[busy], strings.Join(w.seen, "\n"))
		}
	})
	return w
}

// run is the script. Each step that the reader can observe notes what it
// saw.
func (w *lineWorld) run() {
	t := w.t
	w.other(5)
	firstCut := clearInstant()
	start := w.poll("the head", "").Cursor
	w.wait("a wait at the head", nil)
	for range 3 {
		w.own()
	}
	w.other(7)
	w.poll("the head after three events", "")
	w.poll("a poll from the start", start)
	w.wait("a wait from the start", start)
	w.subscribe("a subscribe", "")
	if w.busy {
		w.bulk(60_000)
	}
	got := w.follow("following the cursor from the start", start, 2)
	require.Equal(t, w.ownIDs(), got, "following the cursor reads every event of the target once, in order")
	w.own()
	w.deliver("the first run")
	w.other(3)
	w.deliver("a run with nothing for the reader")
	w.own()
	w.other(2)
	w.deliver("the third run")

	w.purge(firstCut)
	kept := w.poll("a poll from the start after a purge of other targets' events", start)
	require.False(t, kept.Truncated, "a purge of other targets' events deletes nothing of the reader's target")
	w.follow("following the cursor after that purge", start, 100)

	heads := []string{start}
	marks := []time.Time{clearInstant()}
	for range 3 {
		heads = append(heads, w.poll("the head before an event", "").Cursor)
		w.own()
		w.other(1)
		marks = append(marks, clearInstant())
	}
	w.sweep(heads, marks)

	beforeLoss := w.poll("the head before the loss", "").Cursor
	w.own()
	w.own()
	lossCut := clearInstant()
	w.other(2)
	w.purge(lossCut)
	lost := w.poll("a poll from the start after the target lost events", start)
	require.True(t, lost.Truncated, "the target lost events after the start")
	lost = w.poll("a poll from the head before the loss", beforeLoss)
	require.True(t, lost.Truncated, "the target lost the events after that head")
	after := w.poll("the head after the loss", "").Cursor
	kept = w.poll("a poll from the head after the loss", after)
	require.False(t, kept.Truncated, "nothing was lost after the head that followed the loss")
	gaps := w.deliver("the run after the loss")
	require.Equal(t, 1, strings.Count(gaps, `"type":"gap"`), "the endpoint hears of the loss once: %s", gaps)
	w.own()
	w.deliver("the run after the gap")
	w.subscribe("a renewal", start)

	for _, cursor := range []string{"v1.0", "v1.1", "v1.7", "v1.100000", "v2.0", "v2.1", "v2.2", "v2.5", "v2.9", "v2.100000", "v2.9223372036854775807"} {
		w.poll("a poll from the hand-made cursor "+cursor, cursor)
		w.wait("a wait from the hand-made cursor "+cursor, cursor)
	}
	w.wait("a wait from the head", after)
}

func (w *lineWorld) note(step, saw string) {
	w.seen = append(w.seen, step+": "+saw)
}

func (w *lineWorld) insert(delivery string, repo ownedRepo, data string) faucet.Event {
	w.t.Helper()
	stored, inserted, err := w.store.InsertEvent(w.d.ctx, faucet.Event{
		Source: "github", DeliveryID: delivery, Type: "ci.run_completed",
		RepoID: repo.id, OwnerID: repo.ownerID, RepoName: repo.fullName, Data: json.RawMessage(data),
	})
	require.NoError(w.t, err)
	require.True(w.t, inserted)
	return stored
}

// own stores an event of the reader's target.
func (w *lineWorld) own() {
	w.t.Helper()
	n := len(w.owns) + 1
	repo := w.reader.own[n%len(w.reader.own)]
	data := fmt.Sprintf(`{"run_id":%d,"name":"CI","conclusion":"success","head_branch":"main","html_url":"https://example.test/run/%d","run_attempt":1}`, n, n)
	w.owns = append(w.owns, w.insert(fmt.Sprintf("own-%d", n), repo, data))
}

// other stores n events of other targets, in a busy world only.
func (w *lineWorld) other(n int) {
	w.t.Helper()
	if !w.busy {
		return
	}
	for range n {
		w.others++
		w.insert(fmt.Sprintf("other-%d", w.others), w.reader.others[w.others%len(w.reader.others)], sampleData["ci.run_completed"])
	}
}

func (w *lineWorld) ownIDs() []string {
	ids := make([]string, len(w.owns))
	for i, ev := range w.owns {
		ids[i] = ev.DeliveryID
	}
	return ids
}

// clearInstant returns a time well clear of the events stored before and
// after it.
func clearInstant() time.Time {
	time.Sleep(100 * time.Millisecond)
	at := time.Now()
	time.Sleep(100 * time.Millisecond)
	return at
}

func (w *lineWorld) purge(before time.Time) {
	w.t.Helper()
	_, err := w.store.Purge(w.d.ctx, before)
	require.NoError(w.t, err)
}

func (w *lineWorld) poll(step, cursor string, more ...func(*rawParams)) pollOutcome {
	w.t.Helper()
	p := &rawParams{Name: "ci.run_completed", Arguments: raw(w.reader.args)}
	if cursor != "" {
		p.Cursor = cursorOf(cursor)
	}
	for _, m := range more {
		m(p)
	}
	res, err := w.d.call(methodPoll, p)
	if err != nil {
		w.note(step, "error "+err.Error())
		return pollOutcome{}
	}
	out := decode[pollOutcome](w.t, res)
	data := make([]map[string]any, len(out.Events))
	for i, ev := range out.Events {
		data[i] = ev.Data
	}
	w.note(step, fmt.Sprintf("events %v %s cursor %s truncated %t hasMore %t", out.eventIDs(), raw(data), out.Cursor, out.Truncated, out.HasMore))
	return out
}

// follow polls on from cursor until a poll says that nothing is left, and
// returns the events it read.
func (w *lineWorld) follow(step, cursor string, maxEvents int) []string {
	w.t.Helper()
	var ids []string
	for page := 1; ; page++ {
		require.Less(w.t, page, 1000, "the polls end")
		out := w.poll(fmt.Sprintf("%s, page %d", step, page), cursor, func(p *rawParams) { p.MaxEvents = raw(maxEvents) })
		ids = append(ids, out.eventIDs()...)
		cursor = out.Cursor
		if !out.HasMore {
			return ids
		}
	}
}

// sweep polls from every cursor with maxAgeMs set to reach back to each
// mark.
func (w *lineWorld) sweep(cursors []string, marks []time.Time) {
	w.t.Helper()
	for i, cursor := range cursors {
		for j, mark := range marks {
			maxAge := time.Since(mark).Milliseconds()
			w.poll(fmt.Sprintf("a poll from cursor %d with maxAgeMs back to mark %d", i, j), cursor, func(p *rawParams) { p.MaxAgeMs = raw(maxAge) })
		}
	}
}

// wait calls the wait tool for a second and returns the cursor it gave.
func (w *lineWorld) wait(step string, cursor any) any {
	w.t.Helper()
	args := maps.Clone(w.reader.args)
	args["types"] = []string{"ci.run_completed"}
	args["timeout_s"] = 1
	if cursor != nil {
		args["cursor"] = cursor
	}
	res, err := w.d.session.CallTool(w.d.ctx, &mcp.CallToolParams{Name: waitTool, Arguments: args})
	switch {
	case err != nil:
		w.note(step, "error "+err.Error())
		return nil
	case res.IsError:
		text, _ := res.Content[0].(*mcp.TextContent)
		w.note(step, "tool error "+text.Text)
		return nil
	}
	var out map[string]any
	require.NoError(w.t, json.Unmarshal(raw(res.StructuredContent), &out))
	events, _ := out["events"].([]any)
	for _, ev := range events {
		delete(ev.(map[string]any), "received_at")
	}
	w.note(step, string(raw(out)))
	return out["cursor"]
}

func (w *lineWorld) subscribe(step, cursor string) subscribed {
	w.t.Helper()
	p := subscribeTo(w.reader.args, w.d.rcv.url(linePath), w.secret)
	if cursor != "" {
		p.Cursor = cursorOf(cursor)
	}
	res, err := w.d.call(methodSubscribe, p)
	if err != nil {
		w.note(step, "error "+err.Error())
		return subscribed{}
	}
	got := decode[subscribed](w.t, res)
	w.note(step, fmt.Sprintf("cursor %s truncated %t", got.Cursor, got.Truncated))
	return got
}

// deliver runs a delivery and notes the bodies that the endpoint received in
// it, without their HMAC IDs and times, in sorted order: one run sends its
// requests at once.
func (w *lineWorld) deliver(step string) string {
	w.t.Helper()
	w.d.deliverDueWith(context.WithoutCancel(w.d.ctx))
	requests := w.d.rcv.requests(linePath)[w.heard:]
	w.heard += len(requests)
	bodies := make([]string, len(requests))
	for i, r := range requests {
		var body map[string]any
		require.NoError(w.t, json.Unmarshal(r.Body, &body))
		delete(body, "eventId")
		delete(body, "timestamp")
		bodies[i] = string(raw(body))
	}
	slices.Sort(bodies)
	saw := strings.Join(bodies, " ")
	w.note(step, saw)
	return saw
}

// insertManyOnPostgres stores n events of repo in one statement, under the
// lock that every writer of faucet_events takes (pgstore's eventsLockKey).
func insertManyOnPostgres(t *testing.T, db *sql.DB, repo ownedRepo, n int) {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec("SELECT pg_advisory_xact_lock(1664114494957747203)")
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO faucet_events (source, delivery_id, type, repo_id, owner_id, repo_name, data, received_at)
		SELECT 'github', 'bulk-' || g, 'ci.run_completed', $1, $2, $3, $4::jsonb, clock_timestamp() FROM generate_series(1, $5::int) AS g`,
		repo.id, repo.ownerID, repo.fullName, sampleData["ci.run_completed"], n)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}

// appendEventsOf stores n events of repo at once, as appendRunEvents does.
func (s *fakeStore) appendEventsOf(repo ownedRepo, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for range n {
		s.appendLocked(faucet.Event{
			Source: "github", DeliveryID: fmt.Sprintf("bulk-%d", s.lastID+1), Type: "ci.run_completed",
			RepoID: repo.id, OwnerID: repo.ownerID, RepoName: repo.fullName, Data: json.RawMessage(sampleData["ci.run_completed"]),
		})
	}
}

// TestALegacyCursorRestartsAtTheHeadAsTruncated: a v1 cursor is an event ID
// that counts every repository's events, so faucet never reads one as a
// place in a target's events. It answers as if the caller lost events, and
// goes on from the head.
func TestALegacyCursorRestartsAtTheHeadAsTruncated(t *testing.T) {
	t.Parallel()
	h := newWebhookHarness(t)
	first := h.insert(repoID, "ci.run_completed", "")
	h.insert(otherRepoID, "ci.run_completed", "")
	h.insert(repoID, "ci.run_completed", "")
	head := h.headCursor(repoArgs())

	for _, legacy := range []string{"v1.0", fmt.Sprintf("v1.%d", first.ID), "v1.999999"} {
		got := h.poll(pollParams("ci.run_completed", repoArgs(), cursorOf(legacy)))
		require.Empty(t, got.Events, legacy)
		require.True(t, got.Truncated, legacy)
		require.Equal(t, head, got.Cursor, legacy)

		got = h.poll(&rawParams{Name: "ci.run_completed", Arguments: raw(repoArgs()), Cursor: cursorOf(legacy), MaxAgeMs: raw(int64(time.Hour / time.Millisecond))})
		require.Empty(t, got.Events, "%s with maxAgeMs", legacy)
		require.True(t, got.Truncated, "%s with maxAgeMs", legacy)
		require.Equal(t, head, got.Cursor, "%s with maxAgeMs", legacy)
	}

	params := subscribeTo(repoArgs(), callbackURL, newSecret(t, 32))
	params.Cursor = cursorOf("v1.0")
	sub := h.subscribe(params)
	require.True(t, sub.Truncated, "a subscribe from a v1 cursor reports the restart")
	require.Equal(t, head, sub.Cursor, "and starts at the head")
}

// TestTheWaitToolRefusesAnIntegerCursor: an integer was the wait tool's old
// cursor, an event ID that counts every repository's events. The schema
// refuses it rather than read it as a place in the target's events, which
// would skip events.
func TestTheWaitToolRefusesAnIntegerCursor(t *testing.T) {
	t.Parallel()
	h := newWaitHarness(t)
	h.insert(repoID, "ci.run_completed", "")
	text := h.call(args("repo", testRepo, "types", []string{"ci.run_completed"}, "cursor", 0, "timeout_s", 1)).failure(t)
	require.Contains(t, text, "cursor")
	require.Contains(t, text, "string")
}

// TestTheWaitToolAndEventsPollShareOneCursor: a cursor of either reads on
// from the same place in the other.
func TestTheWaitToolAndEventsPollShareOneCursor(t *testing.T) {
	t.Parallel()
	h := newEventsHarness(t)
	start := h.headCursor(repoArgs())
	first := h.insert(repoID, "ci.run_completed", "")
	h.insert(otherRepoID, "ci.run_completed", "")

	wait := func(cursor any) map[string]any {
		t.Helper()
		res, err := h.session.CallTool(h.ctx, &mcp.CallToolParams{Name: waitTool, Arguments: args("repo", testRepo, "types", []string{"ci.run_completed"}, "cursor", cursor, "timeout_s", 1)})
		require.NoError(t, err)
		require.False(t, res.IsError, "tool error: %v", res.Content)
		var out map[string]any
		require.NoError(t, json.Unmarshal(raw(res.StructuredContent), &out))
		return out
	}
	waited := wait(start)
	require.Len(t, waited["events"], 1)
	cursor, ok := waited["cursor"].(string)
	require.True(t, ok, "the wait tool's cursor is a string, as events/poll's is: %v", waited["cursor"])

	polled := h.poll(pollParams("ci.run_completed", repoArgs(), cursorOf(start)))
	require.Equal(t, []string{first.DeliveryID}, polled.eventIDs())
	require.Equal(t, polled.Cursor, cursor, "both stop at the same place")

	second := h.insert(repoID, "ci.run_completed", "")
	again := wait(polled.Cursor)
	require.Len(t, again["events"], 1)
	require.Equal(t, []string{second.DeliveryID}, h.poll(pollParams("ci.run_completed", repoArgs(), cursorOf(cursor))).eventIDs())
}

// TestACursorAboveTheHeadReadsOnFromTheHead: a cursor that no answer gave,
// above the target's newest event, is read as the head, so the target's next
// event is not skipped.
func TestACursorAboveTheHeadReadsOnFromTheHead(t *testing.T) {
	t.Parallel()
	h := newEventsHarness(t)
	h.insert(repoID, "ci.run_completed", "")
	h.insert(otherRepoID, "ci.run_completed", "")
	h.insert(repoID, "ci.run_completed", "")

	got := h.poll(pollParams("ci.run_completed", repoArgs(), cursorOf("v2.1000")))
	require.Empty(t, got.Events)
	require.False(t, got.Truncated)
	require.Equal(t, "v2.2", got.Cursor, "the head of octo/repo is its second event")

	next := h.insert(repoID, "ci.run_completed", "")
	require.Equal(t, []string{next.DeliveryID}, h.poll(pollParams("ci.run_completed", repoArgs(), cursorOf(got.Cursor))).eventIDs())
}

// TestASubscribeThatAPurgeOvertakesStillReportsTheLoss: a purge that commits
// while events/subscribe works out where the subscription starts deletes
// events of the target above the cursor. The caller must hear of that loss,
// from the reply or from a gap envelope.
func TestASubscribeThatAPurgeOvertakesStillReportsTheLoss(t *testing.T) {
	t.Parallel()
	for storeName, newStore := range map[string]lineStore{"postgres": postgresLines, "fake": fakeLines} {
		t.Run(storeName, func(t *testing.T) {
			t.Parallel()
			overtaken := &purgeOvertakes{}
			w := newLineWorld(t, repoLineReader(), func(t *testing.T, repo ownedRepo) (faucet.Store, func(n int)) {
				s, bulk := newStore(t, repo)
				overtaken.Store = s
				return overtaken, bulk
			}, true)
			start := w.poll("the head", "").Cursor
			w.own()
			w.other(2)
			w.own()
			overtaken.arm(clearInstant())

			sub := w.subscribe("a subscribe that a purge overtakes", start)
			require.True(t, overtaken.fired, "the purge committed while the subscribe ran")
			w.own()
			got := w.deliver("the first run")
			require.True(t, sub.Truncated || strings.Contains(got, `"type":"gap"`),
				"the target lost two events above the cursor, so the reply says truncated or a gap envelope follows: truncated %t, %s", sub.Truncated, got)
		})
	}
}

// purgeOvertakes is a store that, once armed, purges the events received
// before a time right after events/subscribe has worked out where the
// subscription starts and before it stores the subscription.
type purgeOvertakes struct {
	faucet.Store
	before time.Time
	fired  bool
}

func (s *purgeOvertakes) arm(before time.Time) { s.before = before }

func (s *purgeOvertakes) StartSubscription(ctx context.Context, q faucet.StartQuery) (faucet.Start, error) {
	start, err := s.Store.StartSubscription(ctx, q)
	if err == nil && !s.before.IsZero() && !s.fired {
		s.fired = true
		if _, err := s.Purge(ctx, s.before); err != nil {
			return faucet.Start{}, err
		}
	}
	return start, err
}

// TestARenewalThatMovesTheTargetAnswersOnTheNewLine: the owner login of a
// subscription names another account ID at its renewal. The reply's cursor
// is on the new account's line, so a resubscribe with it neither replays nor
// skips the new account's events.
func TestARenewalThatMovesTheTargetAnswersOnTheNewLine(t *testing.T) {
	t.Parallel()
	for storeName, newStore := range map[string]lineStore{"postgres": postgresLines, "fake": fakeLines} {
		t.Run(storeName, func(t *testing.T) {
			t.Parallel()
			grants := &fakeGrants{owners: map[string]int64{"coder": ownerID}}
			reader := ownerLineReader(t)
			reader.caller = []func(*faucet.Deps){asCaller(newServicePrincipal(t), grants)}
			w := newLineWorld(t, reader, newStore, false)
			moved := ownedRepo{id: 5001, ownerID: 4242, fullName: "coder/moved"}
			w.own()
			w.own()
			for i := range 3 {
				w.insert(fmt.Sprintf("moved-%d", i), moved, sampleData["ci.run_completed"])
			}
			first := w.subscribe("a subscribe", "")
			require.Equal(t, "v2.2", first.Cursor, "the head of the line of account %d", ownerID)

			grants.mu.Lock()
			grants.owners["coder"] = moved.ownerID
			grants.mu.Unlock()
			renewed := w.subscribe("a renewal", "")
			require.Equal(t, "v2.3", renewed.Cursor, "the position of the line of account %d at the subscription's start", moved.ownerID)

			w.insert("moved-next", moved, sampleData["ci.run_completed"])
			got := w.deliver("the run after the renewal")
			require.Contains(t, got, `"cursor":"v2.3"`, "the envelope's cursor is on the new line too")
		})
	}
}
