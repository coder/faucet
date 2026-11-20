package oauth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/superblink-events/faucet"
)

const (
	tokenKeyBytes    = 32
	tokenSealVersion = 1
	// consentFormKeyInfo is the HKDF info of the key that seals consent
	// forms, so that a form and a GitHub token share no key.
	consentFormKeyInfo = "faucet consent form key v1"
)

// ErrUnreadableToken is the error of a sealed token that does not open: its
// key is not in the ring, the key is another one, the bytes changed, or the
// layout is unknown. An error that wraps it names the key ID.
var ErrUnreadableToken = xerrors.New("the sealed token cannot be read")

var tokenKeyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// TokenKeys is the ring of AES-256 keys that seals the GitHub tokens of a
// connection. The first key seals, and every key opens the tokens that it
// sealed, so an operator rotates by putting a new key first and drops the old
// one once no connection names it. No fmt verb prints a key.
type TokenKeys struct {
	// Both fields hold pointers because fmt prints a value that it reaches
	// through an unexported field without calling Format, and there a
	// pointer prints as an address.
	sealing *tokenKey
	byID    map[string]*tokenKey
}

type tokenKey struct {
	id string
	// aead seals the GitHub tokens of connections, under the key itself.
	aead cipher.AEAD
	// forms seals consent forms, under a key derived from the key with
	// HKDF-SHA256.
	forms cipher.AEAD
}

// ParseTokenKeys reads the value of FAUCET_TOKEN_KEY: one or more
// <key_id>:<key> separated by commas, where a key ID is 1 to 32 letters,
// digits, '_' or '-', and a key is exactly 32 bytes, not all zero, in
// standard base64. An error names a key by its position or its ID, never by
// its bytes.
func ParseTokenKeys(raw string) (TokenKeys, error) {
	if raw == "" {
		return TokenKeys{}, xerrors.New("no token key is given")
	}
	k := TokenKeys{byID: map[string]*tokenKey{}}
	for i, entry := range strings.Split(raw, ",") {
		key, err := parseTokenKey(i+1, entry)
		if err != nil {
			return TokenKeys{}, err
		}
		if k.byID[key.id] != nil {
			return TokenKeys{}, xerrors.Errorf("token key ID %q appears twice", key.id)
		}
		k.byID[key.id] = key
		if k.sealing == nil {
			k.sealing = key
		}
	}
	return k, nil
}

func parseTokenKey(position int, entry string) (*tokenKey, error) {
	id, encoded, ok := strings.Cut(entry, ":")
	if !ok || !tokenKeyIDPattern.MatchString(id) {
		return nil, xerrors.Errorf("token key %d is not <key_id>:<key>, with a key ID of 1 to 32 letters, digits, '_' or '-'", position)
	}
	// The decoder skips CR and LF even in strict mode.
	b, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(b) != tokenKeyBytes || strings.ContainsAny(encoded, "\r\n") {
		return nil, xerrors.Errorf("token key %q is not %d bytes in standard base64", id, tokenKeyBytes)
	}
	if [tokenKeyBytes]byte(b) == [tokenKeyBytes]byte{} {
		return nil, xerrors.Errorf("token key %q is all zero", id)
	}
	aead, err := newTokenAEAD(b)
	if err != nil {
		return nil, xerrors.Errorf("token key %q: %w", id, err)
	}
	formKey, err := hkdf.Key(sha256.New, b, nil, consentFormKeyInfo, tokenKeyBytes)
	if err != nil {
		return nil, xerrors.Errorf("derive the consent form key of token key %q: %w", id, err)
	}
	forms, err := newTokenAEAD(formKey)
	if err != nil {
		return nil, xerrors.Errorf("consent form key of token key %q: %w", id, err)
	}
	return &tokenKey{id: id, aead: aead, forms: forms}, nil
}

func newTokenAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, xerrors.Errorf("create the cipher: %w", err)
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, xerrors.Errorf("create GCM: %w", err)
	}
	return aead, nil
}

// empty reports whether k holds no key, as the zero TokenKeys does.
func (k TokenKeys) empty() bool { return k.sealing == nil }

// Format implements fmt.Formatter so that no verb can print a key.
func (TokenKeys) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprint(f, "TokenKeys{}")
}

// seal seals both tokens of c under the first key and records its ID in c.
// A sealed token is tokenSealVersion, then a random 12-byte nonce, then the
// AES-256-GCM ciphertext and tag. Its additional data names the provider, the
// connection and the field, so a sealed token copied to another row, or to
// the other field of its row, does not open.
func (k TokenKeys) seal(c *Connection, access, refresh string) error {
	if k.sealing == nil {
		return xerrors.New("no token key to seal with")
	}
	if c.ID == "" || c.Provider == "" {
		return xerrors.New("a connection needs its ID and provider before its tokens are sealed")
	}
	c.AccessToken = k.sealing.seal(tokenAAD(*c, "access"), access)
	c.RefreshToken = k.sealing.seal(tokenAAD(*c, "refresh"), refresh)
	c.KeyID = k.sealing.id
	return nil
}

