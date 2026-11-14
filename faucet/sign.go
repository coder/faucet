package faucet

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/xerrors"
)

const maxMessageIDBytes = 128

// SignWebhook returns the Standard Webhooks headers for one message: webhook-id,
// webhook-timestamp and webhook-signature, which is "v1," and the base64 of the
// HMAC-SHA256 of id, timestamp and body joined by dots, keyed with the decoded
// secret. secret is a whsec_ value as events/subscribe accepts it. id is 1 to
// 128 letters, digits, underscores or hyphens. The errors never contain the
// secret.
func SignWebhook(secret, id string, timestamp time.Time, body []byte) (http.Header, error) {
	return signWithSecrets([]subscriberSecret{subscriberSecret(secret)}, id, timestamp, body)
}

// signWithSecrets is SignWebhook for one or more secrets. The signature header
// holds one "v1," entry per distinct signature, in the order given,
// space-delimited, as Standard Webhooks defines for a rotation: a receiver
// accepts the message when any entry verifies.
func signWithSecrets(secrets []subscriberSecret, id string, timestamp time.Time, body []byte) (http.Header, error) {
	switch {
	case len(secrets) == 0:
		return nil, xerrors.New("webhook needs at least one secret to sign with")
	case !validMessageID(id):
		return nil, xerrors.Errorf("webhook id must be 1 to %d letters, digits, underscores or hyphens", maxMessageIDBytes)
	case timestamp.Unix() <= 0:
		return nil, xerrors.New("webhook timestamp must be after 1970")
	}
	seconds := strconv.FormatInt(timestamp.Unix(), 10)
	signatures := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		key, ok := decodeSecret(string(secret))
		if !ok {
			return nil, xerrors.New("webhook secret is not a whsec_ value of 24 to 64 bytes")
		}
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte(id + "." + seconds + "."))
		_, _ = mac.Write(body)
		if signature := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)); !slices.Contains(signatures, signature) {
			signatures = append(signatures, signature)
		}
	}

	headers := http.Header{}
	headers.Set("webhook-id", id)
	headers.Set("webhook-timestamp", seconds)
	headers.Set("webhook-signature", strings.Join(signatures, " "))
	return headers, nil
}

func validMessageID(id string) bool {
	if id == "" || len(id) > maxMessageIDBytes {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}
