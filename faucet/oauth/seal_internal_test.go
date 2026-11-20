package oauth

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
)

// keyBytes returns 32 made-up key bytes that start at first and count up, so
// that a printed key would show as a run like a0a1a2 or 160 161 162.
func keyBytes(first byte) []byte {
	b := make([]byte, tokenKeyBytes)
	for i := range b {
		b[i] = first + byte(i)
	}
	return b
}

func keyEntry(id string, first byte) string {
	return id + ":" + base64.StdEncoding.EncodeToString(keyBytes(first))
}

func mustParseTokenKeys(t *testing.T, raw string) TokenKeys {
	t.Helper()
	k, err := ParseTokenKeys(raw)
	require.NoError(t, err)
	return k
}

func githubConnection(userID int64) Connection {
	return Connection{ID: ConnectionID(ProviderGitHub, userID), Provider: ProviderGitHub}
}

const (
	plainAccess  = "ghu_madeUpAccessTokenForTheSealTests0001"
	plainRefresh = "ghr_madeUpRefreshTokenForTheSealTests001"
)

func TestASealedConnectionOpensBothOfItsTokens(t *testing.T) {
	t.Parallel()

	k := mustParseTokenKeys(t, keyEntry("k1", 0xa0))
	c := githubConnection(583231)
	require.NoError(t, k.seal(&c, plainAccess, plainRefresh))
	require.Equal(t, "k1", c.KeyID)
	require.NotContains(t, string(c.AccessToken), plainAccess)
	require.NotContains(t, string(c.RefreshToken), plainRefresh)

	access, err := k.accessToken(c)
	require.NoError(t, err)
	require.Equal(t, plainAccess, access)
	refresh, err := k.refreshToken(c)
	require.NoError(t, err)
	require.Equal(t, plainRefresh, refresh)
}

func TestASealedTokenIsTheVersionANonceTheCiphertextAndTheTag(t *testing.T) {
	t.Parallel()

	k := mustParseTokenKeys(t, keyEntry("k1", 0xa0))
	c := githubConnection(583231)
	require.NoError(t, k.seal(&c, plainAccess, plainRefresh))
	require.Equal(t, byte(1), c.AccessToken[0])
	require.Len(t, c.AccessToken, 1+12+len(plainAccess)+16)
}

func TestTwoSealsOfOneTokenDiffer(t *testing.T) {
	t.Parallel()

	k := mustParseTokenKeys(t, keyEntry("k1", 0xa0))
	first, second := githubConnection(583231), githubConnection(583231)
	require.NoError(t, k.seal(&first, plainAccess, plainRefresh))
	require.NoError(t, k.seal(&second, plainAccess, plainRefresh))
	require.NotEqual(t, first.AccessToken, second.AccessToken)
	require.NotEqual(t, first.AccessToken[1:13], second.AccessToken[1:13], "each seal draws its own nonce")
}

func TestASealedTokenOpensOnlyInItsOwnFieldOfItsOwnRow(t *testing.T) {
	t.Parallel()

	k := mustParseTokenKeys(t, keyEntry("k1", 0xa0))
	c := githubConnection(583231)
	require.NoError(t, k.seal(&c, plainAccess, plainRefresh))

	swapped := c
	swapped.AccessToken, swapped.RefreshToken = c.RefreshToken, c.AccessToken
	otherUser := c
	otherUser.ID = ConnectionID(ProviderGitHub, 1)
	otherProvider := c
	otherProvider.Provider = "gitlab"
	for name, moved := range map[string]Connection{
		"the fields swapped":      swapped,
		"another connection's ID": otherUser,
		"another provider":        otherProvider,
	} {
		_, err := k.accessToken(moved)
		require.ErrorIs(t, err, ErrUnreadableToken, name)
		_, err = k.refreshToken(moved)
		require.ErrorIs(t, err, ErrUnreadableToken, name)
	}
}

