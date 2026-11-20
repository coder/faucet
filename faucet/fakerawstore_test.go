package faucet_test

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/storetest"
)

type fakeRawOutboxKey struct {
	key   faucet.SubscriptionKeyHash
	rawID int64
}

type fakeRawOutbox struct {
	incarnation time.Time
	enqueuedAt  time.Time
	nextAttempt time.Time
	attempts    int32
	leaseUntil  time.Time
}

// fakeRawGapKey names a gap by the subscription's key and the UnixNano of its
// incarnation, because a time.Time is no map key.
type fakeRawGapKey struct {
	key         faucet.SubscriptionKeyHash
	incarnation int64
}

type fakeRawLease struct {
	lease    faucet.RawLease
	acked    int64
	polledAt time.Time
	unread   map[int64]struct{}
	// lost is the raw ID of the lease's one lost mark, or 0.
	lost int64
}

func (l *fakeRawLease) pending() int32 {
	var n int32
	for range l.unread {
		n++
	}
	return n
}

const fakeRawPrefix = "github."

func (s *fakeStore) initRawLocked() {
	if s.rawOutbox == nil {
		s.rawOutbox = map[fakeRawOutboxKey]*fakeRawOutbox{}
		s.rawGaps = map[fakeRawGapKey]*fakeGap{}
		s.rawLeases = map[faucet.RawLeaseID]*fakeRawLease{}
	}
}

// beginRaw locks the store and reports the error that every method returns
// first. The caller unlocks.
func (s *fakeStore) beginRaw(ctx context.Context) error {
	s.mu.Lock()
	s.initRawLocked()
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.err
}

func (s *fakeStore) rawHeadLocked() int64 {
	if len(s.rawEvents) == 0 {
		return 0
	}
	return s.rawEvents[len(s.rawEvents)-1].ID
}

func (s *fakeStore) rawEventLocked(id int64) (faucet.RawEvent, bool) {
	i, ok := slices.BinarySearchFunc(s.rawEvents, id, func(e faucet.RawEvent, id int64) int { return cmp.Compare(e.ID, id) })
	if !ok {
		return faucet.RawEvent{}, false
	}
	ev := s.rawEvents[i]
	ev.Body, ev.Fields = slices.Clone(ev.Body), slices.Clone(ev.Fields)
	return ev, true
}

// rawSubscriptionLiveLocked reports whether the subscription of key is live
// at now and is still the incarnation.
func (s *fakeStore) rawSubscriptionLiveLocked(key faucet.SubscriptionKeyHash, incarnation, now time.Time) (faucet.Subscription, bool) {
	sub, ok := s.subs[key]
	return sub, ok && sub.CreatedAt.Equal(incarnation) && sub.ExpiresAt.After(now)
}

func (s *fakeStore) rawPendingLocked(key faucet.SubscriptionKeyHash, incarnation time.Time) int32 {
	var n int32
	for k, d := range s.rawOutbox {
		if k.key == key && d.incarnation.Equal(incarnation) {
			n++
		}
	}
	return n
}

type fakeRawArguments struct {
	Actions []string `json:"actions"`
	Fields  []string `json:"fields"`
	Repos   []string `json:"repos"`
}

func fakeRawArgumentsOf(sub faucet.Subscription) fakeRawArguments {
	var args fakeRawArguments
	_ = json.Unmarshal(sub.Arguments, &args)
	return args
}

func fakeRawWantsAction(actions []string, action string) bool {
	return len(actions) == 0 || slices.Contains(actions, action)
}

// fakeRawWantsRepository reports whether a list of lower-case full names,
// nil for none, lets a delivery of this repository name through.
func fakeRawWantsRepository(repos []string, repoName string) bool {
	return repos == nil || slices.Contains(repos, strings.ToLower(repoName))
}

