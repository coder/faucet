package oauth

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-jose/go-jose/v4"
	"golang.org/x/xerrors"
)

const (
	// maxKeyDocumentBytes bounds a key set, pasted or fetched, and a
	// discovery document.
	maxKeyDocumentBytes = 32 << 10
	maxTrustRuleClaims  = 16
	maxClaimBytes       = 256
)

// TrustRule lets the tokens that an outside issuer signs for a workload, such
// as a Vercel function or a Kubernetes pod, act as one service client. A token
// that the issuer's keys verify matches when its iss equals Issuer and each
// claim that Claims names holds that value as a JSON string.
type TrustRule struct {
	// Issuer is the token's iss, an https URL compared byte for byte.
	Issuer string
	// Claims maps a claim name to the value that the claim must hold, byte
	// for byte.
	Claims map[string]string
	// JWKS is the issuer's key set (RFC 7517), pasted for an issuer that
	// faucet cannot reach. When it is empty, faucet fetches the keys that the
	// issuer's OpenID discovery document names.
	JWKS json.RawMessage
}

// trustRule is a TrustRule that passed ValidateTrustRule, with its pasted
// keys read. keys is nil for a rule whose keys come from discovery.
type trustRule struct {
	TrustRule
	keys *jose.JSONWebKeySet
}

// requiredPin is a claim that a rule must pin, to value unless it is empty,
// and why.
type requiredPin struct {
	claim, value, why string
}

// issuerProfile is what faucet requires of a rule for the issuers of one
// host. Every workload of an issuer can get a token for faucet's audience, so
// a rule must pin claims that no other workload of its issuer holds.
type issuerProfile struct {
	// issuer refuses an issuer of the host that no rule may trust. Nil takes
	// every issuer.
	issuer func(issuer string) error
	pins   []requiredPin
}

// issuerProfiles are the profiles of the hosts whose issuers need more than
// otherIssuers, by host in lower case.
var issuerProfiles = map[string]issuerProfile{
	"oidc.vercel.com": {
		issuer: vercelTeamIssuer,
		pins: []requiredPin{
			{claim: "owner_id", why: "Vercel signs the tokens of every team with the same keys, and a renamed team's slug can pass to another team, so only the team ID tells the teams apart"},
			{claim: "project_id", why: "every project of the team can get a token for faucet"},
			{claim: "environment", value: "production", why: "a preview deployment runs any branch, and a development token sits on a laptop for 12 hours"},
		},
	},
	"token.actions.githubusercontent.com": {
		pins: []requiredPin{
			{claim: "repository_owner_id", why: "the workflows of every GitHub account get their tokens from this issuer, an owner can choose the parts of sub, and the name of a renamed or deleted account can pass to another, so only the owner ID tells the accounts apart"},
			{claim: "repository_id", why: "every repository of the owner can get a token for faucet, and the name of a renamed or deleted repository can pass to another, so only the repository ID names one"},
			{claim: "ref", why: "a workflow on any branch of the repository, or on a pull request from one, can get a token for faucet, so the rule names the ref whose code may act as the client, such as refs/heads/main"},
		},
	},
	"gitlab.com": {
		pins: []requiredPin{
			{claim: "namespace_id", why: "the jobs of every group and user of gitlab.com get their tokens from this issuer, a project can choose the parts of sub, and the path of a renamed namespace can pass to another, so only the namespace ID tells the namespaces apart"},
			{claim: "project_id", why: "every project of the namespace can get a token for faucet, and only the project ID names one project of gitlab.com"},
			{claim: "ref_protected", value: "true", why: "a developer of the project can run a job on any branch that is not protected, and the project chooses who may push to a protected one"},
		},
	},
}

// unpinnableClaims are the claims that no rule may pin, and why.
var unpinnableClaims = map[string]string{
	"iss": "the rule's issuer is the token's iss",
	"aud": "faucet takes a token whose aud is faucet alone, and Kubernetes sends aud as an array, which no pin matches",
	"exp": "faucet checks it, and it is a number, which no pin matches",
	"nbf": "faucet checks it, and it is a number, which no pin matches",
	"iat": "faucet checks it, and it is a number, which no pin matches",
	"jti": "it names one token, so the rule would match one token at most",
}

var otherIssuers = issuerProfile{
	pins: []requiredPin{{claim: "sub", why: "every workload of the issuer can get a token for faucet, and sub names one of them"}},
}

const vercelTeamIssuerPrefix = "https://oidc.vercel.com/"

// vercelTeamIssuer takes the issuer of one Vercel team,
// https://oidc.vercel.com/<team>.
func vercelTeamIssuer(issuer string) error {
	if strings.TrimSuffix(issuer, "/")+"/" == vercelTeamIssuerPrefix {
		return xerrors.Errorf("faucet never trusts %s, which every Vercel team shares: trust the team's issuer, %s<team>", issuer, vercelTeamIssuerPrefix)
	}
	if team, ok := strings.CutPrefix(issuer, vercelTeamIssuerPrefix); !ok || strings.Contains(team, "/") {
		return xerrors.Errorf("a Vercel issuer is %s<team>, as the team's tokens name it", vercelTeamIssuerPrefix)
	}
	return nil
}

