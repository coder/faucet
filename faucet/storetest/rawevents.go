package storetest

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
)

// RawStore is a store that keeps the event log and the raw events in one
// database.
type RawStore interface {
	faucet.Store
	faucet.RawStore
}

type rawSubtest struct {
	name string
	run  func(t *testing.T, s RawStore)
}

// RunRaw runs the raw event contract against stores returned by newStore.
// Every call to newStore must return an empty store that does not share state
// with the others, because the subtests run in parallel.
func RunRaw(t *testing.T, newStore func(t *testing.T) RawStore) {
	groups := [][]rawSubtest{rawIngestSubtests, rawLeaseSubtests, rawDeliverySubtests, rawPurgeSubtests, rawConcurrencySubtests, rawLimitSubtests, rawTargetSubtests}
	for _, group := range groups {
		for _, st := range group {
			t.Run(st.name, func(t *testing.T) {
				t.Parallel()
				st.run(t, newStore(t))
			})
		}
	}
}

var rawIngestSubtests = []rawSubtest{
	{"IngestRawWritesNothingWithoutAnInterest", ingestRawWritesNothingWithoutAnInterest},
	{"IngestRawWritesNoPlanAfterItsContextEnded", ingestRawWritesNoPlanAfterItsContextEnded},
	{"IngestRawGivesThePlanEachMatchingInterestOnce", ingestRawGivesThePlanEachMatchingInterestOnce},
	{"IngestRawMatchesOnRepositoryEventLivenessAndAction", ingestRawMatchesOnRepositoryEventLivenessAndAction},
	{"IngestRawWritesWhatThePlanSays", ingestRawWritesWhatThePlanSays},
	{"IngestRawStoresTheBodyAndFieldsByteForByte", ingestRawStoresTheBodyAndFieldsByteForByte},
	{"IngestRawReportsADuplicateAndWritesNothing", ingestRawReportsADuplicateAndWritesNothing},
	{"IngestRawRefusesAnInvalidPlan", ingestRawRefusesAnInvalidPlan},
	{"IngestRawAssignsRisingIDsAndReceivedAtNeverGoesBack", ingestRawAssignsRisingIDsAndReceivedAtNeverGoesBack},
	{"IngestRawCountsPendingPerInterest", ingestRawCountsPendingPerInterest},
}

const (
	rawRepo    int64 = 42
	rawOwner   int64 = 7
	rawEvent         = "push"
	rawMaxLive       = 100
	// rawMaxPerTarget is the limit of interests per event and repository
	// that the cases pass, except those of that limit.
	rawMaxPerTarget = 1000
)

var rawBody = []byte(`{"ref":"refs/heads/main"}`)

// rawSpec says what a raw subscription or a lease wants. Its zero value is
// every push of rawRepo, whole, for octocat. A target, when set, replaces
// the repository repoID, and repos limits it to those full names.
type rawSpec struct {
	principal string
	event     string
	repoID    int64
	target    faucet.Target
	repos     []string
	actions   []string
	fields    []string
}

func (w rawSpec) targetOf() faucet.Target {
	if w.target.Kind() != "" {
		return w.target
	}
	return faucet.RepoTarget(w.repoID)
}

func (w rawSpec) withDefaults() rawSpec {
	if w.principal == "" {
		w.principal = "octocat"
	}
	if w.event == "" {
		w.event = rawEvent
	}
	if w.repoID == 0 {
		w.repoID = rawRepo
	}
	return w
}

func (w rawSpec) arguments() json.RawMessage {
	args := `{"repo":"octo/repo"`
	switch w.target.Kind() {
	case faucet.TargetOwner:
		args = `{"owner":"octo"`
	case faucet.TargetAll:
		args = `{"all":true`
	}
	for _, member := range []struct {
		name   string
		values []string
	}{{"actions", w.actions}, {"fields", w.fields}, {"repos", w.repos}} {
		if len(member.values) == 0 {
			continue
		}
		list, err := json.Marshal(member.values)
		if err != nil {
			panic(err)
		}
		args += fmt.Sprintf(`,%q:%s`, member.name, list)
	}
	return json.RawMessage(args + "}")
}

