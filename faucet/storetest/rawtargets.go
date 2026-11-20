package storetest

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
)

var rawTargetSubtests = []rawSubtest{
	{"IngestRawMatchesEachKindOfTarget", ingestRawMatchesEachKindOfTarget},
	{"IngestRawKeepsOnlyTheRepositoryNamesAnInterestAsked", ingestRawKeepsOnlyTheRepositoryNamesAnInterestAsked},
	{"ReadRawLeaseServesAnOwnerOrAllLease", readRawLeaseServesAnOwnerOrAllLease},
	{"ClaimRawDeliveryCarriesTheTargetAndADeliveryWithNoRepository", claimRawDeliveryCarriesTheTargetAndADeliveryWithNoRepository},
	{"RawWantsNamesTheTargetOfEachInterest", rawWantsNamesTheTargetOfEachInterest},
}

// deliveryOf returns a push of this repository, owner and full name. A
// repository ID of 0 is a delivery that names no repository, as an
// organization's does.
func (r *rawRig) deliveryOf(repoID, ownerID int64, repoName string) faucet.RawDelivery {
	d := r.newDelivery()
	d.RepoID, d.OwnerID, d.RepoName = repoID, ownerID, repoName
	return d
}

// rawInterestSet names each interest of a plan by the subscription's
// callback URL or the lease's number, for a comparison that ignores order.
func rawInterestSet(t *testing.T, interests []faucet.RawInterest, subs map[string]faucet.Subscription, leases map[string]faucet.RawLease) []string {
	t.Helper()
	var names []string
	for _, in := range interests {
		found := ""
		for name, sub := range subs {
			if !in.IsLease && in.Subscription == sub.KeyHash {
				found = name
			}
		}
		for name, lease := range leases {
			if in.IsLease && in.Lease == lease.ID {
				found = name
			}
		}
		require.NotEmpty(t, found, "the plan got an interest that the test did not make")
		names = append(names, found)
	}
	return names
}

func ingestRawMatchesEachKindOfTarget(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	subs := map[string]faucet.Subscription{
		"repo subscription":        r.subscribe(1, rawSpec{}),
		"owner subscription":       r.subscribe(2, rawSpec{target: faucet.OwnerTarget(rawOwner)}),
		"other owner subscription": r.subscribe(3, rawSpec{target: faucet.OwnerTarget(rawOwner + 1)}),
		"all subscription":         r.subscribe(4, rawSpec{target: faucet.AllTarget()}),
	}
	leases := map[string]faucet.RawLease{
		"repo lease":  r.open(rawSpec{}),
		"owner lease": r.open(rawSpec{target: faucet.OwnerTarget(rawOwner)}),
		"all lease":   r.open(rawSpec{target: faucet.AllTarget()}),
	}
	for _, tc := range []struct {
		name     string
		delivery faucet.RawDelivery
		want     []string
	}{
		{
			"a delivery of the repository", r.deliveryOf(rawRepo, rawOwner, "octo/repo"),
			[]string{"repo subscription", "owner subscription", "all subscription", "repo lease", "owner lease", "all lease"},
		},
		{
			"a delivery of another repository of the owner", r.deliveryOf(rawRepo+1, rawOwner, "octo/other"),
			[]string{"owner subscription", "all subscription", "owner lease", "all lease"},
		},
		{
			"a delivery of the owner that names no repository", r.deliveryOf(0, rawOwner, ""),
			[]string{"owner subscription", "all subscription", "owner lease", "all lease"},
		},
		{
			"a delivery of another owner", r.deliveryOf(rawRepo+2, rawOwner+1, "else/where"),
			[]string{"other owner subscription", "all subscription", "all lease"},
		},
		{
			"a delivery of an owner that nobody asked for", r.deliveryOf(0, rawOwner+9, ""),
			[]string{"all subscription", "all lease"},
		},
	} {
		got := rawInterestSet(t, r.interestsAt(tc.delivery, r.now), subs, leases)
		require.ElementsMatch(t, tc.want, got, "the interests of %s", tc.name)
	}
}

