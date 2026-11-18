package faucet

import (
	"context"
	"fmt"
	"strings"
)

const (
	// maxUnreadPerLease is the most unread events that one poll lease holds.
	// A lease at it misses the next events, and its next poll says truncated.
	maxUnreadPerLease = 1000
	// maxRawInterestsPerTarget is the most live subscriptions and poll leases
	// of one raw event of one repository. It bounds what an ingest of that
	// event plans and writes.
	maxRawInterestsPerTarget = 100
	// maxRawInterestsPerLogin is the most live poll leases, and the most live
	// subscriptions, of one raw event of one repository that one login holds,
	// so that no login takes all of maxRawInterestsPerTarget. A poll that
	// opens one more ends the login's lease of it polled longest ago.
	maxRawInterestsPerLogin = 10
)

// rawInterestLimitError refuses a new subscription or poll lease of a raw
// event of a repository that has maxRawInterestsPerTarget of them.
func rawInterestLimitError(entry *rawEntry, repo string) error {
	return rpcError(codeResourceExhausted,
		fmt.Sprintf("raw interest limit reached: %s of %s has %d live subscriptions and poll leases, the most that faucet keeps for one event of one repository",
			entry.name, repo, maxRawInterestsPerTarget),
		map[string]any{"limit": "raw_interests", "max": maxRawInterestsPerTarget})
}

// rawMisses counts the interests that miss an event, by why.
type rawMisses struct {
	leasesFull, leasesTooLarge               int
	subscriptionsFull, subscriptionsTooLarge int
}

// planRawIngest holds the whole policy of a raw ingest, so that a store only
// matches interests and writes what it returns. Each interest at its cap
// misses the event, each that would get more than maxEventBodyBytes cannot
// get it, and the others are served. A lease that misses the event keeps a
// mark, and a subscription is owed a gap. The stored body is the whole
// payload when a served interest wants it whole, and else only the union of
// the fields that the served interests want. Once ctx has ended it projects
// no more and returns an empty plan, which RawStore.IngestRaw does not write.
func planRawIngest(ctx context.Context, doc rawDocument, interests []RawInterest) (RawPlan, rawMisses) {
	var plan RawPlan
	var missed rawMisses
	var open, served []RawInterest
	for _, in := range interests {
		switch {
		case in.IsLease && in.Pending >= maxUnreadPerLease:
			plan.Lost = append(plan.Lost, in.Lease)
			missed.leasesFull++
		case !in.IsLease && in.Pending >= MaxPendingDeliveries:
			plan.Gaps = append(plan.Gaps, RawGap{Key: in.Subscription, Incarnation: in.Incarnation})
			missed.subscriptionsFull++
		default:
			open = append(open, in)
		}
	}
	fits := doc.fitsFor(ctx, open)
	for _, in := range open {
		switch fit := fits(in.Fields); {
		case in.IsLease && !fit:
			plan.Lost = append(plan.Lost, in.Lease)
			missed.leasesTooLarge++
		case !fit:
			plan.Gaps = append(plan.Gaps, RawGap{Key: in.Subscription, Incarnation: in.Incarnation})
			missed.subscriptionsTooLarge++
		case in.IsLease:
			plan.Unread = append(plan.Unread, in.Lease)
			served = append(served, in)
		default:
			plan.Deliveries = append(plan.Deliveries, RawOutbox{Key: in.Subscription, Incarnation: in.Incarnation})
			served = append(served, in)
		}
	}
	if ctx.Err() != nil {
		return RawPlan{}, rawMisses{}
	}
	plan.Body, plan.Fields = doc.bodyFor(served)
	return plan, missed
}

// fitsFor returns whether an interest of interests with these field paths
// gets data of at most maxEventBodyBytes. A projection is never larger than
// one of more paths, so when the union of the paths of interests fits, all
// of them do, and the payload is projected once. Otherwise each distinct set
// of paths is projected once.
func (d rawDocument) fitsFor(ctx context.Context, interests []RawInterest) func(fields []string) bool {
	whole := len(d.body) <= maxEventBodyBytes
	var union []string
	for _, in := range interests {
		union = append(union, in.Fields...)
	}
	if len(union) == 0 || ctx.Err() != nil {
		return func([]string) bool { return whole }
	}
	if _, fits := d.projectWithin(sortedSet(union), maxEventBodyBytes); fits {
		return func(fields []string) bool { return whole || len(fields) > 0 }
	}
	fit := map[string]bool{}
	return func(fields []string) bool {
		if len(fields) == 0 {
			return whole
		}
		key := strings.Join(sortedSet(fields), "\n")
		if _, known := fit[key]; !known && ctx.Err() == nil {
			_, fit[key] = d.projectWithin(fields, maxEventBodyBytes)
		}
		return fit[key]
	}
}

// bodyFor returns what is stored for the served interests: nothing when there
// are none, the whole payload when one of them wants it, and else the union
// of their field paths.
func (d rawDocument) bodyFor(served []RawInterest) (body []byte, fields []string) {
	if len(served) == 0 {
		return nil, nil
	}
	var union []string
	for _, in := range served {
		if len(in.Fields) == 0 {
			return d.body, nil
		}
		union = append(union, in.Fields...)
	}
	fields = sortedSet(union)
	return d.project(fields), fields
}
