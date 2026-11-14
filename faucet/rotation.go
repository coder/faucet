package faucet

import (
	"context"
	"sync"
	"time"

	"cdr.dev/slog/v3"
)

// rotationGrace is the longest that a secret keeps signing after a refresh
// replaced it. SEP-3415 says "a short grace window". A refresh grants a TTL of
// 10 minutes at most, and the window never outlasts the TTL that the refresh
// granted.
const rotationGrace = 10 * time.Minute

// sealedSecrets are the secrets that sign a send, as the store holds them.
type sealedSecrets struct {
	current       []byte
	previous      []byte
	previousUntil time.Time
}

func (s Subscription) sealedSecrets() sealedSecrets {
	return sealedSecrets{current: s.SecretEnc, previous: s.PreviousSecretEnc, previousUntil: s.PreviousSecretUntil}
}

func (c GapClaim) sealedSecrets() sealedSecrets {
	return sealedSecrets{current: c.SecretEnc, previous: c.PreviousSecretEnc, previousUntil: c.PreviousSecretUntil}
}

func (c DeliveryClaim) sealedSecrets() sealedSecrets {
	return sealedSecrets{current: c.SecretEnc, previous: c.PreviousSecretEnc, previousUntil: c.PreviousSecretUntil}
}

type windowWarnings struct {
	mu    sync.Mutex
	until map[SubscriptionKeyHash]time.Time
}

func (w *windowWarnings) shouldWarn(key SubscriptionKeyHash, until, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for k, end := range w.until {
		if !now.Before(end) {
			delete(w.until, k)
		}
	}
	if seen, ok := w.until[key]; ok && seen.Equal(until) {
		return false
	}
	if w.until == nil {
		w.until = map[SubscriptionKeyHash]time.Time{}
	}
	w.until[key] = until
	return true
}

// signingSecrets opens the secrets that sign a send at now: the current one,
// and the previous one while now is before its end. A previous secret that the
// store has not cleared yet, but whose window ended, signs nothing. The error
// is that of openSecret for the current secret, which holds no secret.
//
// A previous secret that does not open is skipped, with one log line per
// subscription and window, and the current secret signs alone. The store
// cannot open it, so a renewal under another FAUCET_WEBHOOK_KEY moves a
// ciphertext of the old key there. It must never end a delivery or the
// subscription, which the current secret serves.
func (wh *webhookState) signingSecrets(ctx context.Context, now time.Time, key SubscriptionKeyHash, sealed sealedSecrets) ([]subscriberSecret, error) {
	current, err := openSecret(wh.aead, sealed.current, key[:])
	if err != nil {
		return nil, err
	}
	secrets := []subscriberSecret{current}
	if len(sealed.previous) == 0 || !now.Before(sealed.previousUntil) {
		return secrets, nil
	}
	previous, err := openSecret(wh.aead, sealed.previous, key[:])
	if err != nil {
		if wh.warned.shouldWarn(key, sealed.previousUntil, now) {
			wh.logger.Warn(ctx, "the previous secret of a rotation does not open, signing with the current secret only",
				slog.F("subscription", key.ID()))
		}
		return secrets, nil
	}
	return append(secrets, previous), nil
}