func (s *fakeStore) rawInterestsLocked(d faucet.RawDelivery, now time.Time) []faucet.RawInterest {
	var interests []faucet.RawInterest
	for key, sub := range s.subs {
		args := fakeRawArgumentsOf(sub)
		if sub.EventType != fakeRawPrefix+d.Event || !sub.Target.Covers(d.RepoID, d.OwnerID) || !sub.ExpiresAt.After(now) ||
			!fakeRawWantsAction(args.Actions, d.Action) || !fakeRawWantsRepository(args.Repos, d.RepoName) {
			continue
		}
		interests = append(interests, faucet.RawInterest{
			Subscription: key, Incarnation: sub.CreatedAt, Fields: args.Fields, Pending: s.rawPendingLocked(key, sub.CreatedAt),
		})
	}
	for id, l := range s.rawLeases {
		if l.lease.Event != d.Event || !l.lease.Target.Covers(d.RepoID, d.OwnerID) || !l.lease.ExpiresAt.After(now) ||
			!fakeRawWantsAction(l.lease.Actions, d.Action) || len(l.lease.RepoNames) > 0 && !fakeRawWantsRepository(l.lease.RepoNames, d.RepoName) {
			continue
		}
		interests = append(interests, faucet.RawInterest{Lease: id, IsLease: true, Fields: slices.Clone(l.lease.Fields), Pending: l.pending()})
	}
	slices.SortFunc(interests, func(a, b faucet.RawInterest) int {
		return cmp.Or(bytes.Compare(a.Subscription[:], b.Subscription[:]), bytes.Compare(a.Lease[:], b.Lease[:]))
	})
	return interests
}

