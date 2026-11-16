package faucet

import (
	"time"

	"golang.org/x/xerrors"
)

const (
	// RedeliveryDateSlack is how far GitHub's Date header may lie behind the
	// clock that deletes attempts. A gap ends this much above the end of
	// GitHub's retention that the Date gives, and a Date that is further
	// behind an attempt that GitHub has listed fails the read.
	RedeliveryDateSlack = time.Hour
	// RedeliveryDateMargin is how far GitHub's Date header may run ahead of
	// GitHub's list. A gap that starts at the Date of the last head read
	// starts this much below it.
	RedeliveryDateMargin = 6 * time.Hour

	// An HTTP Date drops the fraction of its second.
	redeliveryDateResolution = time.Second
)

type attemptTime struct{ at time.Time }

func topOf(scan RedeliveryScan) attemptTime { return attemptTime{scan.Top} }

func deliveredAt(d HookDelivery) attemptTime { return attemptTime{d.DeliveredAt} }

func (r RedeliveryRange) olderEnd() attemptTime { return attemptTime{r.OlderAt} }

func (r RedeliveryRange) newerEnd() attemptTime { return attemptTime{r.NewerAt} }

func newestOn(page DeliveryPage) attemptTime {
	var newest attemptTime
	for _, d := range page.Deliveries {
		newest = newest.orLater(deliveredAt(d))
	}
	return newest
}

func (a attemptTime) orLater(b attemptTime) attemptTime {
	if b.at.After(a.at) {
		return b
	}
	return a
}

func (a attemptTime) orEarlier(b attemptTime) attemptTime {
	if a.at.IsZero() || (!b.at.IsZero() && b.at.Before(a.at)) {
		return b
	}
	return a
}

type readBound struct{ stop time.Time }

func (a attemptTime) bound() readBound { return readBound{a.at.Add(-RedeliveryReadOverlap)} }

func (b readBound) excludes(a attemptTime) bool { return a.at.Before(b.stop) }

func (b readBound) rangeEnd() attemptTime { return attemptTime{b.stop} }

type pageDate struct{ date time.Time }

var errUnusableDate = xerrors.New("GitHub's Date header is unusable")

func checkDate(page DeliveryPage, top attemptTime) (pageDate, error) {
	known := top.orLater(newestOn(page))
	switch {
	case page.At.IsZero():
		return pageDate{}, xerrors.Errorf("%w: the page has none", errUnusableDate)
	case page.At.Before(known.at.Add(-RedeliveryDateSlack)):
		return pageDate{}, xerrors.Errorf("%w: it is %s before the newest attempt, and at most %s is allowed",
			errUnusableDate, known.at.Sub(page.At), RedeliveryDateSlack)
	}
	return pageDate{page.At}, nil
}

func (d pageDate) gapEnd(covered attemptTime) time.Time {
	end := d.date.Add(redeliveryDateResolution - GitHubDeliveryRetention + RedeliveryDateSlack)
	if !covered.at.IsZero() && covered.at.Before(end) {
		end = covered.at
	}
	return end
}

func (d pageDate) expiredBelow(anchor clearAnchor) (time.Time, bool) {
	if !anchor.set() {
		return time.Time{}, false
	}
	return earlierOf(d.date, anchor.headDate).Add(-GitHubDeliveryRetention), true
}

type clearAnchor struct{ headDate time.Time }

func clearOf(scan RedeliveryScan) clearAnchor { return clearAnchor{scan.Clear} }

func (c clearAnchor) set() bool { return !c.headDate.IsZero() }

func (c clearAnchor) unseenFrom() time.Time {
	return c.headDate.Add(-RedeliveryDateMargin - RedeliveryReadOverlap)
}

func (c clearAnchor) headGapStart(top attemptTime) time.Time {
	var start time.Time
	if !top.at.IsZero() {
		start = top.bound().stop
	}
	if c.set() && c.unseenFrom().After(start) {
		start = c.unseenFrom()
	}
	return start
}

func (c clearAnchor) firstRunBound(newest attemptTime) readBound {
	stop := newest.at.Add(-RedeliveryStartLookback)
	if c.set() {
		stop = earlierOf(stop, c.headDate.Add(-RedeliveryDateMargin))
	}
	return readBound{stop}
}

func (c clearAnchor) firstRunGapStart(newest attemptTime) time.Time {
	start := newest.at.Add(-GitHubDeliveryRetention)
	if c.set() {
		start = earlierOf(start, c.unseenFrom())
	}
	return start
}