// newRawSubscription returns the nth subscription of a raw event name, as the
// service writes one: with the position that the delivery queue never reads.
func newRawSubscription(n int, w rawSpec, expiresAt time.Time) faucet.Subscription {
	w = w.withDefaults()
	eventType, callbackURL := "github."+w.event, fmt.Sprintf("https://hooks.example.test/raw/%d", n)
	return faucet.Subscription{
		KeyHash:     faucet.NewSubscriptionKeyHash(w.principal, callbackURL, eventType, w.arguments()),
		Principal:   w.principal,
		CallbackURL: callbackURL,
		EventType:   eventType,
		EventTypes:  []string{eventType},
		Arguments:   w.arguments(),
		Target:      w.targetOf(),
		SecretEnc:   []byte("sealed-1"),
		Cursor:      math.MaxInt64,
		ExpiresAt:   expiresAt,
	}
}

func newRawLease(n uint64, w rawSpec, expiresAt time.Time) faucet.RawLease {
	w = w.withDefaults()
	lease := faucet.RawLease{
		Key:       faucet.NewSubscriptionKeyHash(w.principal, "", "github."+w.event, w.arguments()),
		Principal: w.principal,
		Event:     w.event,
		Actions:   w.actions,
		Fields:    w.fields,
		Target:    w.targetOf(),
		RepoNames: w.repos,
		ExpiresAt: expiresAt,
	}
	binary.BigEndian.PutUint64(lease.ID[8:], n)
	return lease
}

// rawRig drives one store at a fixed time. Its counters are atomic, so that
// goroutines can share it.
type rawRig struct {
	t          *testing.T
	s          RawStore
	ctx        context.Context
	now        time.Time
	deliveries atomic.Int64
	leases     atomic.Uint64
}

// rawLimits are the limits of the cases with live as the limit of a
// principal, and others that they never reach.
func rawLimits(live int) faucet.RawLimits {
	return faucet.RawLimits{Live: live, PerTarget: rawMaxPerTarget, PerPrincipalTarget: rawMaxPerTarget}
}

func newRawRig(t *testing.T, s RawStore) *rawRig {
	t.Helper()
	return &rawRig{t: t, s: s, ctx: context.Background(), now: subscriptionBase}
}

// subscribe stores the nth raw subscription, which lives for an hour, and
// returns the stored row.
func (r *rawRig) subscribe(n int, w rawSpec) faucet.Subscription {
	r.t.Helper()
	return r.subscribeAt(n, w, r.now)
}

func (r *rawRig) subscribeAt(n int, w rawSpec, now time.Time) faucet.Subscription {
	r.t.Helper()
	row, err := r.s.UpsertRawSubscription(r.ctx, newRawSubscription(n, w, now.Add(time.Hour)), now, rawLimits(rawMaxLive))
	require.NoError(r.t, err)
	return row
}

// open stores a new lease, which lives for an hour.
func (r *rawRig) open(w rawSpec) faucet.RawLease {
	r.t.Helper()
	lease := newRawLease(r.leases.Add(1), w, r.now.Add(time.Hour))
	_, err := r.s.OpenRawLease(r.ctx, lease, r.now, rawLimits(rawMaxLive))
	require.NoError(r.t, err)
	return lease
}

func (r *rawRig) head() int64 {
	r.t.Helper()
	head, err := r.s.RawHead(r.ctx)
	require.NoError(r.t, err)
	return head
}

// newDelivery returns a push to rawRepo with a delivery ID of its own.
func (r *rawRig) newDelivery() faucet.RawDelivery {
	return faucet.RawDelivery{DeliveryID: fmt.Sprintf("raw-delivery-%d", r.deliveries.Add(1)), Event: rawEvent, RepoID: rawRepo}
}

type rawPlanner = func([]faucet.RawInterest) faucet.RawPlan

// serveAll plans a delivery for every subscription and an unread event for
// every lease.
func serveAll(body []byte) rawPlanner {
	return func(interests []faucet.RawInterest) faucet.RawPlan {
		plan := faucet.RawPlan{Body: body}
		for _, in := range interests {
			if in.IsLease {
				plan.Unread = append(plan.Unread, in.Lease)
				continue
			}
			plan.Deliveries = append(plan.Deliveries, faucet.RawOutbox{Key: in.Subscription, Incarnation: in.Incarnation})
		}
		return plan
	}
}