func (s *fakeStore) IngestRaw(ctx context.Context, d faucet.RawDelivery, now time.Time, plan func([]faucet.RawInterest) faucet.RawPlan) (faucet.RawIngest, error) {
	err := s.beginRaw(ctx)
	if err == nil {
		err = d.Validate()
	}
	if err != nil {
		s.mu.Unlock()
		return faucet.RawIngest{}, err
	}
	interests := s.rawInterestsLocked(d, now)
	s.mu.Unlock()
	if len(interests) == 0 {
		return faucet.RawIngest{}, nil
	}
	// The plan runs without the mutex, as a store plans outside its
	// transaction: an interest can end before the plan is written.
	p := plan(interests)
	if err := ctx.Err(); err != nil {
		return faucet.RawIngest{}, err
	}
	if err := p.Validate(); err != nil {
		return faucet.RawIngest{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.ContainsFunc(s.rawEvents, func(e faucet.RawEvent) bool { return e.DeliveryID == d.DeliveryID }) {
		return faucet.RawIngest{Matched: true, Duplicate: true}, nil
	}
	s.rawLastID++
	id := s.rawLastID
	if at := s.nowLocked(); at.After(s.rawReceivedAt) {
		s.rawReceivedAt = at
	}
	s.rawEvents = append(s.rawEvents, faucet.RawEvent{
		ID: id, DeliveryID: d.DeliveryID, Event: d.Event, Action: d.Action, RepoID: d.RepoID, OwnerID: d.OwnerID,
		Body: slices.Clone(p.Body), Fields: slices.Clone(p.Fields), ReceivedAt: s.rawReceivedAt,
	})
	for _, o := range p.Deliveries {
		s.rawOutbox[fakeRawOutboxKey{o.Key, id}] = &fakeRawOutbox{incarnation: o.Incarnation, enqueuedAt: now, nextAttempt: now}
	}
	for _, g := range p.Gaps {
		s.raiseRawGapLocked(fakeRawGapKey{g.Key, g.Incarnation.UnixNano()}, id)
	}
	for _, leaseID := range p.Unread {
		if l := s.rawLeases[leaseID]; l != nil {
			l.unread[id] = struct{}{}
		}
	}
	for _, leaseID := range p.Lost {
		if l := s.rawLeases[leaseID]; l != nil {
			l.lost = id
		}
	}
	return faucet.RawIngest{Matched: true, ID: id}, nil
}

func (s *fakeStore) RawHead(ctx context.Context) (int64, error) {
	err := s.beginRaw(ctx)
	defer s.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return s.rawHeadLocked(), nil
}

func (s *fakeStore) UpsertRawSubscription(ctx context.Context, sub faucet.Subscription, now time.Time, limits faucet.RawLimits) (faucet.Subscription, error) {
	err := s.beginRaw(ctx)
	defer s.mu.Unlock()
	if err == nil {
		err = limits.Validate()
	}
	if err != nil {
		return faucet.Subscription{}, err
	}
	event, ok := strings.CutPrefix(sub.EventType, fakeRawPrefix)
	if !ok {
		return faucet.Subscription{}, xerrors.Errorf("%q is not a raw event name", sub.EventType)
	}
	return s.upsertSubscriptionLocked(sub, now, limits.Live, func() error {
		count := s.rawTargetLocked(sub.Principal, event, sub.Target, now, sub.KeyHash, nil)
		switch {
		case count.ownSubscriptions >= limits.PerPrincipalTarget:
			return faucet.ErrRawShareLimit
		case count.interests >= limits.PerTarget:
			return faucet.ErrRawInterestLimit
		}
		return nil
	})
}

type fakeRawTarget struct {
	interests, ownSubscriptions int
	// ownOldest is the principal's live lease of the target polled longest ago.
	ownOldest *fakeRawLease
	ownLeases int
}

// rawTargetLocked counts the live raw subscriptions of event and target under
// another key than except, and their live leases but ended.
func (s *fakeStore) rawTargetLocked(principal, event string, target faucet.Target, now time.Time, except faucet.SubscriptionKeyHash, ended map[faucet.RawLeaseID]bool) fakeRawTarget {
	var t fakeRawTarget
	for key, sub := range s.subs {
		if key != except && sub.EventType == fakeRawPrefix+event && sub.Target == target && sub.ExpiresAt.After(now) {
			t.interests++
			if sub.Principal == principal {
				t.ownSubscriptions++
			}
		}
	}
	for id, l := range s.rawLeases {
		if ended[id] || l.lease.Event != event || l.lease.Target != target || !l.lease.ExpiresAt.After(now) {
			continue
		}
		t.interests++
		if l.lease.Principal == principal {
			t.ownLeases++
			if t.ownOldest == nil || l.polledAt.Before(t.ownOldest.polledAt) || l.polledAt.Equal(t.ownOldest.polledAt) && bytes.Compare(id[:], t.ownOldest.lease.ID[:]) < 0 {
				t.ownOldest = l
			}
		}
	}
	return t
}

func (s *fakeStore) OpenRawLease(ctx context.Context, lease faucet.RawLease, now time.Time, limits faucet.RawLimits) (int64, error) {
	err := s.beginRaw(ctx)
	defer s.mu.Unlock()
	if err == nil {
		err = limits.Validate()
	}
	if err != nil {
		return 0, err
	}
	if err := lease.Validate(now); err != nil {
		return 0, err
	}
	if _, ok := s.rawLeases[lease.ID]; ok {
		return 0, xerrors.New("lease ID is already stored")
	}
	var (
		live   int
		oldest *fakeRawLease
		ended  = map[faucet.RawLeaseID]bool{}
	)
	for id, l := range s.rawLeases {
		switch {
		case l.lease.Principal != lease.Principal:
		case !l.lease.ExpiresAt.After(now):
			ended[id] = true
		default:
			live++
			if oldest == nil || l.polledAt.Before(oldest.polledAt) || l.polledAt.Equal(oldest.polledAt) && bytes.Compare(id[:], oldest.lease.ID[:]) < 0 {
				oldest = l
			}
		}
	}
	if live >= limits.Live {
		ended[oldest.lease.ID] = true
	}
	target := s.rawTargetLocked(lease.Principal, lease.Event, lease.Target, now, faucet.SubscriptionKeyHash{}, ended)
	if target.ownLeases >= limits.PerPrincipalTarget {
		ended[target.ownOldest.lease.ID] = true
		target.interests--
	}
	if target.interests >= limits.PerTarget {
		return 0, faucet.ErrRawInterestLimit
	}
	for id := range ended {
		delete(s.rawLeases, id)
	}
	lease.Actions, lease.Fields, lease.RepoNames = slices.Clone(lease.Actions), slices.Clone(lease.Fields), slices.Clone(lease.RepoNames)
	head := s.rawHeadLocked()
	s.rawLeases[lease.ID] = &fakeRawLease{lease: lease, acked: head, polledAt: now, unread: map[int64]struct{}{}}
	return head, nil
}

func (s *fakeStore) ReadRawLease(ctx context.Context, q faucet.RawRead) (faucet.RawPage, bool, error) {
	err := s.beginRaw(ctx)
	defer s.mu.Unlock()
	if err == nil {
		err = q.Validate()
	}
	if err != nil {
		return faucet.RawPage{}, false, err
	}
	l := s.rawLeases[q.Lease]
	if l == nil || l.lease.Key != q.Key || l.lease.Target != q.Target || !l.lease.ExpiresAt.After(q.Now) {
		return faucet.RawPage{}, false, nil
	}
	page := faucet.RawPage{AckedThrough: l.acked}
	l.acked = max(l.acked, q.After)
	l.polledAt, l.lease.ExpiresAt = q.Now, q.ExpiresAt
	if l.lost <= q.After {
		l.lost = 0
	}
	rows := make([]int64, 0, len(l.unread)+1)
	for id := range l.unread {
		if id <= q.After {
			delete(l.unread, id)
			continue
		}
		rows = append(rows, id)
	}
	if l.lost != 0 {
		rows = append(rows, l.lost)
	}
	slices.Sort(rows)
	last := q.After
	var size int64
	for i, id := range rows {
		last = id
		ev, ok := s.rawEventLocked(id)
		if id == l.lost || !ok || ev.Body == nil || ev.ReceivedAt.Before(q.NotBefore) {
			page.Skipped++
			continue
		}
		page.Events = append(page.Events, ev)
		size += int64(len(ev.Body))
		if len(page.Events) == int(q.Limit) || size >= q.MaxBytes {
			page.More = i < len(rows)-1
			break
		}
	}
	page.Next = last
	if !page.More {
		page.Next = max(last, s.rawHeadLocked())
	}
	return page, true, nil
}

func (s *fakeStore) ClaimRawDelivery(ctx context.Context, now time.Time, lease time.Duration, skip []string) (faucet.RawDeliveryClaim, bool, error) {
	err := s.beginRaw(ctx)
	defer s.mu.Unlock()
	switch {
	case err != nil:
		return faucet.RawDeliveryClaim{}, false, err
	case lease <= 0:
		return faucet.RawDeliveryClaim{}, false, xerrors.New("lease must be positive")
	}
	var (
		best    fakeRawOutboxKey
		bestRow *fakeRawOutbox
	)
	for k, d := range s.rawOutbox {
		sub, live := s.rawSubscriptionLiveLocked(k.key, d.incarnation, now)
		if !live || d.nextAttempt.After(now) || d.leaseUntil.After(now) || slices.Contains(skip, faucet.CallbackOrigin(sub.CallbackURL)) {
			continue
		}
		if bestRow == nil || cmp.Or(d.nextAttempt.Compare(bestRow.nextAttempt), cmp.Compare(k.rawID, best.rawID), bytes.Compare(k.key[:], best.key[:])) < 0 {
			best, bestRow = k, d
		}
	}
	if bestRow == nil {
		return faucet.RawDeliveryClaim{}, false, nil
	}
	bestRow.attempts++
	bestRow.leaseUntil = now.Add(lease)
	sub := s.subs[best.key]
	watermark := best.rawID
	for k, d := range s.rawOutbox {
		if k.key == best.key && d.incarnation.Equal(bestRow.incarnation) {
			watermark = min(watermark, k.rawID)
		}
	}
	claim := faucet.RawDeliveryClaim{
		Ref:                 faucet.DeliveryRef{Key: best.key, EventID: best.rawID, Attempt: bestRow.attempts, Incarnation: sub.CreatedAt},
		EnqueuedAt:          bestRow.enqueuedAt,
		EventType:           sub.EventType,
		Arguments:           slices.Clone(sub.Arguments),
		CallbackURL:         sub.CallbackURL,
		SecretEnc:           slices.Clone(sub.SecretEnc),
		PreviousSecretEnc:   slices.Clone(sub.PreviousSecretEnc),
		PreviousSecretUntil: sub.PreviousSecretUntil,
		Target:              sub.Target,
		Watermark:           watermark - 1,
	}
	if ev, ok := s.rawEventLocked(best.rawID); ok && ev.Body != nil {
		claim.Event = &ev
	}
	return claim, true, nil
}

func (s *fakeStore) FinishRawDelivery(ctx context.Context, ref faucet.DeliveryRef, now time.Time, f faucet.Finish) (faucet.FinishResult, error) {
	err := s.beginRaw(ctx)
	defer s.mu.Unlock()
	switch {
	case err != nil:
		return faucet.FinishResult{}, err
	case ref.Attempt < 1:
		return faucet.FinishResult{}, xerrors.New("attempt must be at least 1")
	}
	if err := f.Validate(); err != nil {
		return faucet.FinishResult{}, err
	}
	sub, ok := s.subs[ref.Key]
	k := fakeRawOutboxKey{ref.Key, ref.EventID}
	d := s.rawOutbox[k]
	if !ok || d == nil || d.attempts != ref.Attempt || !sub.CreatedAt.Equal(ref.Incarnation) || !d.incarnation.Equal(ref.Incarnation) {
		return faucet.FinishResult{}, nil
	}
	result := faucet.FinishResult{Accepted: true}
	if f.Kind == faucet.FinishRetry {
		d.nextAttempt, d.leaseUntil = f.RetryAt, time.Time{}
	} else {
		delete(s.rawOutbox, k)
		result.Removed = 1
	}
	if f.Gap {
		s.raiseRawGapLocked(fakeRawGapKey{ref.Key, ref.Incarnation.UnixNano()}, ref.EventID)
	}
	switch {
	case f.Kind == faucet.FinishDelivered:
		sub.Status = faucet.DeliveryStatus{LastDeliveryAt: now}
	case f.Failure != "":
		sub.Status.LastError = f.Failure
		if sub.Status.FailedSince.IsZero() {
			sub.Status.FailedSince = now
		}
	case f.Kind == faucet.FinishDead && bytes.Equal(sub.SecretEnc, f.Unreadable):
		for other, row := range s.rawOutbox {
			if other.key == ref.Key && row.incarnation.Equal(ref.Incarnation) {
				delete(s.rawOutbox, other)
				result.Removed++
			}
		}
		s.dropSubscriptionLocked(ref.Key)
		return result, nil
	}
	s.subs[ref.Key] = sub
	return result, nil
}

func (s *fakeStore) ClaimRawGap(ctx context.Context, now time.Time, lease time.Duration, skip []string) (faucet.GapClaim, bool, error) {
	err := s.beginRaw(ctx)
	defer s.mu.Unlock()
	switch {
	case err != nil:
		return faucet.GapClaim{}, false, err
	case lease <= 0:
		return faucet.GapClaim{}, false, xerrors.New("lease must be positive")
	}
	var (
		best    fakeRawGapKey
		bestGap *fakeGap
	)
	for k, gap := range s.rawGaps {
		sub, live := s.rawSubscriptionLiveLocked(k.key, time.Unix(0, k.incarnation), now)
		if !live || gap.leaseUntil.After(now) || slices.Contains(skip, faucet.CallbackOrigin(sub.CallbackURL)) {
			continue
		}
		if bestGap == nil || bytes.Compare(k.key[:], best.key[:]) < 0 {
			best, bestGap = k, gap
		}
	}
	if bestGap == nil {
		return faucet.GapClaim{}, false, nil
	}
	sub := s.subs[best.key]
	bestGap.leaseUntil = now.Add(lease)
	return faucet.GapClaim{
		Ref:         faucet.GapRef{Key: best.key, Incarnation: sub.CreatedAt, Through: bestGap.through},
		CallbackURL: sub.CallbackURL, SecretEnc: slices.Clone(sub.SecretEnc),
		PreviousSecretEnc: slices.Clone(sub.PreviousSecretEnc), PreviousSecretUntil: sub.PreviousSecretUntil,
	}, true, nil
}

func (s *fakeStore) FinishRawGap(ctx context.Context, ref faucet.GapRef) error {
	err := s.beginRaw(ctx)
	defer s.mu.Unlock()
	if err != nil {
		return err
	}
	k := fakeRawGapKey{ref.Key, ref.Incarnation.UnixNano()}
	switch gap := s.rawGaps[k]; {
	case gap == nil:
	case gap.through <= ref.Through:
		delete(s.rawGaps, k)
	default:
		gap.leaseUntil = time.Time{}
	}
	return nil
}

func (s *fakeStore) ExpireRawDeliveries(ctx context.Context, now time.Time) (int64, error) {
	err := s.beginRaw(ctx)
	defer s.mu.Unlock()
	if err != nil {
		return 0, err
	}
	var deleted int64
	for k, d := range s.rawOutbox {
		if _, live := s.rawSubscriptionLiveLocked(k.key, d.incarnation, now); !live && !d.leaseUntil.After(now) {
			delete(s.rawOutbox, k)
			deleted++
		}
	}
	for k, gap := range s.rawGaps {
		if _, live := s.rawSubscriptionLiveLocked(k.key, time.Unix(0, k.incarnation), now); !live && !gap.leaseUntil.After(now) {
			delete(s.rawGaps, k)
		}
	}
	return deleted, nil
}

func (s *fakeStore) raiseRawGapLocked(k fakeRawGapKey, through int64) {
	if s.rawGaps[k] == nil {
		s.rawGaps[k] = &fakeGap{}
	}
	s.rawGaps[k].through = max(s.rawGaps[k].through, through)
}

// owePurgedLocked owes a gap to each subscription whose delivery names the
// event, whose payload a purge takes.
func (s *fakeStore) owePurgedLocked(id int64) {
	for k, d := range s.rawOutbox {
		if k.rawID == id {
			s.raiseRawGapLocked(fakeRawGapKey{k.key, d.incarnation.UnixNano()}, id)
		}
	}
}

func (s *fakeStore) rawNamedLocked(id int64) bool {
	for k := range s.rawOutbox {
		if k.rawID == id {
			return true
		}
	}
	for _, l := range s.rawLeases {
		if _, ok := l.unread[id]; ok {
			return true
		}
	}
	return false
}

func (s *fakeStore) PurgeRawEvents(ctx context.Context, p faucet.RawPurge) (faucet.RawPurgeResult, error) {
	err := s.beginRaw(ctx)
	defer s.mu.Unlock()
	if err == nil {
		err = p.Validate()
	}
	if err != nil {
		return faucet.RawPurgeResult{}, err
	}
	var res faucet.RawPurgeResult
	for id, l := range s.rawLeases {
		if !l.lease.ExpiresAt.After(p.Now) {
			delete(s.rawLeases, id)
			res.ExpiredLeases++
		}
	}
	kept := s.rawEvents[:0:0]
	for _, ev := range s.rawEvents {
		held := ev.Body != nil
		if held && (ev.ReceivedAt.Before(p.ClearBefore) || !s.rawNamedLocked(ev.ID)) {
			ev.Body, ev.Fields = nil, nil
			res.Cleared++
		}
		deleted := ev.ReceivedAt.Before(p.DeleteBefore)
		if held && (ev.Body == nil || deleted) {
			s.owePurgedLocked(ev.ID)
		}
		if deleted {
			res.Deleted++
			continue
		}
		kept = append(kept, ev)
	}
	s.rawEvents = kept
	return res, nil
}

func (s *fakeStore) RawWants(ctx context.Context, now time.Time) ([]faucet.RawWant, error) {
	err := s.beginRaw(ctx)
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var wants []faucet.RawWant
	add := func(target faucet.Target, event string, actions []string) {
		actions = slices.Clone(actions)
		slices.Sort(actions)
		wants = append(wants, faucet.RawWant{Target: target, Event: event, Actions: slices.Compact(actions)})
	}
	for _, sub := range s.subs {
		if event, raw := strings.CutPrefix(sub.EventType, fakeRawPrefix); raw && sub.ExpiresAt.After(now) {
			add(sub.Target, event, fakeRawArgumentsOf(sub).Actions)
		}
	}
	for _, l := range s.rawLeases {
		if l.lease.ExpiresAt.After(now) {
			add(l.lease.Target, l.lease.Event, l.lease.Actions)
		}
	}
	order := func(a, b faucet.RawWant) int {
		return cmp.Or(cmp.Compare(a.Target.Kind(), b.Target.Kind()), cmp.Compare(a.Target.RepoID(), b.Target.RepoID()), cmp.Compare(a.Target.OwnerID(), b.Target.OwnerID()), cmp.Compare(a.Event, b.Event), slices.Compare(a.Actions, b.Actions))
	}
	slices.SortFunc(wants, order)
	return slices.CompactFunc(wants, func(a, b faucet.RawWant) bool { return order(a, b) == 0 }), nil
}

func TestFakeStoreMatchesRawStoreContract(t *testing.T) {
	t.Parallel()
	storetest.RunRaw(t, func(*testing.T) storetest.RawStore { return &fakeStore{} })
}
