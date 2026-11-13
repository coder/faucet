package faucet

import (
	"time"
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

func (t ScanTarget) sealedSecrets() sealedSecrets {
	return sealedSecrets{current: t.SecretEnc, previous: t.PreviousSecretEnc, previousUntil: t.PreviousSecretUntil}
}

func (c DeliveryClaim) sealedSecrets() sealedSecrets {
	return sealedSecrets{current: c.SecretEnc, previous: c.PreviousSecretEnc, previousUntil: c.PreviousSecretUntil}
}

// signingSecrets opens the secrets that sign a send at now: the current one,
// and the previous one while now is before its end. A previous secret that the
// store has not cleared yet, but whose window ended, signs nothing. The errors
// are those of openSecret, which hold no secret.
func (wh *webhookState) signingSecrets(now time.Time, key SubscriptionKeyHash, sealed sealedSecrets) ([]subscriberSecret, error) {
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
		return nil, err
	}
	return append(secrets, previous), nil
}