func (r *rawRig) ingestAt(d faucet.RawDelivery, now time.Time, plan rawPlanner) faucet.RawIngest {
	r.t.Helper()
	res, err := r.s.IngestRaw(r.ctx, d, now, plan)
	require.NoError(r.t, err)
	return res
}

// store ingests d, which must be stored, and returns its raw ID.
func (r *rawRig) store(d faucet.RawDelivery, plan rawPlanner) int64 {
	r.t.Helper()
	before := r.head()
	res := r.ingestAt(d, r.now, plan)
	require.True(r.t, res.Matched, "an interest wants the delivery")
	require.False(r.t, res.Duplicate)
	require.Greater(r.t, res.ID, before, "a stored event gets an ID above the head")
	return res.ID
}

// push stores a new push for every interest that matches it.
func (r *rawRig) push() int64 {
	r.t.Helper()
	return r.store(r.newDelivery(), serveAll(rawBody))
}

// interestsAt ingests d for every interest that matches at now and returns
// the interests that the plan got, or nil when the store did not ask for one.
func (r *rawRig) interestsAt(d faucet.RawDelivery, now time.Time) []faucet.RawInterest {
	r.t.Helper()
	var (
		calls int
		got   []faucet.RawInterest
	)
	res := r.ingestAt(d, now, func(interests []faucet.RawInterest) faucet.RawPlan {
		calls++
		got = interests
		return serveAll(rawBody)(interests)
	})
	if calls == 0 {
		require.Equal(r.t, faucet.RawIngest{}, res, "an ingest that matches nothing reports nothing")
		return nil
	}
	require.Equal(r.t, 1, calls, "the store plans once")
	require.NotEmpty(r.t, got, "the store plans only for interests")
	require.True(r.t, res.Matched)
	return got
}

func subscriptionInterest(t *testing.T, interests []faucet.RawInterest, sub faucet.Subscription) faucet.RawInterest {
	t.Helper()
	var found []faucet.RawInterest
	for _, in := range interests {
		if !in.IsLease && in.Subscription == sub.KeyHash {
			found = append(found, in)
		}
	}
	require.Len(t, found, 1, "the plan gets the subscription once")
	require.True(t, found[0].Incarnation.Equal(sub.CreatedAt), "the incarnation is the subscription's creation time")
	return found[0]
}

func leaseInterest(t *testing.T, interests []faucet.RawInterest, lease faucet.RawLease) faucet.RawInterest {
	t.Helper()
	var found []faucet.RawInterest
	for _, in := range interests {
		if in.IsLease && in.Lease == lease.ID {
			found = append(found, in)
		}
	}
	require.Len(t, found, 1, "the plan gets the lease once")
	return found[0]
}

func (r *rawRig) query(lease faucet.RawLease, after int64) faucet.RawRead {
	return faucet.RawRead{
		Lease: lease.ID, Key: lease.Key, Target: lease.Target, After: after,
		Limit: 100, MaxBytes: 1 << 30, Now: r.now, ExpiresAt: r.now.Add(time.Hour),
	}
}

// read polls a lease that must be live.
func (r *rawRig) read(q faucet.RawRead) faucet.RawPage {
	r.t.Helper()
	page, found, err := r.s.ReadRawLease(r.ctx, q)
	require.NoError(r.t, err)
	require.True(r.t, found, "the lease is live")
	return page
}

func rawIDs(events []faucet.RawEvent) []int64 {
	ids := make([]int64, 0, len(events))
	for _, ev := range events {
		ids = append(ids, ev.ID)
	}
	return ids
}

func (r *rawRig) claim(now time.Time) (faucet.RawDeliveryClaim, bool) {
	r.t.Helper()
	claim, ok, err := r.s.ClaimRawDelivery(r.ctx, now, testLease, nil)
	require.NoError(r.t, err)
	return claim, ok
}

