package faucet

// maxUnreadPerLease is the most unread events that one poll lease holds. A
// lease at it misses the next events, and its next poll says truncated.
const maxUnreadPerLease = 1000

// planRawIngest holds the whole policy of a raw ingest, so that a store only
// matches interests and writes what it returns. Each interest at its cap
// misses the event, each that would get more than maxEventBodyBytes cannot
// get it, and the others are served. The stored body is the whole payload
// when a served interest wants it whole, and else only the union of the
// fields that the served interests want.
func planRawIngest(doc rawDocument, interests []RawInterest) RawPlan {
	var plan RawPlan
	var served []RawInterest
	for _, in := range interests {
		atCap := in.Pending >= MaxPendingDeliveries
		if in.IsLease {
			atCap = in.Pending >= maxUnreadPerLease
		}
		tooLarge := !atCap && rawTooLarge(doc, in.Fields)
		switch {
		case in.IsLease && (atCap || tooLarge):
			plan.Lost = append(plan.Lost, in.Lease)
		case in.IsLease:
			plan.Unread = append(plan.Unread, in.Lease)
			served = append(served, in)
		case atCap:
			plan.Gaps = append(plan.Gaps, RawGap{Key: in.Subscription, Incarnation: in.Incarnation})
		case tooLarge:
			plan.Deliveries = append(plan.Deliveries, RawOutbox{Key: in.Subscription, Incarnation: in.Incarnation, GiveUp: GiveUpEventTooLarge})
		default:
			plan.Deliveries = append(plan.Deliveries, RawOutbox{Key: in.Subscription, Incarnation: in.Incarnation})
			served = append(served, in)
		}
	}
	if len(served) == 0 {
		return plan
	}
	var fields []string
	for _, in := range served {
		if len(in.Fields) == 0 {
			plan.Body = doc.body
			return plan
		}
		fields = append(fields, in.Fields...)
	}
	plan.Fields = sortedSet(fields)
	plan.Body = doc.project(plan.Fields)
	return plan
}

func rawTooLarge(doc rawDocument, fields []string) bool {
	if len(fields) == 0 {
		return len(doc.body) > maxEventBodyBytes
	}
	return len(doc.project(fields)) > maxEventBodyBytes
}