func profileOf(u *url.URL) issuerProfile {
	if p, ok := issuerProfiles[strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")]; ok {
		return p
	}
	return otherIssuers
}

// ValidateTrustRule reports whether a service client may hold r: its issuer
// is an https URL, it pins at least one claim to a value and none of
// unpinnableClaims, it pins the claims that the profile of its issuer
// requires, and a pasted key set holds a public key that faucet can use. Its
// error says what to change and why.
func ValidateTrustRule(r TrustRule) error {
	_, err := parseTrustRule(r)
	return err
}

// CanonicalTrustRule returns r in the form to store: its pasted key set is
// {"keys":[...]}, compact, with the entries of keys that ValidateTrustRule
// read and no other member. It fails as ValidateTrustRule does.
func CanonicalTrustRule(r TrustRule) (TrustRule, error) {
	rule, err := parseTrustRule(r)
	return rule.TrustRule, err
}

func parseTrustRule(r TrustRule) (trustRule, error) {
	u, err := url.Parse(r.Issuer)
	switch {
	case len(r.Issuer) > maxURIBytes:
		return trustRule{}, xerrors.Errorf("a trust rule's issuer must be at most %d bytes", maxURIBytes)
	case !printable(r.Issuer) || strings.ContainsFunc(r.Issuer, unicode.IsSpace):
		return trustRule{}, xerrors.New("a trust rule's issuer must be valid UTF-8 with no control character or white space")
	case err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(r.Issuer, "#"):
		return trustRule{}, xerrors.New("a trust rule's issuer must be an https URL with a host, and no user info, query or fragment")
	case len(r.Claims) == 0:
		return trustRule{}, xerrors.New("a trust rule must pin at least one claim, or every token of its issuer matches")
	case len(r.Claims) > maxTrustRuleClaims:
		return trustRule{}, xerrors.Errorf("a trust rule pins at most %d claims", maxTrustRuleClaims)
	}
	for name, value := range r.Claims {
		switch why, unpinnable := unpinnableClaims[name]; {
		case name == "" || value == "":
			return trustRule{}, xerrors.New("a trust rule pins each claim to a value, and neither the name nor the value can be empty")
		case len(name) > maxClaimBytes || len(value) > maxClaimBytes:
			return trustRule{}, xerrors.Errorf("a trust rule's claim names and values must be at most %d bytes", maxClaimBytes)
		case !printable(name) || !printable(value):
			return trustRule{}, xerrors.New("a trust rule's claim names and values must be valid UTF-8 with no control character")
		case strings.TrimSpace(name) != name:
			return trustRule{}, xerrors.Errorf("a trust rule's claim names cannot start or end with white space, as %q does", name)
		case unpinnable:
			return trustRule{}, xerrors.Errorf("a trust rule cannot pin %s: %s", name, why)
		}
	}
	profile := profileOf(u)
	if profile.issuer != nil {
		if err := profile.issuer(r.Issuer); err != nil {
			return trustRule{}, err
		}
	}
	for _, pin := range profile.pins {
		got, ok := r.Claims[pin.claim]
		switch {
		case pin.value == "" && !ok:
			return trustRule{}, xerrors.Errorf("a trust rule for %s must pin %s: %s", r.Issuer, pin.claim, pin.why)
		case pin.value != "" && got != pin.value:
			return trustRule{}, xerrors.Errorf("a trust rule for %s must pin %s=%s: %s", r.Issuer, pin.claim, pin.value, pin.why)
		}
	}
	rule := trustRule{TrustRule: r}
	if len(r.JWKS) > 0 {
		keys, entries, err := parseKeySet(r.JWKS)
		if err != nil {
			return trustRule{}, xerrors.Errorf("a trust rule's JWKS %w", err)
		}
		if !storableStrings(r.JWKS) {
			return trustRule{}, xerrors.New(`a trust rule's JWKS must be valid UTF-8, with no \u0000, lone surrogate or U+FFFD in a string`)
		}
		rule.keys = &keys
		if rule.JWKS, err = compactKeySet(entries); err != nil {
			return trustRule{}, xerrors.Errorf("a trust rule's JWKS: %w", err)
		}
	}
	return rule, nil
}

// printable reports whether s is valid UTF-8 with no control or format
// character, which could hide or fake text in a terminal.
func printable(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool { return unicode.In(r, unicode.Cc, unicode.Cf) })
}