func (r *rawRig) mustClaim(now time.Time) faucet.RawDeliveryClaim {
	r.t.Helper()
	claim, ok := r.claim(now)
	require.True(r.t, ok, "a raw delivery is due at %s", now)
	return claim
}

// claimAll claims every raw delivery that is due at now.
func (r *rawRig) claimAll(now time.Time) []faucet.RawDeliveryClaim {
	r.t.Helper()
	var claims []faucet.RawDeliveryClaim
	for {
		claim, ok := r.claim(now)
		if !ok {
			return claims
		}
		claims = append(claims, claim)
	}
}

func (r *rawRig) finish(ref faucet.DeliveryRef, now time.Time, f faucet.Finish) faucet.FinishResult {
	r.t.Helper()
	res, err := r.s.FinishRawDelivery(r.ctx, ref, now, f)
	require.NoError(r.t, err)
	return res
}

func (r *rawRig) claimGap(now time.Time) (faucet.GapClaim, bool) {
	r.t.Helper()
	claim, ok, err := r.s.ClaimRawGap(r.ctx, now, testLease, nil)
	require.NoError(r.t, err)
	return claim, ok
}

func (r *rawRig) purge(p faucet.RawPurge) faucet.RawPurgeResult {
	r.t.Helper()
	res, err := r.s.PurgeRawEvents(r.ctx, p)
	require.NoError(r.t, err)
	return res
}

