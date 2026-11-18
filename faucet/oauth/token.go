// Package oauth is faucet's own authorization server. It mints access tokens
// for service clients and for users who log in at GitHub, and verifies them.
package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

const (
	accessTokenPrefix  = "fct_"
	refreshTokenPrefix = "fcr_"
	clientSecretPrefix = "fcs_"
	// secretBytes is the random part of every token and secret, written in
	// base64url without padding.
	secretBytes = 32
)

var prefixes = []string{accessTokenPrefix, refreshTokenPrefix, clientSecretPrefix}

// Minted reports whether token starts like a token or secret that faucet
// mints, in any letter case. Such a bearer is faucet's to judge, well formed
// or not, and must never be sent to GitHub.
func Minted(token string) bool {
	for _, prefix := range prefixes {
		if len(token) >= len(prefix) && strings.EqualFold(token[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}

// TokenHash is the SHA-256 of a token, the only form of it that a store
// keeps.
type TokenHash [sha256.Size]byte

// HashToken returns the hash under which a store keeps token.
func HashToken(token string) TokenHash {
	return sha256.Sum256([]byte(token))
}

// wellFormedAccessToken reports whether token has the form of an access
// token: the prefix and 32 bytes in base64url without padding.
func wellFormedAccessToken(token string) bool {
	body, ok := strings.CutPrefix(token, accessTokenPrefix)
	if !ok || len(body) != base64.RawURLEncoding.EncodedLen(secretBytes) {
		return false
	}
	_, err := base64.RawURLEncoding.Strict().DecodeString(body)
	return err == nil
}
