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

func deliveredAt(d HookDelivery) attemptTime { return attemptTime{d.DeliveredAt} }

func (r RedeliveryRange) newerEnd() attemptTime { return attemptTime{r.NewerAt} }

func newestOn(page DeliveryPage) attemptTime {
	var newest attemptTime
	for _, d := range page.Deliveries {
		newest = newest.orLater(deliveredAt(d))
	}
	return newest
}

func (a attemptTime) set() bool { return !a.at.IsZero() }

func (a attemptTime) orLater(b attemptTime) attemptTime {
	if b.at.After(a.at) {
		return b
	}
	return a
}

func (a attemptTime) orEarlier(b attemptTime) attemptTime {
	switch {
	case !a.set():
		return b
	case !b.set():
		return a
	case b.at.Before(a.at):
		return b
	}
	return a
}

type readBound struct{ stop time.Time }

func (a attemptTime) bound() readBound { return readBound{a.at.Add(-RedeliveryReadOverlap)} }

func (r RedeliveryRange) bound() readBound { return readBound{r.OlderAt.Add(-RedeliveryReadOverlap)} }

func (b readBound) excludes(a attemptTime) bool { return a.at.Before(b.stop) }

func (b readBound) loweredTo(t time.Time) readBound { return readBound{earlierOf(b.stop, t)} }

type pageDate struct{ date time.Time }

var errUnusableDate = xerrors.New("GitHub's Date header is unusable")

func checkDate(page DeliveryPage, newestKnown attemptTime) (pageDate, error) {
	known := newestKnown.orLater(newestOn(page))
	switch {
	case page.At.IsZero():
		return pageDate{}, xerrors.Errorf("%w: the page has none", errUnusableDate)
	case page.At.Add(redeliveryDateResolution).Before(known.at.Add(-RedeliveryDateSlack)):
		return pageDate{}, xerrors.Errorf("%w: it is %s before the newest attempt, and at most %s is allowed",
			errUnusableDate, known.at.Sub(page.At), RedeliveryDateSlack+redeliveryDateResolution)
	}
	return pageDate{page.At}, nil
}

func (d pageDate) gapEnd(covered attemptTime) time.Time {
	end := d.date.Add(redeliveryDateResolution - GitHubDeliveryRetention + RedeliveryDateSlack)
	if covered.set() && covered.at.Before(end) {
		end = covered.at
	}
	return end
}

type storedClear struct{ clearAt time.Time }

var errTopWithoutClear = xerrors.New("the store returned a top without a clear: a RedeliveryStore must keep Clear, and must move a top that it stored before into Clear")

func topAndClearOf(scan RedeliveryScan) (attemptTime, storedClear, error) {
	top, clearAt := attemptTime{scan.Top}, storedClear{scan.Clear}
	if top.set() && !clearAt.set() {
		return attemptTime{}, storedClear{}, errTopWithoutClear
	}
	return top, clearAt, nil
}

func (c storedClear) set() bool { return !c.clearAt.IsZero() }

func expiredByBoth(d pageDate, c storedClear) (time.Time, bool) {
	if !c.set() {
		return time.Time{}, false
	}
	return earlierOf(d.date, c.clearAt).Add(-GitHubDeliveryRetention), true
}

func (c storedClear) unseenFrom() time.Time {
	return c.clearAt.Add(-RedeliveryDateMargin - RedeliveryReadOverlap)
}

func (c storedClear) headGapStart(top attemptTime) (time.Time, bool) {
	switch {
	case !c.set():
		return time.Time{}, false
	case !top.set():
		return c.unseenFrom(), true
	}
	return laterOf(top.bound().stop, c.unseenFrom()), true
}

func (c storedClear) firstRunBound(newest attemptTime) readBound {
	bound := readBound{newest.at.Add(-RedeliveryStartLookback)}
	if c.set() {
		bound = bound.loweredTo(c.clearAt.Add(-RedeliveryDateMargin))
	}
	return bound
}

func (c storedClear) firstRunGapStart(newest attemptTime) time.Time {
	start := newest.at.Add(-GitHubDeliveryRetention)
	if c.set() {
		start = earlierOf(start, c.unseenFrom())
	}
	return start
}

func (c storedClear) firstRunGapEnd(bound readBound, expiredEnd time.Time) time.Time {
	if !c.set() {
		return bound.stop
	}
	return laterOf(bound.stop, expiredEnd)
}
