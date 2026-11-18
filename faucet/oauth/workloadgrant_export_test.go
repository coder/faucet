package oauth

import (
	"net/http"
	"testing"
	"time"
)

// ServeVercel serves Vercel's real discovery document of the team codercom
// and its real key set, with the key of SignAsVercel added, over TLS at their
// real URLs. It returns a client that reaches them there, for
// WorkloadKeysThrough, and a function after which the key set answers 404.
func ServeVercel(t *testing.T) (client *http.Client, failKeys func()) {
	t.Helper()
	s := newIssuerServer(t)
	s.serveVercel(t, publicJWK(testRSAKey(), testKID))
	return s.client, func() { s.unserve(testVercelKeys) }
}

// WorkloadKeysThrough makes s fetch the keys of workload issuers with client.
func WorkloadKeysThrough(s *Server, client *http.Client) {
	s.workloads = newWorkloadChecker(client, s.clock)
}

// VercelFunctionClaims are the claims of a token of a production function of
// the team codercom, as vercel.com/docs/oidc/reference shows them, issued at
// now for https://faucet.example.com and valid for 2 hours.
func VercelFunctionClaims(now time.Time) map[string]any { return vercelClaims(now) }

// SignAsVercel signs claims with the key that ServeVercel adds.
func SignAsVercel(t *testing.T, claims map[string]any) string { return signVercel(t, claims) }

// VercelRule is a trust rule that VercelFunctionClaims match.
func VercelRule() TrustRule { return testVercelRule() }

// VercelKeyID is the kid of the key that SignAsVercel signs with.
const VercelKeyID = testKID
