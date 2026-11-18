package faucet

// maxUnreadPerLease is the most unread events that one poll lease holds. A
// lease at it misses the next events, and its next poll says truncated.
const maxUnreadPerLease = 1000

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
// the fields that the served interests want.
func planRawIngest(doc rawDocument, interests []RawInterest) (RawPlan, rawMisses) {
	var plan RawPlan
	var missed rawMisses
	var served []RawInterest
	for _, in := range interests {
		atCap := in.Pending >= MaxPendingDeliveries
		if in.IsLease {
			atCap = in.Pending >= maxUnreadPerLease
		}
		tooLarge := !atCap && rawTooLarge(doc, in.Fields)
		gap := RawGap{Key: in.Subscription, Incarnation: in.Incarnation}
		switch {
		case in.IsLease && atCap:
			plan.Lost = append(plan.Lost, in.Lease)
			missed.leasesFull++
		case in.IsLease && tooLarge:
			plan.Lost = append(plan.Lost, in.Lease)
			missed.leasesTooLarge++
		case in.IsLease:
			plan.Unread = append(plan.Unread, in.Lease)
			served = append(served, in)
		case atCap:
			plan.Gaps = append(plan.Gaps, gap)
			missed.subscriptionsFull++
		case tooLarge:
			plan.Gaps = append(plan.Gaps, gap)
			missed.subscriptionsTooLarge++
		default:
			plan.Deliveries = append(plan.Deliveries, RawOutbox{Key: in.Subscription, Incarnation: in.Incarnation})
			served = append(served, in)
		}
	}
	if len(served) == 0 {
		return plan, missed
	}
	var fields []string
	for _, in := range served {
		if len(in.Fields) == 0 {
			plan.Body = doc.body
			return plan, missed
		}
		fields = append(fields, in.Fields...)
	}
	plan.Fields = sortedSet(fields)
	plan.Body = doc.project(plan.Fields)
	return plan, missed
}

func rawTooLarge(doc rawDocument, fields []string) bool {
	if len(fields) == 0 {
		return len(doc.body) > maxEventBodyBytes
	}
	return len(doc.project(fields)) > maxEventBodyBytes
}
