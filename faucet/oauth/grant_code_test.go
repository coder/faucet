package oauth_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/coder/faucet/faucet/oauth"
)

// go-sdk v1.8.0 detects how a client authenticates: its first token request
// of a public client carries HTTP Basic credentials with an empty password
// and no client_id in the form. A server that redeemed the code before it
// looked at the client burned the code on that request (R6).
func TestAGoSDKClientRedeemsItsCodeWithBasicCredentialsInOneRequest(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	l := h.approve("login-1", 1)

	cfg := oauth2.Config{
		ClientID: grantClientID, RedirectURL: grantRedirectURI,
		Endpoint: oauth2.Endpoint{TokenURL: h.srv.URL + oauth.TokenPath, AuthStyle: oauth2.AuthStyleAutoDetect},
	}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, h.srv.Client())
	token, err := cfg.Exchange(ctx, l.code, oauth2.VerifierOption(l.verifier))
	require.NoError(t, err)

	received := h.tokenRequests()
	require.Len(t, received, 1, "the first request succeeded, so the client did not try another style")
	first := http.Request{Header: http.Header{"Authorization": {received[0].authorization}}}
	id, password, ok := first.BasicAuth()
	require.True(t, ok, "the first request carried Basic credentials")
	require.Equal(t, url.QueryEscape(grantClientID), id)
	require.Empty(t, password)
	require.False(t, received[0].form.Has("client_id"))

	require.Equal(t, "Bearer", token.TokenType)
	require.Regexp(t, `^fcr_[A-Za-z0-9_-]{43}$`, token.RefreshToken)
	require.Equal(t, "events:read events:subscribe", token.Extra("scope"))
	stored, err := h.store.AccessToken(context.Background(), oauth.HashToken(token.AccessToken))
	require.NoError(t, err)
	require.Equal(t, oauth.AccessToken{
		Hash: oauth.HashToken(token.AccessToken), ExpiresAt: h.clock.Now().Add(15 * time.Minute), Scopes: bothScopes,
		Resource: resource, SubjectKind: oauth.SubjectUser, Subject: "login-1",
	}, stored)
	session := h.session("login-1")
	require.Equal(t, oauth.HashToken(token.RefreshToken), session.RefreshHash)
	require.Equal(t, grantClientID, session.ClientID)
	require.Equal(t, oauth.ConnectionID(oauth.ProviderGitHub, 1), session.ConnectionID)
}

// A token request that fails the client check or the proof check changes
// nothing, so the code still works for the client that then sends it right
// (R6).
func TestACodeStaysRedeemableAfterARequestThatFailsTheClientOrTheProofCheck(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	l := h.approve("login-1", 1)
	other := "https://other.example.com/client.json"
	without := func(name string) func(url.Values) { return func(f url.Values) { f.Del(name) } }
	with := func(name, value string) func(url.Values) { return func(f url.Values) { f.Set(name, value) } }

	for name, c := range map[string]struct {
		authorization string
		change        func(url.Values)
		status        int
		code          string
	}{
		"a Basic password":                 {basic(grantClientID, "secret"), without("client_id"), http.StatusUnauthorized, "invalid_client"},
		"a Basic password and a client_id": {basic(grantClientID, "secret"), nil, http.StatusUnauthorized, "invalid_client"},
		"a bearer":                         {"Bearer fct_x", nil, http.StatusUnauthorized, "invalid_client"},
		"a Basic ID that is not URL-encoded right": {
			"Basic " + base64.StdEncoding.EncodeToString([]byte("%zz:")), without("client_id"), http.StatusUnauthorized, "invalid_client",
		},
		"an empty Basic ID":       {"Basic " + base64.StdEncoding.EncodeToString([]byte(":")), nil, http.StatusUnauthorized, "invalid_client"},
		"no client_id":            {"", without("client_id"), http.StatusBadRequest, "invalid_request"},
		"an empty client_id":      {"", with("client_id", ""), http.StatusBadRequest, "invalid_request"},
		"a client_secret":         {"", with("client_secret", "secret"), http.StatusBadRequest, "invalid_request"},
		"Basic and form disagree": {basic(grantClientID, ""), with("client_id", other), http.StatusBadRequest, "invalid_request"},
		"another client_id":       {"", with("client_id", other), http.StatusBadRequest, "invalid_grant"},
		"Basic of another client": {basic(other, ""), without("client_id"), http.StatusBadRequest, "invalid_grant"},
		"no code":                 {"", without("code"), http.StatusBadRequest, "invalid_request"},
		"a wrong code":            {"", with("code", grantSecret(t)), http.StatusBadRequest, "invalid_grant"},
		"no verifier":             {"", without("code_verifier"), http.StatusBadRequest, "invalid_request"},
		"a short verifier":        {"", with("code_verifier", l.verifier[:42]), http.StatusBadRequest, "invalid_request"},
		"a verifier with a space": {"", with("code_verifier", l.verifier[:42]+" "), http.StatusBadRequest, "invalid_request"},
		"a wrong verifier":        {"", with("code_verifier", grantSecret(t)), http.StatusBadRequest, "invalid_grant"},
		"no redirect_uri":         {"", without("redirect_uri"), http.StatusBadRequest, "invalid_request"},
		"a wrong redirect_uri":    {"", with("redirect_uri", grantRedirectURI+"/x"), http.StatusBadRequest, "invalid_grant"},
		"another resource":        {"", with("resource", issuer+"/mcp"), http.StatusBadRequest, "invalid_target"},
	} {
		form := grantCodeForm(l)
		if c.change != nil {
			c.change(form)
		}
		reply := h.post(c.authorization, form)
		grantRequireRefused(t, reply, c.status, c.code, name)
		require.NotContains(t, reply.rawBody, l.code, name)
		require.NotContains(t, reply.rawBody, l.verifier, name)
	}
	_, err := h.store.Session(context.Background(), "login-1")
	require.ErrorIs(t, err, oauth.ErrUnknownSession, "no refusal made a session")

	reply := h.post(basic(grantClientID, ""), grantCodeForm(l))
	grantTokens(t, l.id, reply)
}