func TestASecondKeyOpensWhatItSealedWhileTheFirstSealsNewTokens(t *testing.T) {
	t.Parallel()

	old := mustParseTokenKeys(t, keyEntry("old", 0x10))
	before := githubConnection(583231)
	require.NoError(t, old.seal(&before, plainAccess, plainRefresh))

	rotated := mustParseTokenKeys(t, keyEntry("new", 0x50)+","+keyEntry("old", 0x10))
	access, err := rotated.accessToken(before)
	require.NoError(t, err)
	require.Equal(t, plainAccess, access)

	after := githubConnection(583231)
	require.NoError(t, rotated.seal(&after, plainAccess, plainRefresh))
	require.Equal(t, "new", after.KeyID)
	_, err = old.accessToken(after)
	require.ErrorIs(t, err, ErrUnreadableToken, "the old ring does not hold the new key")
}

func TestATokenUnderAnUnknownKeyNamesTheKey(t *testing.T) {
	t.Parallel()

	c := githubConnection(583231)
	require.NoError(t, mustParseTokenKeys(t, keyEntry("retired", 0x10)).seal(&c, plainAccess, plainRefresh))

	_, err := mustParseTokenKeys(t, keyEntry("current", 0x50)).refreshToken(c)
	require.ErrorIs(t, err, ErrUnreadableToken)
	require.Contains(t, err.Error(), `"retired"`)
}

func TestATokenUnderTheWrongKeyOfTheSameIDDoesNotOpen(t *testing.T) {
	t.Parallel()

	c := githubConnection(583231)
	require.NoError(t, mustParseTokenKeys(t, keyEntry("k1", 0x10)).seal(&c, plainAccess, plainRefresh))

	_, err := mustParseTokenKeys(t, keyEntry("k1", 0x50)).accessToken(c)
	require.ErrorIs(t, err, ErrUnreadableToken)
	require.Contains(t, err.Error(), `"k1"`)
}

func TestAChangedSealedTokenDoesNotOpen(t *testing.T) {
	t.Parallel()

	k := mustParseTokenKeys(t, keyEntry("k1", 0xa0))
	c := githubConnection(583231)
	require.NoError(t, k.seal(&c, plainAccess, plainRefresh))

	changed := map[string][]byte{"empty": nil, "only the version": c.AccessToken[:1], "cut short": c.AccessToken[:len(c.AccessToken)-1]}
	for i := range c.AccessToken {
		b := bytes.Clone(c.AccessToken)
		b[i] ^= 0x01
		changed[fmt.Sprintf("byte %d flipped", i)] = b
	}
	for name, sealed := range changed {
		row := c
		row.AccessToken = sealed
		token, err := k.accessToken(row)
		require.ErrorIs(t, err, ErrUnreadableToken, name)
		require.Empty(t, token, name)
		require.NotContains(t, err.Error(), plainAccess[4:12], name)
	}
}

func TestSealNeedsTheConnectionsIDAndProviderAndAKey(t *testing.T) {
	t.Parallel()

	k := mustParseTokenKeys(t, keyEntry("k1", 0xa0))
	for name, c := range map[string]Connection{
		"no ID":       {Provider: ProviderGitHub},
		"no provider": {ID: "github:583231"},
	} {
		require.Error(t, k.seal(&c, plainAccess, plainRefresh), name)
		require.Empty(t, c.AccessToken, name)
		require.Empty(t, c.KeyID, name)
	}
	c := githubConnection(583231)
	require.Error(t, TokenKeys{}.seal(&c, plainAccess, plainRefresh), "the zero ring holds no key")
}

func TestParseTokenKeysTakesEveryWellFormedRing(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		keyEntry("k1", 0xa0),
		keyEntry(strings.Repeat("a", 32), 0xa0),
		keyEntry("2026-11_rotation", 0xa0) + "," + keyEntry("A", 0x10) + "," + keyEntry("b", 0xa0),
	} {
		_, err := ParseTokenKeys(raw)
		require.NoError(t, err, raw)
	}
}