// accessToken opens the access token of c.
func (k TokenKeys) accessToken(c Connection) (string, error) {
	return k.open(c, "access", c.AccessToken)
}

// refreshToken opens the refresh token of c.
func (k TokenKeys) refreshToken(c Connection) (string, error) {
	return k.open(c, "refresh", c.RefreshToken)
}

// open reverses seal. Its errors hold the key ID and the field, never a byte
// of the token or of the key.
func (k TokenKeys) open(c Connection, field string, sealed []byte) (string, error) {
	key := k.byID[c.KeyID]
	switch {
	case key == nil:
		return "", xerrors.Errorf("the %s token is sealed under key %q, which the token keys do not hold: %w", field, c.KeyID, ErrUnreadableToken)
	case len(sealed) == 0 || sealed[0] != tokenSealVersion:
		return "", xerrors.Errorf("the %s token under key %q has an unknown layout: %w", field, c.KeyID, ErrUnreadableToken)
	}
	plain, err := key.aead.Open(nil, nil, sealed[1:], tokenAAD(c, field))
	if err != nil {
		return "", xerrors.Errorf("the %s token does not open under key %q: %w", field, c.KeyID, ErrUnreadableToken)
	}
	return string(plain), nil
}

// pendingLogin is what the consent form carries of a request until its
// user presses Continue, so that showing the consent page stores nothing.
type pendingLogin struct {
	ClientID      string         `json:"client_id"`
	ClientName    string         `json:"client_name"`
	RedirectURI   string         `json:"redirect_uri"`
	ClientState   string         `json:"state"`
	Resource      string         `json:"resource"`
	Scopes        []faucet.Scope `json:"scopes"`
	CodeChallenge string         `json:"code_challenge"`
	CreatedAt     time.Time      `json:"created_at"`
	ExpiresAt     time.Time      `json:"expires_at"`
}

// sealRequest seals the pending request r into the value that its consent
// form carries: the ID of the first key, a dot, and the sealed request in
// base64url. It is sealed under the key's consent form key, and its
// additional data names r's ID and browser hash, so the value opens only
// for that request and with the cookie of the browser that got the page.
func (k TokenKeys) sealRequest(r AuthRequest) (string, error) {
	if k.sealing == nil {
		return "", xerrors.New("no token key to seal with")
	}
	plain, err := json.Marshal(pendingLogin{
		ClientID: r.ClientID, ClientName: r.ClientName, RedirectURI: r.RedirectURI, ClientState: r.ClientState,
		Resource: r.Resource, Scopes: r.Scopes, CodeChallenge: r.CodeChallenge, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt,
	})
	if err != nil {
		return "", xerrors.Errorf("encode the pending request: %w", err)
	}
	sealed := k.sealing.forms.Seal([]byte{tokenSealVersion}, nil, plain, requestAAD(r.ID, r.BrowserHash))
	return k.sealing.id + "." + base64.RawURLEncoding.EncodeToString(sealed), nil
}

// openRequest reverses sealRequest for the request with this ID and
// browser hash. It fails, wrapping ErrUnreadableToken, when the value was
// sealed for another request or browser, or under a key that k does not
// hold, or was changed.
func (k TokenKeys) openRequest(value, id string, browser TokenHash) (AuthRequest, error) {
	keyID, encoded, _ := strings.Cut(value, ".")
	key := k.byID[keyID]
	sealed, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	switch {
	case key == nil:
		return AuthRequest{}, xerrors.Errorf("the pending request is sealed under a key that the token keys do not hold: %w", ErrUnreadableToken)
	case err != nil || len(sealed) == 0 || sealed[0] != tokenSealVersion:
		return AuthRequest{}, xerrors.Errorf("the pending request has an unknown layout: %w", ErrUnreadableToken)
	}
	plain, err := key.forms.Open(nil, nil, sealed[1:], requestAAD(id, browser))
	if err != nil {
		return AuthRequest{}, xerrors.Errorf("the pending request does not open under key %q for this request and browser: %w", keyID, ErrUnreadableToken)
	}
	var p pendingLogin
	if err := json.Unmarshal(plain, &p); err != nil {
		return AuthRequest{}, xerrors.Errorf("decode the pending request: %w", err)
	}
	return AuthRequest{
		ID: id, State: AuthRequestPending, ClientID: p.ClientID, ClientName: p.ClientName, RedirectURI: p.RedirectURI,
		ClientState: p.ClientState, Resource: p.Resource, Scopes: p.Scopes, CodeChallenge: p.CodeChallenge,
		BrowserHash: browser, CreatedAt: p.CreatedAt, ExpiresAt: p.ExpiresAt,
	}, nil
}

func requestAAD(id string, browser TokenHash) []byte {
	return append([]byte("faucet pending request v1\x00"+id+"\x00"), browser[:]...)
}

func (key *tokenKey) seal(aad []byte, token string) []byte {
	return key.aead.Seal([]byte{tokenSealVersion}, nil, []byte(token), aad)
}

func tokenAAD(c Connection, field string) []byte {
	return []byte("faucet connection token v1\x00" + c.Provider + "\x00" + c.ID + "\x00" + field)
}
