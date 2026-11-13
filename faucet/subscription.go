package faucet

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"time"

	"golang.org/x/xerrors"
)

// ErrSubscriptionLimit means a principal already holds the most live
// subscriptions that Store.UpsertSubscription was told to allow.
var ErrSubscriptionLimit = xerrors.New("subscription limit reached")

// SubscriptionKeyHash is the SHA-256 of a subscription's key, which SEP-3415
// defines as (principal, callback URL, event name, arguments). Two calls that
// agree on all four address one subscription. The principal is part of the
// hash, so one principal's key never matches another's.
type SubscriptionKeyHash [sha256.Size]byte

// NewSubscriptionKeyHash hashes the four parts of a subscription key. Each
// part is length-prefixed, so no two keys share an encoding. arguments must be
// the canonical JSON that the service builds, because the bytes are hashed.
func NewSubscriptionKeyHash(principal, callbackURL, eventType string, arguments json.RawMessage) SubscriptionKeyHash {
	h := sha256.New()
	for _, part := range [][]byte{[]byte(principal), []byte(callbackURL), []byte(eventType), arguments} {
		var size [binary.MaxVarintLen64]byte
		_, _ = h.Write(size[:binary.PutUvarint(size[:], uint64(len(part)))])
		_, _ = h.Write(part)
	}
	var sum SubscriptionKeyHash
	h.Sum(sum[:0])
	return sum
}

// ID is the handle that subscribe returns and that a delivery carries in
// X-MCP-Subscription-Id. It routes. It does not authorize anything.
func (k SubscriptionKeyHash) ID() string {
	return "sub_" + hex.EncodeToString(k[:8])
}

// Subscription is one stored webhook subscription. It holds no GitHub token.
type Subscription struct {
	KeyHash     SubscriptionKeyHash
	Principal   string
	CallbackURL string
	EventType   string
	// Arguments is the canonical JSON of the event arguments.
	Arguments json.RawMessage
	// RepoID is the GitHub ID of the repository that the principal could read
	// at the last subscribe.
	RepoID int64
	// SecretEnc is the subscriber's signing secret, sealed. It is never the
	// plaintext.
	SecretEnc []byte
	// PreviousSecretEnc is the secret that SecretEnc replaced, sealed like it,
	// while the grace window of a rotation runs, and nil otherwise. The store
	// assigns it, so UpsertSubscription ignores the field.
	PreviousSecretEnc []byte
	// PreviousSecretUntil is when PreviousSecretEnc stops signing, and zero
	// when it is nil. The store assigns it, so UpsertSubscription ignores the
	// field. A reader treats a value at or before the time of its send as
	// "no previous secret", because the store clears expired ones only when
	// ClearExpiredPreviousSecrets runs.
	PreviousSecretUntil time.Time
	// RotationGrace is an input of UpsertSubscription and is never returned:
	// when the call replaces the SecretEnc of a live row with a different
	// value, the replaced one becomes PreviousSecretEnc for this long. Zero
	// starts no window and drops any previous secret.
	RotationGrace time.Duration
	// Cursor is the event ID after which delivery starts. A refresh of a live
	// subscription keeps it.
	Cursor int64
	// CreatedAt and UpdatedAt are assigned by the store.
	CreatedAt time.Time
	UpdatedAt time.Time
	ExpiresAt time.Time
	// Status is what deliveries have recorded. The store assigns it, so
	// UpsertSubscription ignores the field and keeps what a refreshed row has.
	Status DeliveryStatus
}

// Validate checks the fields a caller supplies to Store.UpsertSubscription.
// CreatedAt, UpdatedAt, Status, PreviousSecretEnc and PreviousSecretUntil are
// ignored because the store assigns them. It requires KeyHash to be the hash
// of the identity fields, so a row cannot carry the key of another principal's
// subscription.
func (s Subscription) Validate(now time.Time) error {
	switch {
	case s.Principal == "":
		return xerrors.New("principal is required")
	case s.CallbackURL == "":
		return xerrors.New("callback URL is required")
	case s.EventType == "":
		return xerrors.New("event type is required")
	case !json.Valid(s.Arguments) || !bytes.HasPrefix(s.Arguments, []byte("{")):
		return xerrors.New("arguments must be a JSON object")
	case s.RepoID <= 0:
		return xerrors.New("repo ID must be positive")
	case len(s.SecretEnc) == 0:
		return xerrors.New("sealed secret is required")
	case s.Cursor < 0:
		return xerrors.New("cursor must not be negative")
	case !s.ExpiresAt.After(now):
		return xerrors.New("expiry must be after now")
	case s.RotationGrace < 0 || now.Add(s.RotationGrace).After(s.ExpiresAt):
		return xerrors.New("rotation grace must not be negative or outlast the expiry")
	case s.KeyHash != NewSubscriptionKeyHash(s.Principal, s.CallbackURL, s.EventType, s.Arguments):
		return xerrors.New("key hash does not match the principal, callback URL, event type and arguments")
	}
	return nil
}