func TestParseTokenKeysRefusesAMalformedRingWithoutQuotingAKey(t *testing.T) {
	t.Parallel()

	key := base64.StdEncoding.EncodeToString(keyBytes(0xa0))
	short := base64.StdEncoding.EncodeToString(keyBytes(0xa0)[:31])
	long := base64.StdEncoding.EncodeToString(append(keyBytes(0xa0), 0xc0))
	for name, raw := range map[string]string{
		"empty":                    "",
		"a key without an ID":      key,
		"an empty ID":              ":" + key,
		"an ID of 33 bytes":        strings.Repeat("a", 33) + ":" + key,
		"an ID with a dot":         "k.1:" + key,
		"an ID with a space":       "k 1:" + key,
		"an empty key":             "k1:",
		"31 bytes":                 "k1:" + short,
		"33 bytes":                 "k1:" + long,
		"base64 without padding":   "k1:" + strings.TrimRight(key, "="),
		"base64url":                "k1:" + base64.URLEncoding.EncodeToString(keyBytes(0xfa)),
		"a space before the key":   "k1: " + key,
		"all zero":                 "k1:" + base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"one ID twice":             "k1:" + key + ",k1:" + base64.StdEncoding.EncodeToString(keyBytes(0x10)),
		"a trailing comma":         "k1:" + key + ",",
		"a space after the comma":  "k1:" + key + ", k2:" + key,
		"a newline after the ring": "k1:" + key + "\n",
		"a newline inside the key": "k1:" + key[:20] + "\r\n" + key[20:],
	} {
		k, err := ParseTokenKeys(raw)
		require.Error(t, err, name)
		require.Equal(t, TokenKeys{}, k, name)
		for _, secret := range []string{key, short, long, key[:12], base64.URLEncoding.EncodeToString(keyBytes(0xfa))[:12]} {
			require.NotContains(t, err.Error(), secret, name)
		}
		if raw != "" {
			require.NotContains(t, err.Error(), raw, name)
		}
	}
}

func TestParseTokenKeysNamesTheKeyItRefuses(t *testing.T) {
	t.Parallel()

	_, err := ParseTokenKeys(keyEntry("k1", 0xa0) + ",k2:" + base64.StdEncoding.EncodeToString(make([]byte, 32)))
	require.ErrorContains(t, err, `"k2"`)
	_, err = ParseTokenKeys(keyEntry("k1", 0xa0) + ",k2")
	require.ErrorContains(t, err, "token key 2 ", "a key without a valid ID is named by its position")
}

// ringHolder keeps a ring in an unexported field, where fmt does not call
// Format.
type ringHolder struct {
	keys TokenKeys
}

type exportedRingHolder struct {
	Keys TokenKeys
}

func TestTokenKeysAreNeverPrinted(t *testing.T) {
	t.Parallel()

	k := mustParseTokenKeys(t, keyEntry("k1", 0xa0)+","+keyEntry("k2", 0x10))
	values := []any{k, &k, ringHolder{keys: k}, &ringHolder{keys: k}, exportedRingHolder{Keys: k}, []TokenKeys{k}, map[string]TokenKeys{"k": k}}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%X", "%d", "%q"} {
		for _, value := range values {
			out := fmt.Sprintf(verb, value)
			for _, first := range []byte{0xa0, 0x10} {
				b := keyBytes(first)
				require.NotContains(t, out, fmt.Sprintf("%x", b[:4]), "%s of %T", verb, value)
				require.NotContains(t, out, fmt.Sprintf("%X", b[:4]), "%s of %T", verb, value)
				require.NotContains(t, out, fmt.Sprintf("%d %d %d %d", b[0], b[1], b[2], b[3]), "%s of %T", verb, value)
				require.NotContains(t, out, fmt.Sprintf("%#x, %#x, %#x", b[0], b[1], b[2]), "%s of %T", verb, value)
				require.NotContains(t, out, base64.StdEncoding.EncodeToString(b)[:12], "%s of %T", verb, value)
			}
		}
	}
}

