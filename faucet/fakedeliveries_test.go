package faucet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/superblink-events/faucet"
)

type fakeDeliveryKey struct {
	key     faucet.SubscriptionKeyHash
	eventID int64
}

type fakeGap struct {
	through    int64
	leaseUntil time.Time
}

type fakeDelivery struct {
	messageID   string
	enqueuedAt  time.Time
	nextAttempt time.Time
	attempts    int32
	leaseUntil  time.Time
}

func (s *fakeStore) initSubscriptionsLocked() {
	if s.subs == nil {
		s.subs = map[faucet.SubscriptionKeyHash]faucet.Subscription{}
		s.scanned = map[faucet.SubscriptionKeyHash]int64{}
		s.deliveries = map[fakeDeliveryKey]*fakeDelivery{}
		s.gaps = map[faucet.SubscriptionKeyHash]*fakeGap{}
	}
}

func (s *fakeStore) dropSubscriptionLocked(key faucet.SubscriptionKeyHash) {
	delete(s.subs, key)
	delete(s.scanned, key)
	delete(s.gaps, key)
	for k := range s.deliveries {
		if k.key == key {
			delete(s.deliveries, k)
		}
	}
}

func (s *fakeStore) pendingLocked(key faucet.SubscriptionKeyHash) int32 {
	var n int32
	for k := range s.deliveries {
		if k.key == key {
			n++
		}
	}
	return n
}

func (s *fakeStore) ScanTargets(ctx context.Context, now time.Time, maxPending, limit int32, skipPrincipals ...string) ([]faucet.ScanTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.err != nil {
		return nil, s.err
	}
	if maxPending <= 0 || limit <= 0 {
		return nil, xerrors.New("max pending and limit must be positive")
	}
	head := int64(s.headLocked(faucet.AllTarget()))
	var targets []faucet.ScanTarget
	for key, sub := range s.subs {
		pending := s.pendingLocked(key)
		if !sub.ExpiresAt.After(now) || s.scanned[key] >= head || pending >= maxPending || slices.Contains(skipPrincipals, sub.Principal) ||
			!s.hasSomethingToReadLocked(key, sub) {
			continue
		}
		targets = append(targets, faucet.ScanTarget{
			KeyHash: key, Principal: sub.Principal, EventTypes: slices.Clone(sub.EventTypes), Arguments: slices.Clone(sub.Arguments), Target: sub.Target,
			ScannedThrough: s.scanned[key], Pending: pending,
		})
	}
	slices.SortFunc(targets, func(a, b faucet.ScanTarget) int {
		if a.ScannedThrough != b.ScannedThrough {
			return int(a.ScannedThrough - b.ScannedThrough)
		}
		return bytes.Compare(a.KeyHash[:], b.KeyHash[:])
	})
	return targets[:min(len(targets), int(limit))], nil
}

func (s *fakeStore) EnqueueDeliveries(ctx context.Context, now time.Time, e faucet.Enqueue) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if s.err != nil {
		return false, s.err
	}
	if err := e.Validate(); err != nil {
		return false, err
	}
	sub, ok := s.subs[e.Key]
	if !ok || !sub.ExpiresAt.After(now) || s.scanned[e.Key] != e.ExpectedScannedThrough {
		return false, nil
	}
	s.scanned[e.Key] = e.NewScannedThrough
	if e.GapThrough > 0 {
		if s.gaps[e.Key] == nil {
			s.gaps[e.Key] = &fakeGap{}
		}
		s.gaps[e.Key].through = max(s.gaps[e.Key].through, e.GapThrough)
	}
	for _, d := range e.Deliveries {
		k := fakeDeliveryKey{e.Key, d.EventID}
		if _, dup := s.deliveries[k]; dup {
			return false, xerrors.New("delivery is already queued")
		}
		s.deliveries[k] = &fakeDelivery{messageID: d.MessageID, enqueuedAt: now, nextAttempt: now}
	}
	return true, nil
}

