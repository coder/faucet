package faucet

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
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