// The consent form carries its request sealed, and the request opens only
// for its own ID and browser, under a key of the ring, as the form sent it.
func TestASealedRequestOpensOnlyForItsRequestAndBrowser(t *testing.T) {
	t.Parallel()
	old := mustParseTokenKeys(t, keyEntry("old", 0x10))
	rotated := mustParseTokenKeys(t, keyEntry("new", 0x50)+","+keyEntry("old", 0x10))
	now := time.Now().UTC()
	r := AuthRequest{
		ID: "req-1", State: AuthRequestPending, ClientID: "https://agent.example.com/client.json", ClientName: "Agent",
		RedirectURI: "http://127.0.0.1:1/cb", ClientState: "state", Resource: "https://faucet.example.com/github/mcp",
		Scopes: []faucet.Scope{faucet.ScopeEventsRead}, CodeChallenge: "challenge", BrowserHash: HashToken("browser"),
		CreatedAt: now, ExpiresAt: now.Add(authRequestLifetime),
	}
	sealed, err := old.sealRequest(r)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(sealed, "old."), "the value names its key")
	for _, ring := range []TokenKeys{old, rotated} {
		opened, err := ring.openRequest(sealed, r.ID, r.BrowserHash)
		require.NoError(t, err)
		require.True(t, r.CreatedAt.Equal(opened.CreatedAt) && r.ExpiresAt.Equal(opened.ExpiresAt))
		opened.CreatedAt, opened.ExpiresAt = r.CreatedAt, r.ExpiresAt
		require.Equal(t, r, opened)
	}

	changed := []byte(sealed)
	changed[len(changed)-3] ^= 'A' ^ 'B'
	for name, open := range map[string]func() (AuthRequest, error){
		"another request": func() (AuthRequest, error) { return old.openRequest(sealed, "req-2", r.BrowserHash) },
		"another browser": func() (AuthRequest, error) { return old.openRequest(sealed, r.ID, HashToken("another browser")) },
		"a key not held": func() (AuthRequest, error) {
			return mustParseTokenKeys(t, keyEntry("new", 0x50)).openRequest(sealed, r.ID, r.BrowserHash)
		},
		"a changed value": func() (AuthRequest, error) { return old.openRequest(string(changed), r.ID, r.BrowserHash) },
		"no key ID": func() (AuthRequest, error) {
			return old.openRequest(strings.TrimPrefix(sealed, "old."), r.ID, r.BrowserHash)
		},
		"an empty value":             func() (AuthRequest, error) { return old.openRequest("", r.ID, r.BrowserHash) },
		"a value that is not base64": func() (AuthRequest, error) { return old.openRequest("old.!!!", r.ID, r.BrowserHash) },
	} {
		_, err := open()
		require.ErrorIs(t, err, ErrUnreadableToken, name)
	}
}

// The consent form is sealed under a key that is derived for it from the
// token key, so a GitHub token and a form share no key.
func TestAConsentFormIsSealedUnderAKeyOfItsOwn(t *testing.T) {
	t.Parallel()
	k := mustParseTokenKeys(t, keyEntry("k1", 0xa0))
	now := time.Now().UTC()
	r := AuthRequest{
		ID: "req-1", State: AuthRequestPending, ClientID: "https://agent.example.com/client.json", RedirectURI: "http://127.0.0.1:1/cb",
		Resource: "https://faucet.example.com/github/mcp", Scopes: []faucet.Scope{faucet.ScopeEventsRead}, CodeChallenge: "challenge",
		BrowserHash: HashToken("browser"), CreatedAt: now, ExpiresAt: now.Add(authRequestLifetime),
	}
	sealed, err := k.sealRequest(r)
	require.NoError(t, err)
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sealed, "k1."))
	require.NoError(t, err)

	block, err := aes.NewCipher(keyBytes(0xa0))
	require.NoError(t, err)
	tokenKey, err := cipher.NewGCMWithRandomNonce(block)
	require.NoError(t, err)
	_, err = tokenKey.Open(nil, nil, raw[1:], requestAAD(r.ID, r.BrowserHash))
	require.Error(t, err, "the key of the GitHub tokens does not open a form")
	_, err = k.openRequest(sealed, r.ID, r.BrowserHash)
	require.NoError(t, err)
}
