package faucet

import (
	"net/http"

	"cdr.dev/slog/v3"
)

// serveRawWebhook answers a verified delivery that no typed family stores. It
// stores the delivery when every reader of its repository may see it and a
// live interest of its raw event name wants it, and ignores it otherwise.
func (s *Service) serveRawWebhook(w http.ResponseWriter, r *http.Request, logger slog.Logger, eventName, deliveryID string, body []byte) {
	entry, ok := s.rawEntry(rawNamePrefix + eventName)
	var doc rawDocument
	if ok {
		doc, ok = parseRawDocument(body)
	}
	ok = ok && doc.readersSee(entry.event)
	var d RawDelivery
	if ok {
		d, ok = doc.envelope(deliveryID, entry.event)
	}
	if !ok {
		logger.Debug(r.Context(), "webhook ignored")
		reply(w, http.StatusAccepted, "ignored")
		return
	}

	var plan RawPlan
	var missed rawMisses
	res, err := s.raw.IngestRaw(r.Context(), d, s.clock.Now(), func(interests []RawInterest) RawPlan {
		plan, missed = planRawIngest(r.Context(), doc, interests)
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
	for _, m := range []struct {
		msg, reason string
		count       int
	}{
		{"raw event missed by leases", string(GiveUpEventTooLarge), missed.leasesTooLarge},
		{"raw event missed by leases", "unread_limit", missed.leasesFull},
		{"raw event missed by subscriptions", "pending_limit", missed.subscriptionsFull},
		{"raw event missed by subscriptions", string(GiveUpEventTooLarge), missed.subscriptionsTooLarge},
	} {
		if m.count > 0 {
			logger.Warn(r.Context(), m.msg, slog.F("raw_id", res.ID), slog.F("count", m.count), slog.F("reason", m.reason))
		}
	}
	reply(w, http.StatusOK, "stored")
}