// storableStrings reports whether no string of a JSON text holds NUL or
// U+FFFD. Postgres refuses \u0000 in jsonb, and encoding/json reads a lone
// surrogate, which Postgres refuses too, and invalid UTF-8 as U+FFFD.
func storableStrings(text []byte) bool {
	d := json.NewDecoder(bytes.NewReader(text))
	for {
		token, err := d.Token()
		switch {
		case xerrors.Is(err, io.EOF):
			return true
		case err != nil:
			return false
		}
		if s, ok := token.(string); ok && strings.ContainsAny(s, "\x00\uFFFD") {
			return false
		}
	}
}

// compactKeySet is the key set {"keys":entries}, with no white space. It is
// no longer than the set that entries came from.
func compactKeySet(entries []json.RawMessage) (json.RawMessage, error) {
	set := []byte(`{"keys":[`)
	for i, entry := range entries {
		if i > 0 {
			set = append(set, ',')
		}
		set = append(set, entry...)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, append(set, "]}"...)); err != nil {
		return nil, err
	}
	return compact.Bytes(), nil
}

// keyAlgorithm is the alg that faucet takes a signature of k with, or empty
// for a key that faucet does not use: RS256 for an RSA key of 2048 bits or
// more (RFC 7518 section 3.3), ES256 for a P-256 key.
func keyAlgorithm(k jose.JSONWebKey) jose.SignatureAlgorithm {
	switch key := k.Key.(type) {
	case *rsa.PublicKey:
		if key.N.BitLen() >= 2048 {
			return jose.RS256
		}
	case *ecdsa.PublicKey:
		if key.Curve == elliptic.P256() {
			return jose.ES256
		}
	}
	return ""
}

// secretKeyMembers are the members of a JWK that hold a private or a
// symmetric key: the private parameters of RFC 7518 section 6, and priv,
// which go-jose reads for an AKP key.
var secretKeyMembers = []string{"d", "p", "q", "dp", "dq", "qi", "oth", "k", "priv"}

// parseKeySet reads a JWK Set (RFC 7517 section 5) and keeps its public keys
// that have a kid, for signatures with RS256 or ES256. It skips a key that it
// cannot read, as section 5 says, and refuses a set in which a key has a
// member of secretKeyMembers, read or not, or that holds no key that it
// keeps. It returns the entries of keys too, each as it read it. Its errors
// complete the sentence "the key set ...".
func parseKeySet(body []byte) (jose.JSONWebKeySet, []json.RawMessage, error) {
	if len(body) > maxKeyDocumentBytes {
		return jose.JSONWebKeySet{}, nil, xerrors.Errorf("must be at most %d bytes", maxKeyDocumentBytes)
	}
	entries, err := keySetEntries(body)
	if err != nil {
		return jose.JSONWebKeySet{}, nil, err
	}
	var set jose.JSONWebKeySet
	for _, entry := range entries {
		var members map[string]json.RawMessage
		if json.Unmarshal(entry, &members) != nil {
			continue
		}
		for _, name := range secretKeyMembers {
			if _, ok := members[name]; ok {
				return jose.JSONWebKeySet{}, nil, xerrors.New("must hold no private key")
			}
		}
		var k jose.JSONWebKey
		if err := k.UnmarshalJSON(entry); err != nil {
			continue
		}
		alg := keyAlgorithm(k)
		if alg != "" && k.KeyID != "" && (k.Use == "" || k.Use == "sig") && (k.Algorithm == "" || k.Algorithm == string(alg)) {
			set.Keys = append(set.Keys, k)
		}
	}
	if len(set.Keys) == 0 {
		return jose.JSONWebKeySet{}, nil, xerrors.New("must hold an RSA key of 2048 bits or more, or a P-256 key, for signatures, named by a kid")
	}
	return set, entries, nil
}

// keySetEntries returns the entries of the keys array of a key set. It
// refuses a set with a second member named keys, or one whose name is keys
// in another case: encoding/json takes the last member whose name folds to
// keys, and jsonb sorts the members of an object, so such a set can read as
// other keys once stored.
func keySetEntries(body []byte) ([]json.RawMessage, error) {
	notASet := xerrors.New("must be a JSON object with a keys array")
	if !json.Valid(body) {
		return nil, notASet
	}
	d := json.NewDecoder(bytes.NewReader(body))
	if open, err := d.Token(); err != nil || open != json.Delim('{') {
		return nil, notASet
	}
	var entries []json.RawMessage
	found := false
	for d.More() {
		name, err := d.Token()
		if err != nil {
			return nil, notASet
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, notASet
		}
		member, _ := name.(string)
		if !strings.EqualFold(member, "keys") {
			continue
		}
		if found || member != "keys" {
			return nil, xerrors.New("must name its keys array once, as keys in lower case")
		}
		found = true
		if err := json.Unmarshal(value, &entries); err != nil {
			return nil, notASet
		}
	}
	if !found {
		return nil, notASet
	}
	return entries, nil
}
