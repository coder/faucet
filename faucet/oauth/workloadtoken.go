package oauth

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-jose/go-jose/v4"
	// The fork matches member names case-sensitively, unlike encoding/json,
	// so "EXP" is not exp.
	"github.com/go-jose/go-jose/v4/json"
	"github.com/go-jose/go-jose/v4/jwt"
	"golang.org/x/xerrors"

	"github.com/coder/quartz"
)

// workloadClockLeeway is how far ahead of faucet's clock a subject token's
// nbf and iat may be. The clocks of Vercel and of a cluster differ from
// faucet's by well under a second, so a minute covers a badly drifted host
// and lets a token start at most a minute early. exp gets no leeway, so that
// faucet mints no token after the subject token's end by its own clock.
const workloadClockLeeway = time.Minute

// minWorkloadTokenLifetime is the least time that a subject token must have
// left, so that lifetime is at least a second: faucet answers expires_in in
// whole seconds, and golang.org/x/oauth2 takes expires_in 0 for a token that
// never expires.
const minWorkloadTokenLifetime = time.Second

// maxSubjectTokenLifetime is the longest that a subject token may live, from
// the earlier of its iat and nbf to its exp. Faucet asks no platform whether
// the workload of a token still exists, so this bounds how long a token
// that outlives it logs in, such as one that kubectl create token --duration
// made for a service account.
const maxSubjectTokenLifetime = 24 * time.Hour

// workloadAlgorithms are the signatures that faucet takes from an outside
// issuer. Every other alg, none and the HMAC ones included, fails the parse.
var workloadAlgorithms = []jose.SignatureAlgorithm{jose.RS256, jose.ES256}

// errSubjectToken is the error of a subject token that faucet refuses. An
// error that wraps it says why, and names no claim value of a rule.
var errSubjectToken = xerrors.New("the subject token is refused")

type workloadChecker struct {
	keys *keySource
}

// newWorkloadChecker returns a checker that fetches the keys of issuers with
// client, which must be the guarded client of faucet.NewHTTPClient: an
// issuer's discovery document chooses the URL of its keys.
func newWorkloadChecker(client *http.Client, clock quartz.Clock) *workloadChecker {
	return &workloadChecker{keys: newKeySource(client, clock)}
}

type workloadMatch struct {
	Rule TrustRule
	// Expires is the subject token's exp.
	Expires time.Time
}

// lifetime is how long a faucet token for m lasts when faucet mints it at
// now.
func (m workloadMatch) lifetime(now time.Time) time.Duration {
	return min(accessTokenLifetime, m.Expires.Sub(now))
}

// check returns the first of rules, in their order, that token matches at
// now. audience is faucet's issuer, which the token's aud must hold as its
// only value. It fetches keys only for the issuer of a rule that names the
// token's iss. When ctx ends, it stops with ctx's error. Its other errors
// wrap errIssuerKeys when the keys of a rule could not be fetched, or else
// errSubjectToken, and then give the reason of a rule that the token's
// signature passed over the reason of one that it did not.
func (c *workloadChecker) check(ctx context.Context, rules []TrustRule, token, audience string, now time.Time) (workloadMatch, error) {
	jws, err := jose.ParseSignedCompact(token, workloadAlgorithms)
	if err != nil {
		return workloadMatch{}, refuseToken("it must be a JWT in compact form, signed with RS256 or ES256")
	}
	header := jws.Signatures[0].Header
	if header.KeyID == "" {
		return workloadMatch{}, refuseToken("its header must name its key in kid")
	}
	// The unverified iss only picks the rules, and with them the keys. match
	// checks the verified iss again.
	var unverified struct {
		Issuer string `json:"iss"`
	}
	if err := json.Unmarshal(jws.UnsafePayloadWithoutVerification(), &unverified); err != nil {
		return workloadMatch{}, refuseToken("its claims must be a JSON object with a string iss")
	}
	var refused error
	for _, r := range rules {
		if r.Issuer != unverified.Issuer {
			continue
		}
		// A rule that a store kept before the profiles refused it matches
		// nothing.
		rule, err := parseTrustRule(r)
		if err != nil {
			continue
		}
		expires, err := c.match(ctx, rule, jws, header, audience, now)
		switch {
		case err == nil:
			return workloadMatch{Rule: r, Expires: expires}, nil
		case ctx.Err() != nil:
			return workloadMatch{}, ctx.Err()
		case refused == nil || rank(err) > rank(refused):
			refused = err
		}
	}
	if refused == nil {
		refused = refuseToken("it matches no trust rule of the client")
	}
	return workloadMatch{}, refused
}

