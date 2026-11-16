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

// A scan works with three kinds of time. An attempt time is never ahead of
// GitHub's clock. A Date estimates that clock, and it can run ahead of GitHub's
// list. The clear is the Date of an earlier run. Each kind has a type here,
// every value of one is made in this file from the field that it comes from,
// and only firstRunBound leads from a Date to a readBound. So the compiler
// keeps a Date out of every other read bound.

// attemptTime is a time that GitHub stamped on a delivery attempt: a
// delivered_at, the stored top, or an end of a range. GitHub's clock had
// passed it when the attempt was listed. An attempt that GitHub lists later
// is therefore not older, unless it ended late, so a read bound comes from
// this type. The zero value means that there is no attempt.
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

// orEarlier is the earlier of two attempts. No attempt counts as later than
// any attempt.
func (a attemptTime) orEarlier(b attemptTime) attemptTime {
	if a.at.IsZero() || (!b.at.IsZero() && b.at.Before(a.at)) {
		return b
	}
	return a
}

// readBound is where a read stops: the read takes the attempts at or after
// it. One comes from an attempt time, or from firstRunBound.
type readBound struct{ stop time.Time }

// bound is the read bound below a position. The overlap is for an attempt
// that GitHub lists below the position because it ended late.
func (a attemptTime) bound() readBound { return readBound{a.at.Add(-RedeliveryReadOverlap)} }

func (b readBound) excludes(a attemptTime) bool { return a.at.Before(b.stop) }

// rangeEnd is the old end of a range that a first run leaves unread. A first
// run has no position below it, so the range ends at the bound, and its read
// goes the overlap further, into the gap of the first run.
func (b readBound) rangeEnd() attemptTime { return attemptTime{b.stop} }

// pageDate is the Date header of a page that passed checkDate. It estimates
// GitHub's clock when GitHub answered. It can run ahead of GitHub's list, so
// it gives the end of a gap, the floor of a range that closes unread and the
// next clear, and never a read bound.
type pageDate struct{ date time.Time }

var errUnusableDate = xerrors.New("GitHub's Date header is unusable")

// checkDate makes the pageDate of a page, and it is the only way to one. top
// is the newest attempt that the scan knows. A Date further behind an attempt
// than RedeliveryDateSlack would end a gap below attempts that GitHub has
// deleted, so it fails the read, and a page without a Date does too.
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

// gapEnd is where the gap of a read that reached the end of the list ends.
// The Date is at most the slack behind the clock that deletes attempts, and
// it dropped the fraction of its second. So nothing later than the Date plus
// both, less the retention, has expired. covered is the oldest attempt that
// the read saw, and the read missed nothing above it.
func (d pageDate) gapEnd(covered attemptTime) time.Time {
	end := d.date.Add(redeliveryDateResolution - GitHubDeliveryRetention + RedeliveryDateSlack)
	if !covered.at.IsZero() && covered.at.Before(end) {
		end = covered.at
	}
	return end
}

// expiredBelow is the time below which GitHub has deleted every attempt, by
// this Date and by the stored clear. A range whose new end is below it closes
// unread. The lower of the two decides, so one Date that runs far ahead
// closes nothing, and without a stored clear nothing closes.
func (d pageDate) expiredBelow(anchor clearAnchor) (time.Time, bool) {
	if !anchor.set() {
		return time.Time{}, false
	}
	return earlierOf(d.date, anchor.headDate).Add(-GitHubDeliveryRetention), true
}

// clearAnchor is the stored clear: the Date of the last head read, or zero
// before one. A run saw the top of the list then, so an attempt that no run
// has seen is not older than this time, less the margin by which a Date runs
// ahead and less the overlap. It gives gap starts and the bound of a first
// run.
type clearAnchor struct{ headDate time.Time }

func clearOf(scan RedeliveryScan) clearAnchor { return clearAnchor{scan.Clear} }

func (c clearAnchor) set() bool { return !c.headDate.IsZero() }

func (c clearAnchor) unseenFrom() time.Time {
	return c.headDate.Add(-RedeliveryDateMargin - RedeliveryReadOverlap)
}

// headGapStart is where the gap of a head read that reached the end of the
// list starts: at the bound of the read, or where the last head read stopped
// seeing, whichever is later. It is the zero time when no run has read the
// hook, and then there is no gap.
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

// firstRunBound is the bound of a run that has no top: the lookback below the
// newest attempt, and lower when an earlier run or the migration left a
// clear, because tracking began then. It is the one bound that a Date takes
// part in. That is safe because the clear can only lower the bound, and the
// gap of the first run covers everything below it.
func (c clearAnchor) firstRunBound(newest attemptTime) readBound {
	stop := newest.at.Add(-RedeliveryStartLookback)
	if c.set() {
		stop = earlierOf(stop, c.headDate.Add(-RedeliveryDateMargin))
	}
	return readBound{stop}
}

// firstRunGapStart is where the gap below firstRunBound starts: where
// GitHub's list can start at the earliest, and lower when tracking began
// before that.
func (c clearAnchor) firstRunGapStart(newest attemptTime) time.Time {
	start := newest.at.Add(-GitHubDeliveryRetention)
	if c.set() {
		start = earlierOf(start, c.unseenFrom())
	}
	return start
}
