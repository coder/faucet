package faucet_test

import (
	"bytes"
	"context"
	"slices"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
)

type fakeDeliveryKey struct {
	key     faucet.SubscriptionKeyHash
	eventID int64
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
	}
}

func (s *fakeStore) dropSubscriptionLocked(key faucet.SubscriptionKeyHash) {
	delete(s.subs, key)
	delete(s.scanned, key)
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

func (s *fakeStore) headLocked() int64 {
	head := s.purgeMark
	for _, e := range s.events {
		head = max(head, e.ID)
	}
	return head
}

func (s *fakeStore) ScanTargets(ctx context.Context, now time.Time, maxPending, limit int32) ([]faucet.ScanTarget, error) {
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
	head := s.headLocked()
	var targets []faucet.ScanTarget
	for key, sub := range s.subs {
		pending := s.pendingLocked(key)
		if !sub.ExpiresAt.After(now) || s.scanned[key] >= head || pending >= maxPending || !s.hasSomethingToReadLocked(key, sub) {
			continue
		}
		targets = append(targets, faucet.ScanTarget{
			KeyHash: key, EventType: sub.EventType, Arguments: slices.Clone(sub.Arguments), RepoID: sub.RepoID,
			CallbackURL: sub.CallbackURL, SecretEnc: slices.Clone(sub.SecretEnc), PreviousSecretEnc: slices.Clone(sub.PreviousSecretEnc), PreviousSecretUntil: sub.PreviousSecretUntil,
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
	for _, d := range e.Deliveries {
		k := fakeDeliveryKey{e.Key, d.EventID}
		if _, dup := s.deliveries[k]; dup {
			return false, xerrors.New("delivery is already queued")
		}
		s.deliveries[k] = &fakeDelivery{messageID: d.MessageID, enqueuedAt: now, nextAttempt: now}
	}
	return true, nil
}

func (s *fakeStore) ClaimDelivery(ctx context.Context, now time.Time, lease time.Duration) (faucet.DeliveryClaim, bool, error) {
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
		due := ok && sub.ExpiresAt.After(now) && !d.nextAttempt.After(now) && (d.leaseUntil.IsZero() || !d.leaseUntil.After(now))
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
	watermark := best.eventID
	for k := range s.deliveries {
		if k.key == best.key {
			watermark = min(watermark, k.eventID)
		}
	}
	claim := faucet.DeliveryClaim{
		Ref:                 faucet.DeliveryRef{Key: best.key, EventID: best.eventID, MessageID: bestRow.messageID, Attempt: bestRow.attempts},
		MessageID:           bestRow.messageID,
		EnqueuedAt:          bestRow.enqueuedAt,
		CallbackURL:         sub.CallbackURL,
		SecretEnc:           slices.Clone(sub.SecretEnc),
		PreviousSecretEnc:   slices.Clone(sub.PreviousSecretEnc),
		PreviousSecretUntil: sub.PreviousSecretUntil,
		RepoID:              sub.RepoID,
		Watermark:           watermark - 1,
	}
	for _, e := range s.events {
		if e.ID == best.eventID {
			stored := e
			claim.Event = &stored
		}
	}
	return claim, true, nil
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
	sub, ok := s.subs[ref.Key]
	k := fakeDeliveryKey{ref.Key, ref.EventID}
	d := s.deliveries[k]
	if !ok || d == nil || d.attempts != ref.Attempt || d.messageID != ref.MessageID {
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
		return e.RepoID == sub.RepoID && e.Type == sub.EventType && e.ID > s.scanned[key]
	})
}
