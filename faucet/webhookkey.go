package faucet

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"

	"golang.org/x/xerrors"
)

const (
	webhookKeyBytes = 32
	sealVersion     = 1
)

// WebhookKey is the AES-256 key that seals subscriber secrets at rest. Hosts
// build it with NewWebhookKey. No fmt verb prints it.
type WebhookKey struct {
	key [webhookKeyBytes]byte
}

// NewWebhookKey returns the key for exactly 32 bytes that are not all zero. The
// error never contains the bytes.
func NewWebhookKey(b []byte) (WebhookKey, error) {
	var k WebhookKey
	if len(b) != webhookKeyBytes {
		return WebhookKey{}, xerrors.Errorf("webhook key must be %d bytes", webhookKeyBytes)
	}
	copy(k.key[:], b)
	if k == (WebhookKey{}) {
		return WebhookKey{}, xerrors.New("webhook key must not be all zero")
	}
	return k, nil
}

// Format implements fmt.Formatter so that no verb can print the key.
func (WebhookKey) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprint(f, "WebhookKey{}")
}

// messageIDKey is the key of the webhook-ids, kept apart from the key that seals
// secrets.
func (k WebhookKey) messageIDKey() []byte {
	mac := hmac.New(sha256.New, k.key[:])
	_, _ = mac.Write([]byte("faucet webhook-id v1"))
	return mac.Sum(nil)
}

func (k WebhookKey) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(k.key[:])
	if err != nil {
		return nil, xerrors.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, xerrors.Errorf("create GCM: %w", err)
	}
	return gcm, nil
}

// sealSecret encrypts a subscriber secret for the table. The result is
// sealVersion, then the nonce, then the AES-256-GCM ciphertext and tag. The
// nonce is random per call. aad binds the ciphertext to its row. Whoever reads
// the table must use the same layout.
func sealSecret(aead cipher.AEAD, secret subscriberSecret, aad []byte) ([]byte, error) {
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, xerrors.Errorf("read nonce: %w", err)
	}
	sealed := append([]byte{sealVersion}, nonce...)
	return aead.Seal(sealed, nonce, []byte(secret), aad), nil
}

// openSecret reverses sealSecret. Its errors are fixed text, because the cause
// of a failure says nothing a caller can use and must not echo the bytes.
func openSecret(aead cipher.AEAD, sealed, aad []byte) (subscriberSecret, error) {
	nonceSize := aead.NonceSize()
	if len(sealed) < 1+nonceSize+aead.Overhead() || sealed[0] != sealVersion {
		return "", xerrors.New("sealed secret has an unknown layout")
	}
	plain, err := aead.Open(nil, sealed[1:1+nonceSize], sealed[1+nonceSize:], aad)
	if err != nil {
		return "", xerrors.New("sealed secret does not open under this key")
	}
	return subscriberSecret(plain), nil
}