func (s *fakeStore) ClaimDelivery(ctx context.Context, now time.Time, lease time.Duration, skip []string) (faucet.DeliveryClaim, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return faucet.DeliveryClaim{}, false, err
	}
	if s.err != nil {
		return faucet.DeliveryClaim{}, false, s.err
	}
	if lease <= 0 {
		return faucet.DeliveryClaim{}, false, xerrors.New("lease must be positive")
	}
	var (
		best    fakeDeliveryKey
		bestRow *fakeDelivery
	)
	for k, d := range s.deliveries {
		sub, ok := s.subs[k.key]
		due := ok && sub.ExpiresAt.After(now) && !d.nextAttempt.After(now) && (d.leaseUntil.IsZero() || !d.leaseUntil.After(now)) &&
			!slices.Contains(skip, faucet.CallbackOrigin(sub.CallbackURL))
		if !due {
			continue
		}
		if bestRow == nil || d.nextAttempt.Before(bestRow.nextAttempt) || d.nextAttempt.Equal(bestRow.nextAttempt) && k.eventID < best.eventID {
			best, bestRow = k, d
		}
	}
	if bestRow == nil {
		return faucet.DeliveryClaim{}, false, nil
	}
	bestRow.attempts++
	bestRow.leaseUntil = now.Add(lease)
	sub := s.subs[best.key]
	// The lowest unsent event of the target now, or one that a purge
	// deleted, which may have been of it.
	watermark, lowest := s.purgedThroughLocked(sub.Target), int64(math.MaxInt64)
	for k := range s.deliveries {
		if k.key != best.key || k.eventID > lowest {
			continue
		}
		i := slices.IndexFunc(s.events, func(e faucet.Event) bool { return e.ID == k.eventID })
		switch {
		case i < 0:
			watermark, lowest = s.purgedThroughLocked(sub.Target), k.eventID
		case sub.Target.Covers(s.events[i].RepoID, s.events[i].OwnerID):
			watermark, lowest = sub.Target.PositionOf(s.events[i]), k.eventID
		}
	}
	claim := faucet.DeliveryClaim{
		Ref:                 faucet.DeliveryRef{Key: best.key, EventID: best.eventID, MessageID: bestRow.messageID, Attempt: bestRow.attempts, Incarnation: sub.CreatedAt},
		MessageID:           bestRow.messageID,
		EnqueuedAt:          bestRow.enqueuedAt,
		EventType:           sub.EventType,
		CallbackURL:         sub.CallbackURL,
		SecretEnc:           slices.Clone(sub.SecretEnc),
		PreviousSecretEnc:   slices.Clone(sub.PreviousSecretEnc),
		PreviousSecretUntil: sub.PreviousSecretUntil,
		Target:              sub.Target,
		Watermark:           max(watermark-1, 0),
	}
	for _, e := range s.events {
		if e.ID == best.eventID {
			stored := e
			claim.Event = &stored
		}
	}
	return claim, true, nil
}

func (s *fakeStore) ClaimGap(ctx context.Context, now time.Time, lease time.Duration, skip []string) (faucet.GapClaim, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return faucet.GapClaim{}, false, err
	}
	if s.err != nil {
		return faucet.GapClaim{}, false, s.err
	}
	var best faucet.SubscriptionKeyHash
	for key, gap := range s.gaps {
		sub := s.subs[key]
		due := gap.through > 0 && sub.ExpiresAt.After(now) && !gap.leaseUntil.After(now) && !slices.Contains(skip, faucet.CallbackOrigin(sub.CallbackURL))
		if due && (best == faucet.SubscriptionKeyHash{} || bytes.Compare(key[:], best[:]) < 0) {
			best = key
		}
	}
	if best == (faucet.SubscriptionKeyHash{}) {
		return faucet.GapClaim{}, false, nil
	}
	sub, gap := s.subs[best], s.gaps[best]
	gap.leaseUntil = now.Add(lease)
	return faucet.GapClaim{
		Ref:         faucet.GapRef{Key: best, Incarnation: sub.CreatedAt, Through: gap.through},
		Cursor:      s.purgedThroughLocked(sub.Target),
		CallbackURL: sub.CallbackURL, SecretEnc: slices.Clone(sub.SecretEnc),
		PreviousSecretEnc: slices.Clone(sub.PreviousSecretEnc), PreviousSecretUntil: sub.PreviousSecretUntil,
	}, true, nil
}

func (s *fakeStore) FinishGap(_ context.Context, ref faucet.GapRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	sub, gap := s.subs[ref.Key], s.gaps[ref.Key]
	if gap == nil || !sub.CreatedAt.Equal(ref.Incarnation) {
		return nil
	}
	if gap.through <= ref.Through {
		gap.through = 0
	}
	gap.leaseUntil = time.Time{}
	return nil
}

