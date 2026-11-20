package storetest

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"github.com/coder/superblink-events/faucet"
)

// rawLimitSubtests are the contract cases of RawLimits.PerTarget and
// RawLimits.PerPrincipalTarget.
var rawLimitSubtests = []rawSubtest{
	{"OpenRawLeaseRefusesALeaseOfAFullEventOfARepository", openRawLeaseRefusesALeaseOfAFullEventOfARepository},
	{"UpsertRawSubscriptionRefusesANewOneOfAFullEventAndRenewsAnOldOne", upsertRawSubscriptionRefusesANewOneOfAFullEventAndRenewsAnOldOne},
	{"UpsertRawSubscriptionRefusesOneMoreOfThePrincipalsShare", upsertRawSubscriptionRefusesOneMoreOfThePrincipalsShare},
	{"OpenRawLeaseEndsThePrincipalsOldestLeaseOfTheEventAtItsShare", openRawLeaseEndsThePrincipalsOldestLeaseOfTheEventAtItsShare},
	{"RawInterestsKeepTheLimitOfAnEventUnderConcurrentCalls", rawInterestsKeepTheLimitOfAnEventUnderConcurrentCalls},
	{"TheLimitsOfAnEventCountTheInterestsOfEachTargetApart", theLimitsOfAnEventCountTheInterestsOfEachTargetApart},
}

func targetLimits(perTarget, perPrincipal int) faucet.RawLimits {
	return faucet.RawLimits{Live: rawMaxLive, PerTarget: perTarget, PerPrincipalTarget: perPrincipal}
}

func openRawLeaseRefusesALeaseOfAFullEventOfARepository(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	limits := targetLimits(3, 3)
	r.subscribe(1, rawSpec{principal: "alice"})
	for _, principal := range []string{"bob", "carol"} {
		_, err := s.OpenRawLease(r.ctx, newRawLease(r.leases.Add(1), rawSpec{principal: principal}, r.now.Add(time.Hour)), r.now, limits)
		require.NoError(t, err)
	}
	full := newRawLease(r.leases.Add(1), rawSpec{principal: "dave"}, r.now.Add(time.Hour))
	_, err := s.OpenRawLease(r.ctx, full, r.now, limits)
	require.ErrorIs(t, err, faucet.ErrRawInterestLimit)
	_, found, err := s.ReadRawLease(r.ctx, r.query(full, 0))
	require.NoError(t, err)
	require.False(t, found, "a refused lease is not stored")
	require.Len(t, r.interestsAt(r.newDelivery(), r.now), 3)

	for name, spec := range map[string]rawSpec{
		"another event":      {principal: "dave", event: "fork"},
		"another repository": {principal: "dave", repoID: rawRepo + 1},
	} {
		_, err := s.OpenRawLease(r.ctx, newRawLease(r.leases.Add(1), spec, r.now.Add(time.Hour)), r.now, limits)
		require.NoError(t, err, name)
	}
	later := r.now.Add(2 * time.Hour)
	_, err = s.OpenRawLease(r.ctx, newRawLease(r.leases.Add(1), rawSpec{principal: "dave"}, later.Add(time.Hour)), later, limits)
	require.NoError(t, err, "interests that expired leave room")
}

func upsertRawSubscriptionRefusesANewOneOfAFullEventAndRenewsAnOldOne(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	limits := targetLimits(2, 2)
	r.open(rawSpec{principal: "alice"})
	held := r.subscribe(1, rawSpec{principal: "bob"})

	_, err := s.UpsertRawSubscription(r.ctx, newRawSubscription(2, rawSpec{principal: "carol"}, r.now.Add(time.Hour)), r.now, limits)
	require.ErrorIs(t, err, faucet.ErrRawInterestLimit)
	renewed, err := s.UpsertRawSubscription(r.ctx, newRawSubscription(1, rawSpec{principal: "bob"}, r.now.Add(2*time.Hour)), r.now, limits)
	require.NoError(t, err, "a renewal adds no interest")
	require.True(t, renewed.CreatedAt.Equal(held.CreatedAt))
	require.Len(t, r.interestsAt(r.newDelivery(), r.now), 2, "the refused subscription is not stored")

	_, err = s.UpsertRawSubscription(r.ctx, newRawSubscription(3, rawSpec{principal: "carol", event: "fork"}, r.now.Add(time.Hour)), r.now, limits)
	require.NoError(t, err, "another event has room")
	_, err = s.UpsertRawSubscription(r.ctx, newRawSubscription(4, rawSpec{}, r.now.Add(time.Hour)), r.now, faucet.RawLimits{Live: rawMaxLive, PerTarget: 2})
	require.Error(t, err, "every limit must be positive")
}