func ingestRawWritesNoPlanAfterItsContextEnded(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	r.subscribe(1, rawSpec{})
	lease := r.open(rawSpec{})
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	_, err := s.IngestRaw(ctx, r.newDelivery(), r.now, func(interests []faucet.RawInterest) faucet.RawPlan {
		cancel()
		return serveAll(rawBody)(interests)
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, r.head(), "a plan that its context outlived is not written")
	require.Empty(t, r.claimAll(r.now))
	require.Empty(t, r.read(r.query(lease, 0)).Events)
}

func ingestRawWritesNothingWithoutAnInterest(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	unplanned := func([]faucet.RawInterest) faucet.RawPlan {
		t.Error("the store planned a delivery that no interest wants")
		return faucet.RawPlan{}
	}
	d := r.newDelivery()
	require.Zero(t, r.head())
	require.Equal(t, faucet.RawIngest{}, r.ingestAt(d, r.now, unplanned))
	require.Zero(t, r.head(), "a delivery that nobody wants is not stored")

	sub := r.subscribe(1, rawSpec{})
	id := r.store(d, serveAll(rawBody))
	require.Equal(t, id, r.head(), "the delivery was not stored before, so it is no duplicate")

	deleted, err := s.DeleteSubscription(r.ctx, sub.Principal, sub.KeyHash)
	require.NoError(t, err)
	require.True(t, deleted)
	require.Equal(t, faucet.RawIngest{}, r.ingestAt(d, r.now, unplanned), "no interest wins over a stored delivery ID")
	require.Equal(t, id, r.head())
}

func ingestRawGivesThePlanEachMatchingInterestOnce(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	typed := newSubscription("octocat", "https://hooks.example.test/typed", r.now.Add(time.Hour))
	typed.Target = faucet.RepoTarget(rawRepo)
	_, err := s.UpsertSubscription(r.ctx, typed, r.now, rawMaxLive)
	require.NoError(t, err)
	projected := r.subscribe(1, rawSpec{fields: []string{"ref"}})
	whole := r.subscribe(2, rawSpec{})
	projectedLease := r.open(rawSpec{fields: []string{"after", "before"}})
	wholeLease := r.open(rawSpec{})

	interests := r.interestsAt(r.newDelivery(), r.now)
	require.Len(t, interests, 4, "the two subscriptions and the two leases, and not the typed subscription")

	in := subscriptionInterest(t, interests, projected)
	require.Equal(t, []string{"ref"}, in.Fields)
	require.Zero(t, in.Pending)
	in = subscriptionInterest(t, interests, whole)
	require.Nil(t, in.Fields, "no field paths means the whole payload")
	require.Zero(t, in.Pending)
	in = leaseInterest(t, interests, projectedLease)
	require.Equal(t, []string{"after", "before"}, in.Fields)
	require.Zero(t, in.Pending)
	in = leaseInterest(t, interests, wholeLease)
	require.Nil(t, in.Fields, "no field paths means the whole payload")
	require.Zero(t, in.Pending)
}

func ingestRawMatchesOnRepositoryEventLivenessAndAction(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	cases := []struct {
		name    string
		actions []string
		change  func(d *faucet.RawDelivery)
		after   time.Duration
		want    bool
	}{
		{name: "another repository", change: func(d *faucet.RawDelivery) { d.RepoID += 1_000_000 }},
		{name: "another event", change: func(d *faucet.RawDelivery) { d.Event = "issues" }},
		{name: "an expired interest", after: time.Hour},
		{name: "actions that do not hold the delivery's", actions: []string{"opened"}, change: func(d *faucet.RawDelivery) { d.Action = "closed" }},
		{name: "actions and a delivery without one", actions: []string{"opened"}},
		{name: "actions that hold the delivery's", actions: []string{"closed", "opened"}, change: func(d *faucet.RawDelivery) { d.Action = "closed" }, want: true},
		{name: "no actions and a delivery with one", change: func(d *faucet.RawDelivery) { d.Action = "closed" }, want: true},
		{name: "no actions and a delivery without one", want: true},
		{name: "an interest in its last instant", after: time.Hour - time.Microsecond, want: true},
	}
	for i, tc := range cases {
		for _, isLease := range []bool{false, true} {
			w := rawSpec{repoID: int64(1000 + 2*i), actions: tc.actions}
			var (
				sub   faucet.Subscription
				lease faucet.RawLease
			)
			if isLease {
				w.repoID++
				lease = r.open(w)
			} else {
				sub = r.subscribe(i, w)
			}
			d := r.newDelivery()
			d.RepoID = w.repoID
			if tc.change != nil {
				tc.change(&d)
			}
			interests := r.interestsAt(d, r.now.Add(tc.after))
			switch {
			case !tc.want:
				require.Empty(t, interests, "%s, lease %t", tc.name, isLease)
			case isLease:
				require.Len(t, interests, 1, "%s, lease", tc.name)
				leaseInterest(t, interests, lease)
			default:
				require.Len(t, interests, 1, "%s, subscription", tc.name)
				subscriptionInterest(t, interests, sub)
			}
		}
	}
}

func ingestRawWritesWhatThePlanSays(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	served, left := r.subscribe(1, rawSpec{}), r.subscribe(2, rawSpec{})
	servedLease, leftLease := r.open(rawSpec{}), r.open(rawSpec{})

	d := r.newDelivery()
	d.Action = "created"
	id := r.store(d, func(interests []faucet.RawInterest) faucet.RawPlan {
		subscriptionInterest(t, interests, left)
		leaseInterest(t, interests, leftLease)
		return faucet.RawPlan{
			Body:       rawBody,
			Deliveries: []faucet.RawOutbox{{Key: served.KeyHash, Incarnation: served.CreatedAt}},
			Unread:     []faucet.RawLeaseID{servedLease.ID},
		}
	})

	claims := r.claimAll(r.now)
	require.Len(t, claims, 1, "only the subscription that the plan names gets a delivery")
	claim := claims[0]
	require.Equal(t, faucet.DeliveryRef{Key: served.KeyHash, EventID: id, Attempt: 1, Incarnation: claim.Ref.Incarnation}, claim.Ref)
	require.True(t, claim.Ref.Incarnation.Equal(served.CreatedAt))
	require.True(t, claim.EnqueuedAt.Equal(r.now), "a delivery is enqueued at the time of the ingest")
	require.NotNil(t, claim.Event)
	require.Equal(t, id, claim.Event.ID)
	require.Equal(t, d.DeliveryID, claim.Event.DeliveryID)
	require.Equal(t, "created", claim.Event.Action)
	_, ok := r.claimGap(r.now)
	require.False(t, ok, "the plan names no gap")

	page := r.read(r.query(servedLease, 0))
	require.Equal(t, []int64{id}, rawIDs(page.Events))
	require.Zero(t, page.Skipped)
	page = r.read(r.query(leftLease, 0))
	require.Empty(t, page.Events, "a lease that the plan left out gets nothing")
	require.Zero(t, page.Skipped, "and has no lost mark")
}

func ingestRawStoresTheBodyAndFieldsByteForByte(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	sub, lease := r.subscribe(1, rawSpec{}), r.open(rawSpec{})
	large := append(append([]byte(`{"blob":"`), bytes.Repeat([]byte("x"), 1<<20)...), `"}`...)
	cases := []struct {
		name   string
		body   []byte
		fields []string
	}{
		{"an escaped NUL", []byte(`{"text":"a\u0000b"}`), []string{"text"}},
		{"bytes that are not UTF-8", []byte("{\"text\":\"\xff\xfe\x80\"}"), nil},
		{"1 MiB", large, []string{"blob", "repository.full_name"}},
		{"whitespace and key order", []byte("{ \"b\" : 1,\n\t\"a\" : 2, \"b\" : 3 }"), nil},
	}
	ids := make([]int64, len(cases))
	for i, tc := range cases {
		ids[i] = r.store(r.newDelivery(), func([]faucet.RawInterest) faucet.RawPlan {
			return faucet.RawPlan{
				Body: tc.body, Fields: tc.fields,
				Deliveries: []faucet.RawOutbox{{Key: sub.KeyHash, Incarnation: sub.CreatedAt}},
				Unread:     []faucet.RawLeaseID{lease.ID},
			}
		})
	}
	same := func(i int, ev faucet.RawEvent) {
		t.Helper()
		tc := cases[i]
		require.Equal(t, ids[i], ev.ID)
		require.True(t, bytes.Equal(tc.body, ev.Body), "%s: the body is stored byte for byte", tc.name)
		if tc.fields == nil {
			require.Nil(t, ev.Fields, "%s: no field paths with a body means the whole payload", tc.name)
		} else {
			require.Equal(t, tc.fields, ev.Fields, tc.name)
		}
		require.Equal(t, rawEvent, ev.Event)
		require.Equal(t, rawRepo, ev.RepoID)
		require.False(t, ev.ReceivedAt.IsZero())
	}

	page := r.read(r.query(lease, 0))
	require.Equal(t, ids, rawIDs(page.Events))
	for i, ev := range page.Events {
		same(i, ev)
	}
	claims := r.claimAll(r.now)
	require.Len(t, claims, len(cases))
	for i, claim := range claims {
		require.NotNil(t, claim.Event)
		same(i, *claim.Event)
	}
}

func ingestRawReportsADuplicateAndWritesNothing(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	sub, lease := r.subscribe(1, rawSpec{}), r.open(rawSpec{})
	d := r.newDelivery()
	id := r.store(d, serveAll(rawBody))

	duplicate := faucet.RawIngest{Matched: true, Duplicate: true}
	require.Equal(t, duplicate, r.ingestAt(d, r.now, serveAll(rawBody)))
	require.Equal(t, duplicate, r.ingestAt(d, r.now, func([]faucet.RawInterest) faucet.RawPlan {
		return faucet.RawPlan{
			Body:   []byte(`{"other":true}`),
			Gaps:   []faucet.RawGap{{Key: sub.KeyHash, Incarnation: sub.CreatedAt}},
			Lost:   []faucet.RawLeaseID{lease.ID},
			Fields: []string{"other"},
		}
	}), "a second plan for a stored delivery ID is not written")
	require.Equal(t, id, r.head())

	page := r.read(r.query(lease, 0))
	require.Equal(t, []int64{id}, rawIDs(page.Events))
	require.Equal(t, rawBody, []byte(page.Events[0].Body))
	require.Nil(t, page.Events[0].Fields)
	require.Zero(t, page.Skipped, "the second plan's lost mark was not written")
	_, ok := r.claimGap(r.now)
	require.False(t, ok, "the second plan's gap was not written")
	claims := r.claimAll(r.now)
	require.Len(t, claims, 1)
	require.True(t, r.finish(claims[0].Ref, r.now, faucet.Finish{Kind: faucet.FinishDelivered}).Accepted)
	r.read(r.query(lease, id))

	require.Equal(t, int64(1), r.purge(faucet.RawPurge{Now: r.now}).Cleared)
	require.Equal(t, duplicate, r.ingestAt(d, r.now, serveAll(rawBody)), "an event without a body keeps its delivery ID")
	require.Equal(t, id, r.head())
	require.Empty(t, r.claimAll(r.now.Add(time.Minute)))
	page = r.read(r.query(lease, id))
	require.Empty(t, page.Events)
	require.Zero(t, page.Skipped)
}

func ingestRawRefusesAnInvalidPlan(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	sub, lease := r.subscribe(1, rawSpec{}), r.open(rawSpec{})
	delivery := faucet.RawOutbox{Key: sub.KeyHash, Incarnation: sub.CreatedAt}
	d := r.newDelivery()
	for name, plan := range map[string]faucet.RawPlan{
		"field paths without a body":   {Fields: []string{"ref"}},
		"a delivery without a body":    {Deliveries: []faucet.RawOutbox{delivery}},
		"an unread event without body": {Unread: []faucet.RawLeaseID{lease.ID}},
	} {
		require.Error(t, plan.Validate(), name)
		_, err := s.IngestRaw(r.ctx, d, r.now, func([]faucet.RawInterest) faucet.RawPlan { return plan })
		require.Error(t, err, name)
	}
	require.Zero(t, r.head(), "a refused plan stores no event")
	require.Empty(t, r.claimAll(r.now))
	page := r.read(r.query(lease, 0))
	require.Empty(t, page.Events)
	require.Zero(t, page.Skipped)

	id := r.store(d, serveAll(rawBody))
	require.Equal(t, id, r.head(), "a refused plan does not keep the delivery ID")
}

func ingestRawAssignsRisingIDsAndReceivedAtNeverGoesBack(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	lease := r.open(rawSpec{})
	var ids []int64
	for range 5 {
		id := r.push()
		if len(ids) > 0 {
			require.Greater(t, id, ids[len(ids)-1])
		}
		require.Equal(t, id, r.head())
		ids = append(ids, id)
	}
	page := r.read(r.query(lease, 0))
	require.Equal(t, ids, rawIDs(page.Events))
	for i, ev := range page.Events {
		require.False(t, ev.ReceivedAt.IsZero())
		if i > 0 {
			require.False(t, ev.ReceivedAt.Before(page.Events[i-1].ReceivedAt), "a later event is never received before an earlier one")
		}
	}
}

func ingestRawCountsPendingPerInterest(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	sub, lease := r.subscribe(1, rawSpec{}), r.open(rawSpec{})
	pending := func(plan rawPlanner) (subscription, unread int32) {
		t.Helper()
		r.store(r.newDelivery(), func(interests []faucet.RawInterest) faucet.RawPlan {
			subscription = subscriptionInterest(t, interests, sub).Pending
			unread = leaseInterest(t, interests, lease).Pending
			return plan(interests)
		})
		return subscription, unread
	}
	first, second := r.push(), r.push()
	r.push()
	unsent, unread := pending(serveAll(rawBody))
	require.Equal(t, int32(3), unsent)
	require.Equal(t, int32(3), unread)

	claim := r.mustClaim(r.now)
	require.Equal(t, first, claim.Ref.EventID)
	unsent, _ = pending(serveAll(rawBody))
	require.Equal(t, int32(4), unsent, "a claimed delivery is still unsent")
	require.True(t, r.finish(claim.Ref, r.now, faucet.Finish{Kind: faucet.FinishDelivered}).Accepted)
	r.read(r.query(lease, second))

	unsent, unread = pending(func(interests []faucet.RawInterest) faucet.RawPlan {
		plan := serveAll(rawBody)(interests)
		plan.Unread, plan.Lost = nil, []faucet.RawLeaseID{lease.ID}
		return plan
	})
	require.Equal(t, int32(4), unsent, "five deliveries, less the one that was delivered")
	require.Equal(t, int32(3), unread, "five events, less the two that the poller acknowledged")

	_, unread = pending(serveAll(rawBody))
	require.Equal(t, int32(3), unread, "a lost mark is not an unread event")
}