func (s *fakeStore) FinishDelivery(_ context.Context, ref faucet.DeliveryRef, now time.Time, f faucet.Finish) (faucet.FinishResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return faucet.FinishResult{}, s.err
	}
	if ref.Attempt < 1 {
		return faucet.FinishResult{}, xerrors.New("attempt must be at least 1")
	}
	if err := f.Validate(); err != nil {
		return faucet.FinishResult{}, err
	}
	if f.Gap {
		return faucet.FinishResult{}, xerrors.New("a typed delivery owes no gap of its own")
	}
	sub, ok := s.subs[ref.Key]
	k := fakeDeliveryKey{ref.Key, ref.EventID}
	d := s.deliveries[k]
	if !ok || d == nil || d.attempts != ref.Attempt || d.messageID != ref.MessageID || !sub.CreatedAt.Equal(ref.Incarnation) {
		return faucet.FinishResult{}, nil
	}
	result := faucet.FinishResult{Accepted: true}
	if f.Kind == faucet.FinishRetry {
		d.nextAttempt, d.leaseUntil = f.RetryAt, time.Time{}
	} else {
		delete(s.deliveries, k)
		result.Removed = 1
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
		result.Removed += int64(s.pendingLocked(ref.Key))
		s.dropSubscriptionLocked(ref.Key)
		return result, nil
	}
	s.subs[ref.Key] = sub
	return result, nil
}

func (s *fakeStore) ExpireDeliveries(ctx context.Context, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if s.err != nil {
		return 0, s.err
	}
	var deleted int64
	for k, d := range s.deliveries {
		sub, ok := s.subs[k.key]
		if ok && !sub.ExpiresAt.After(now) && (d.leaseUntil.IsZero() || !d.leaseUntil.After(now)) {
			delete(s.deliveries, k)
			deleted++
		}
	}
	return deleted, nil
}

func (s *fakeStore) deliveryRows() map[string]int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := map[string]int32{}
	for _, d := range s.deliveries {
		rows[d.messageID] = d.attempts
	}
	return rows
}

// tamperSecret flips a byte of the sealed secret, so that it no longer opens.
func (s *fakeStore) tamperSecret(key faucet.SubscriptionKeyHash) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub := s.subs[key]
	sub.SecretEnc = slices.Clone(sub.SecretEnc)
	sub.SecretEnc[len(sub.SecretEnc)-1] ^= 1
	s.subs[key] = sub
}

func (s *fakeStore) hasSomethingToReadLocked(key faucet.SubscriptionKeyHash, sub faucet.Subscription) bool {
	if s.scanned[key] < s.purgeMark {
		return true
	}
	return slices.ContainsFunc(s.events, func(e faucet.Event) bool {
		return sub.Target.Covers(e.RepoID, e.OwnerID) && slices.Contains(sub.EventTypes, e.Type) && e.ID > s.scanned[key]
	})
}

// retarget gives the subscription with this handle another target, as a
// subscribe of an owner or of every repository stores.
func (s *fakeStore) retarget(id string, target faucet.Target) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, sub := range s.subs {
		if key.ID() == id {
			sub.Target = target
			s.subs[key] = sub
		}
	}
}

// A row sealed before rotation windows existed looks like this after its first
// refresh with the same secret.
func (s *fakeStore) repeatCurrentSecretAsPrevious(key faucet.SubscriptionKeyHash, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub := s.subs[key]
	sub.PreviousSecretEnc, sub.PreviousSecretUntil = slices.Clone(sub.SecretEnc), until
	s.subs[key] = sub
}

// A row written by another version of faucet can hold such arguments.
func (s *fakeStore) corruptArguments(key faucet.SubscriptionKeyHash) {
	s.setArguments(key, json.RawMessage(`{"repo":"octo/repo","pr":"not a number"}`))
}

// appendRunEvents stores n run events of the test repository at once. It
// skips the delivery ID check of InsertEvent, which reads every event, so a
// test can store a long history quickly.
func (s *fakeStore) appendRunEvents(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for range n {
		s.appendLocked(faucet.Event{
			Source: "github", DeliveryID: fmt.Sprintf("history-%d", s.lastID+1), Type: "ci.run_completed",
			RepoID: repoID, Data: json.RawMessage(sampleData["ci.run_completed"]),
		})
	}
}

func (s *fakeStore) setArguments(key faucet.SubscriptionKeyHash, arguments json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub := s.subs[key]
	sub.Arguments = arguments
	s.subs[key] = sub
}