func upsertRawSubscriptionRefusesOneMoreOfThePrincipalsShare(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	limits := targetLimits(10, 2)
	for n := range 2 {
		_, err := s.UpsertRawSubscription(r.ctx, newRawSubscription(n, rawSpec{principal: "alice"}, r.now.Add(time.Hour)), r.now, limits)
		require.NoError(t, err)
	}
	_, err := s.UpsertRawSubscription(r.ctx, newRawSubscription(2, rawSpec{principal: "alice"}, r.now.Add(time.Hour)), r.now, limits)
	require.ErrorIs(t, err, faucet.ErrRawShareLimit)
	_, err = s.UpsertRawSubscription(r.ctx, newRawSubscription(0, rawSpec{principal: "alice"}, r.now.Add(2*time.Hour)), r.now, limits)
	require.NoError(t, err, "a renewal is in the share")
	for name, spec := range map[string]rawSpec{
		"another principal": {principal: "bob"},
		"another event":     {principal: "alice", event: "fork"},
	} {
		_, err := s.UpsertRawSubscription(r.ctx, newRawSubscription(3, spec, r.now.Add(time.Hour)), r.now, limits)
		require.NoError(t, err, name)
	}
	_, err = s.OpenRawLease(r.ctx, newRawLease(r.leases.Add(1), rawSpec{principal: "alice"}, r.now.Add(time.Hour)), r.now, limits)
	require.NoError(t, err, "the share of leases is apart from the share of subscriptions")
}

func openRawLeaseEndsThePrincipalsOldestLeaseOfTheEventAtItsShare(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	limits := targetLimits(10, 2)
	open := func(spec rawSpec, at time.Time) faucet.RawLease {
		lease := newRawLease(r.leases.Add(1), spec, at.Add(time.Hour))
		_, err := s.OpenRawLease(r.ctx, lease, at, limits)
		require.NoError(t, err)
		return lease
	}
	oldest := open(rawSpec{principal: "alice"}, r.now)
	other := open(rawSpec{principal: "alice", event: "fork"}, r.now)
	second := open(rawSpec{principal: "alice"}, r.now.Add(time.Second))
	bobs := open(rawSpec{principal: "bob"}, r.now.Add(time.Second))
	third := open(rawSpec{principal: "alice"}, r.now.Add(2*time.Second))

	r.now = r.now.Add(3 * time.Second)
	_, found, err := s.ReadRawLease(r.ctx, r.query(oldest, 0))
	require.NoError(t, err)
	require.False(t, found, "the principal's lease of the event polled longest ago ended")
	for _, kept := range []faucet.RawLease{other, second, bobs, third} {
		r.read(r.query(kept, 0))
	}
}

func rawInterestsKeepTheLimitOfAnEventUnderConcurrentCalls(t *testing.T, s RawStore) {
	const (
		callers   = 12
		perTarget = 5
	)
	r := newRawRig(t, s)
	limits := targetLimits(perTarget, perTarget)
	var refused atomic.Int64
	var fns []func() error
	for n := range callers {
		principal := fmt.Sprintf("caller-%d", n)
		fns = append(fns, func() error {
			var err error
			if n%2 == 0 {
				_, err = s.OpenRawLease(r.ctx, newRawLease(r.leases.Add(1), rawSpec{principal: principal}, r.now.Add(time.Hour)), r.now, limits)
			} else {
				_, err = s.UpsertRawSubscription(r.ctx, newRawSubscription(n, rawSpec{principal: principal}, r.now.Add(time.Hour)), r.now, limits)
			}
			if xerrors.Is(err, faucet.ErrRawInterestLimit) {
				refused.Add(1)
				return nil
			}
			return err
		})
	}
	require.NoError(t, concurrentRun(fns...))
	require.Len(t, r.interestsAt(r.newDelivery(), r.now), perTarget, "concurrent calls fill the event to its limit and no further")
	require.EqualValues(t, callers-perTarget, refused.Load())
}

// An owner, all or repository target of one event has limits of its own, so
// the interests of one owner never fill those of another.
func theLimitsOfAnEventCountTheInterestsOfEachTargetApart(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	limits := targetLimits(1, 1)
	targets := map[string]faucet.Target{
		"an owner":       faucet.OwnerTarget(rawOwner),
		"another owner":  faucet.OwnerTarget(rawOwner + 1),
		"all":            faucet.AllTarget(),
		"the repository": faucet.RepoTarget(rawRepo),
	}
	for name, target := range targets {
		_, err := s.OpenRawLease(r.ctx, newRawLease(r.leases.Add(1), rawSpec{principal: "alice", target: target}, r.now.Add(time.Hour)), r.now, limits)
		require.NoError(t, err, name)
	}
	n := 0
	for name, target := range targets {
		n++
		_, err := s.UpsertRawSubscription(r.ctx, newRawSubscription(n, rawSpec{principal: "bob", target: target}, r.now.Add(time.Hour)), r.now, limits)
		require.ErrorIs(t, err, faucet.ErrRawInterestLimit, name)
	}
}