func ingestRawKeepsOnlyTheRepositoryNamesAnInterestAsked(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	subs := map[string]faucet.Subscription{
		"owner subscription of octo/other": r.subscribe(1, rawSpec{target: faucet.OwnerTarget(rawOwner), repos: []string{"octo/other"}}),
	}
	leases := map[string]faucet.RawLease{
		"all lease of octo/other and else/where": r.open(rawSpec{target: faucet.AllTarget(), repos: []string{"octo/other", "else/where"}}),
	}
	for _, tc := range []struct {
		name     string
		delivery faucet.RawDelivery
		want     []string
	}{
		{"a named repository, written in another case", r.deliveryOf(rawRepo+1, rawOwner, "Octo/Other"), []string{"owner subscription of octo/other", "all lease of octo/other and else/where"}},
		{"a repository of the owner that is not named", r.deliveryOf(rawRepo, rawOwner, "octo/repo"), nil},
		{"a delivery of the owner that names no repository", r.deliveryOf(0, rawOwner, ""), nil},
		{"a repository of another owner that the lease names", r.deliveryOf(rawRepo+2, rawOwner+1, "else/where"), []string{"all lease of octo/other and else/where"}},
	} {
		got := rawInterestSet(t, r.interestsAt(tc.delivery, r.now), subs, leases)
		require.ElementsMatch(t, tc.want, got, "the interests of %s", tc.name)
	}
}

func readRawLeaseServesAnOwnerOrAllLease(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	owner := r.open(rawSpec{target: faucet.OwnerTarget(rawOwner)})
	all := r.open(rawSpec{target: faucet.AllTarget()})
	ofOwner := r.store(r.deliveryOf(rawRepo+1, rawOwner, "octo/other"), serveAll(rawBody))
	org := r.store(r.deliveryOf(0, rawOwner, ""), serveAll(rawBody))
	other := r.store(r.deliveryOf(rawRepo+2, rawOwner+1, "else/where"), serveAll(rawBody))

	page := r.read(r.query(owner, 0))
	require.Equal(t, []int64{ofOwner, org}, rawIDs(page.Events), "the owner lease reads the events of its owner")
	require.Equal(t, rawOwner, page.Events[0].OwnerID)
	require.Equal(t, rawRepo+1, page.Events[0].RepoID)
	require.Zero(t, page.Events[1].RepoID, "an event that names no repository keeps repository 0")
	require.Equal(t, rawOwner, page.Events[1].OwnerID)
	require.Equal(t, []int64{ofOwner, org, other}, rawIDs(r.read(r.query(all, 0)).Events), "the all lease reads every event")

	for name, target := range map[string]faucet.Target{
		"a repository of the owner": faucet.RepoTarget(rawRepo + 1),
		"another owner":             faucet.OwnerTarget(rawOwner + 1),
		"every repository":          faucet.AllTarget(),
	} {
		q := r.query(owner, 0)
		q.Target = target
		_, found := r.leaseFind(q)
		require.False(t, found, "a read of the owner lease with %s finds no lease", name)
	}
}

func claimRawDeliveryCarriesTheTargetAndADeliveryWithNoRepository(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	sub := r.subscribe(1, rawSpec{target: faucet.OwnerTarget(rawOwner)})
	id := r.store(r.deliveryOf(0, rawOwner, ""), serveAll(rawBody))

	claim := r.mustClaim(r.now)
	require.Equal(t, sub.KeyHash, claim.Ref.Key)
	require.Equal(t, faucet.OwnerTarget(rawOwner), claim.Target, "the claim carries the subscription's target")
	require.NotNil(t, claim.Event)
	require.Equal(t, id, claim.Event.ID)
	require.Zero(t, claim.Event.RepoID)
	require.Equal(t, rawOwner, claim.Event.OwnerID, "and the event its owner, so the run can check that the target covers it")

	missed := r.store(r.deliveryOf(rawRepo, rawOwner, "octo/repo"), func(interests []faucet.RawInterest) faucet.RawPlan {
		plan := faucet.RawPlan{}
		for _, in := range interests {
			plan.Gaps = append(plan.Gaps, faucet.RawGap{Key: in.Subscription, Incarnation: in.Incarnation})
		}
		return plan
	})
	gap, ok := r.claimGap(r.now)
	require.True(t, ok, "an owner subscription that misses an event is owed a gap")
	require.Equal(t, sub.KeyHash, gap.Ref.Key)
	require.Equal(t, missed, gap.Ref.Through)
}

func rawWantsNamesTheTargetOfEachInterest(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	r.subscribe(1, rawSpec{target: faucet.OwnerTarget(rawOwner), actions: []string{"opened"}})
	r.open(rawSpec{target: faucet.AllTarget(), event: "project"})
	r.open(rawSpec{})

	require.ElementsMatch(t, []faucet.RawWant{
		{Target: faucet.OwnerTarget(rawOwner), Event: rawEvent, Actions: []string{"opened"}},
		{Target: faucet.AllTarget(), Event: "project"},
		{Target: faucet.RepoTarget(rawRepo), Event: rawEvent},
	}, r.purgeWantsAt(r.now))
}
