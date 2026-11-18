package faucet

import (
	"context"
	"encoding/json"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
)

func (r *deliveryRun) claimRaw(ctx context.Context) (outbound, bool, error) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	claim, ok, err := r.svc.raw.ClaimRawDelivery(ctx, r.svc.clock.Now(), DeliveryLease, r.gate.full())
	if err = r.unlessEnded(ctx, err); err != nil {
		return outbound{}, false, xerrors.Errorf("claim a raw delivery: %w", err)
	}
	if !ok {
		return outbound{}, false, nil
	}
	r.gate.enter(CallbackOrigin(claim.CallbackURL))
	return r.rawOutbound(claim), true, nil
}

// rawOutbound builds the attempt of a raw delivery. Its webhook-id comes from
// the same function as a typed one: a subscription is typed or raw, never
// both, so the IDs of the two kinds cannot collide.
func (r *deliveryRun) rawOutbound(claim RawDeliveryClaim) outbound {
	s := r.svc
	logger := s.logger.With(slog.F("subscription", claim.Ref.Key.ID()), slog.F("event_id", claim.Ref.EventID))
	messageID := r.wh.messageID(claim.Ref.Key, claim.Ref.EventID)
	return outbound{
		ref: claim.Ref, messageID: messageID, enqueuedAt: claim.EnqueuedAt,
		callbackURL: claim.CallbackURL, secretEnc: claim.SecretEnc,
		secrets: sealedSecrets{current: claim.SecretEnc, previous: claim.PreviousSecretEnc, previousUntil: claim.PreviousSecretUntil},
		mustEnd: func(ctx context.Context) GiveUpReason {
			switch ev := claim.Event; {
			case claim.GiveUp != "":
				return claim.GiveUp
			case ev == nil:
				return GiveUpEventPurged
			case ev.RepoID != claim.RepoID:
				logger.Error(ctx, "event belongs to another repository than the subscription's")
				return GiveUpRepoMismatch
			}
			return ""
		},
		build: func(ctx context.Context) ([]byte, attemptOutcome, bool) {
			var args rawArguments
			if err := json.Unmarshal(claim.Arguments, &args); err != nil {
				// The claim has no principal to delete the subscription by,
				// so the delivery ends as dead under the sealed secret it
				// read: that deletes the subscription and ends its other
				// raw deliveries.
				logger.Error(ctx, "ended a subscription whose stored arguments do not decode")
				return nil, attemptOutcome{finish: Finish{Kind: FinishDead, Unreadable: claim.SecretEnc}, reason: GiveUpArgumentsUndecodable}, false
			}
			data, err := rawData(*claim.Event, args.Fields)
			if err != nil {
				logger.Error(ctx, "the stored raw event cannot be sent", slog.Error(err))
				return nil, giveUp(GiveUpEventMalformed), false
			}
			return encodeWebhookBody(webhookBody{
				EventID: messageID, Name: claim.EventType, Timestamp: claim.Event.ReceivedAt, Data: data, Cursor: encodeRawCursor(claim.Watermark),
			})
		},
		finish: s.raw.FinishRawDelivery,
	}
}

// rawData is the data that a subscription with these field paths gets: the
// stored body when it asked for none and the store holds the whole payload,
// and the projection otherwise. A body that holds less than the subscription
// asked for is never sent as if it were whole.
func rawData(ev RawEvent, fields []string) (json.RawMessage, error) {
	if len(fields) > 0 {
		return projectFields(ev.Body, fields)
	}
	if ev.Fields != nil {
		return nil, xerrors.New("the subscription asks for the whole payload, and the store holds a projection")
	}
	return ev.Body, nil
}

func (r *deliveryRun) rawGaps() gapKind {
	return gapKind{
		noun: "raw gap", claim: r.svc.raw.ClaimRawGap, finish: r.svc.raw.FinishRawGap, cursor: encodeRawCursor, through: "missed_through",
		sent:   "the subscription held the most unsent deliveries when events arrived, the gap envelope was sent",
		failed: "the subscription held the most unsent deliveries when events arrived, the gap envelope was not delivered",
	}
}
