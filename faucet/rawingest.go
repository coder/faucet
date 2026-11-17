package faucet

import (
	"net/http"

	"cdr.dev/slog/v3"
)

// serveRawWebhook answers a verified delivery that no typed family stores. It
// stores the delivery when a live interest of its raw event name wants it,
// and ignores it otherwise.
func (s *Service) serveRawWebhook(w http.ResponseWriter, r *http.Request, logger slog.Logger, eventName, deliveryID string, body []byte) {
	entry, ok := s.rawEntry(rawNamePrefix + eventName)
	var doc rawDocument
	if ok {
		doc, ok = parseRawDocument(body)
	}
	var d RawDelivery
	if ok {
		d = RawDelivery{DeliveryID: deliveryID, Event: entry.event}
		d.RepoID, d.Action, ok = doc.envelope()
	}
	if !ok {
		logger.Debug(r.Context(), "webhook ignored")
		reply(w, http.StatusAccepted, "ignored")
		return
	}

	var plan RawPlan
	var fullLeases int
	res, err := s.raw.IngestRaw(r.Context(), d, s.clock.Now(), func(interests []RawInterest) RawPlan {
		plan = planRawIngest(doc, interests)
		for _, in := range interests {
			if in.IsLease && in.Pending >= maxUnreadPerLease {
				fullLeases++
			}
		}
		return plan
	})
	switch {
	case err != nil:
		logger.Error(r.Context(), "store raw event", slog.Error(err))
		reply(w, http.StatusInternalServerError, "could not store event")
		return
	case !res.Matched:
		logger.Debug(r.Context(), "webhook ignored")
		reply(w, http.StatusAccepted, "ignored")
		return
	}
	logger.Debug(r.Context(), "webhook stored",
		slog.F("type", entry.name), slog.F("raw_id", res.ID), slog.F("inserted", !res.Duplicate),
		slog.F("deliveries", len(plan.Deliveries)), slog.F("unread", len(plan.Unread)),
		slog.F("gaps", len(plan.Gaps)), slog.F("lost", len(plan.Lost)))
	if res.Duplicate {
		reply(w, http.StatusOK, "duplicate")
		return
	}
	// A lease's loss has no other counter, and a gap is sent later, so these
	// lines are the record of an event that an interest missed.
	if n := len(plan.Lost) - fullLeases; n > 0 {
		logger.Warn(r.Context(), "raw event missed by leases",
			slog.F("raw_id", res.ID), slog.F("count", n), slog.F("reason", GiveUpEventTooLarge))
	}
	if fullLeases > 0 {
		logger.Warn(r.Context(), "raw event missed by leases",
			slog.F("raw_id", res.ID), slog.F("count", fullLeases), slog.F("reason", "unread_limit"))
	}
	if n := len(plan.Gaps); n > 0 {
		logger.Warn(r.Context(), "raw event missed by subscriptions",
			slog.F("raw_id", res.ID), slog.F("count", n), slog.F("reason", "pending_limit"))
	}
	reply(w, http.StatusOK, "stored")
}