func (c *workloadChecker) match(ctx context.Context, rule trustRule, jws *jose.JSONWebSignature, header jose.Header, audience string, now time.Time) (time.Time, error) {
	key, err := c.key(ctx, rule, header)
	if err != nil {
		return time.Time{}, err
	}
	payload, err := jws.Verify(key)
	if err != nil {
		return time.Time{}, refuseToken("its signature does not verify with its issuer's key")
	}
	var registered jwt.Claims
	var claims map[string]any
	if json.Unmarshal(payload, &registered) != nil || json.Unmarshal(payload, &claims) != nil {
		return time.Time{}, refuseSigned("its claims must be a JSON object, with the registered claims of RFC 7519 in their types")
	}
	issued := registered.IssuedAt
	if registered.NotBefore != nil && (issued == nil || registered.NotBefore.Time().Before(issued.Time())) {
		issued = registered.NotBefore
	}
	switch {
	case registered.Issuer != rule.Issuer:
		return time.Time{}, refuseSigned("its iss must be the rule's issuer")
	case len(registered.Audience) != 1 || registered.Audience[0] != audience:
		return time.Time{}, refuseSigned("its aud must be " + audience + " alone")
	case registered.Expiry == nil:
		return time.Time{}, refuseSigned("it must have an exp")
	case registered.Expiry.Time().Sub(now) < minWorkloadTokenLifetime:
		return time.Time{}, refuseSigned("it has expired, or expires in less than a second")
	case registered.NotBefore != nil && registered.NotBefore.Time().After(now.Add(workloadClockLeeway)):
		return time.Time{}, refuseSigned("its nbf is in the future")
	case registered.IssuedAt != nil && registered.IssuedAt.Time().After(now.Add(workloadClockLeeway)):
		return time.Time{}, refuseSigned("its iat is in the future")
	case issued == nil:
		return time.Time{}, refuseSigned("it must have an iat or an nbf")
	case registered.Expiry.Time().Sub(issued.Time()) > maxSubjectTokenLifetime:
		return time.Time{}, refuseSigned(fmt.Sprintf("it must expire at most %d hours after its iat and its nbf", int(maxSubjectTokenLifetime/time.Hour)))
	}
	for name, want := range rule.Claims {
		if got, ok := claims[name].(string); !ok || got != want {
			return time.Time{}, refuseSigned("it matches no trust rule of the client")
		}
	}
	return registered.Expiry.Time(), nil
}

func (c *workloadChecker) key(ctx context.Context, rule trustRule, header jose.Header) (any, error) {
	set := rule.keys
	if set == nil {
		fetched, err := c.keys.keySet(ctx, rule.Issuer, header.KeyID)
		if err != nil {
			return nil, err
		}
		set = &fetched
	}
	for _, k := range set.Key(header.KeyID) {
		if string(keyAlgorithm(k)) == header.Algorithm {
			return k.Key, nil
		}
	}
	return nil, refuseToken("its kid and alg name no key of its issuer")
}

// subjectTokenError is the error of a subject token that faucet refuses. It
// wraps errSubjectToken, and signed tells a refusal after the token's
// signature verified.
type subjectTokenError struct {
	why    string
	signed bool
}

func (r subjectTokenError) Error() string { return errSubjectToken.Error() + ": " + r.why }

func (subjectTokenError) Unwrap() error { return errSubjectToken }

func refuseToken(why string) error { return subjectTokenError{why: why} }

func refuseSigned(why string) error { return subjectTokenError{why: why, signed: true} }

// rank orders the failures of the rules that a token fails: keys that faucet
// could not fetch first, as the token may match once they come, then a
// refusal after the token's signature verified, then any other refusal.
func rank(err error) int {
	var r subjectTokenError
	switch {
	case xerrors.Is(err, errIssuerKeys):
		return 2
	case xerrors.As(err, &r) && r.signed:
		return 1
	}
	return 0
}
