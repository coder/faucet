package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/faucet/faucet"
)

const (
	// browserCookiePrefix starts the name of the cookie that ties a login
	// to the browser that saw its consent page. The name ends with the
	// request's ID, so two logins in one browser do not share a cookie.
	browserCookiePrefix = "__Host-faucet_"
	// githubCookieSuffix ends the name of the cookie that ties GitHub's
	// answer to the browser that pressed Continue.
	githubCookieSuffix = "_gh"

	consentFormLabel = "faucet consent form v1"
	githubStateLabel = "faucet github state v1"
	githubPKCELabel  = "faucet github pkce v1"

	// maxConsentFormBytes holds the sealed request of the form, whose
	// client ID, redirect URI and client state are 2 KiB each at most.
	maxConsentFormBytes = 16 << 10
	maxClientStateBytes = 2048
)

// refusal is a step of a browser login that does not go on: the page that
// the browser gets, and what the log gets.
type refusal struct {
	status int
	page   errorPage
	reason string
	// err is set when faucet or GitHub failed, rather than the request.
	err error
}

// badAuthorization refuses an authorization request that the client got
// wrong. Its page shows the reason, for the client's developer.
func badAuthorization(reason string) *refusal {
	return &refusal{status: http.StatusBadRequest, reason: reason, page: errorPage{
		Title:   "This login cannot start",
		Message: "The app that sent you here asked for a login that faucet does not accept. Go back to your agent and start the login again; if this page comes back, the app's developer must fix the request.",
		Detail:  reason,
	}}
}

// forbiddenConsent refuses a consent that faucet cannot tie to the browser
// that saw the consent page. Its page does not say which check failed.
func forbiddenConsent(reason string) *refusal {
	return &refusal{status: http.StatusForbidden, reason: reason, page: errorPage{
		Title:   "Login stopped",
		Message: "faucet cannot tell that this browser started this login, so the login stops here. Go back to your agent and start the login again.",
	}}
}

// unknownLogin refuses a callback of a login that is not waiting for
// GitHub.
func unknownLogin(reason string) *refusal {
	return &refusal{status: http.StatusBadRequest, reason: reason, page: errorPage{
		Title:   "This login is over",
		Message: "faucet has no open login for this page: it expired, it finished already, or it did not start here. Go back to your agent and start the login again.",
	}}
}

// wrongBrowser refuses a callback in a browser other than the one that
// pressed Continue. The login stays open for that browser.
func wrongBrowser(reason string) *refusal {
	return &refusal{status: http.StatusForbidden, reason: reason, page: errorPage{
		Title:   "Login stopped",
		Message: "This browser is not the one that pressed Continue for this login. Finish the login in that browser, or go back to your agent and start the login again.",
	}}
}

// githubFailure answers a callback whose login GitHub did not complete.
// The login stays open.
func githubFailure(status int, err error) *refusal {
	return &refusal{status: status, err: err, page: errorPage{
		Title:   "GitHub did not finish the login",
		Message: "faucet could not get your login from GitHub. Go back to your agent and start the login again.",
	}}
}

func wrongMethod(method string) *refusal {
	return &refusal{status: http.StatusMethodNotAllowed, reason: "method " + method, page: errorPage{
		Title:   "This page takes a link",
		Message: "faucet's login pages open from a link of your agent. Go back to your agent and start the login again.",
	}}
}

func failure(err error) *refusal {
	return &refusal{status: http.StatusInternalServerError, err: err, page: errorPage{
		Title:   "Something went wrong",
		Message: "faucet could not finish this step of the login. Go back to your agent and start the login again.",
	}}
}

// refuse logs ref and sends its page. fields name the login, never a
// secret of it.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, ref *refusal, fields ...slog.Field) {
	fields = append(fields, slog.F("path", r.URL.Path), slog.F("status", ref.status))
	if ref.err != nil {
		s.logger.Error(r.Context(), "browser login failed", append(fields, slog.Error(ref.err))...)
	} else {
		s.logger.Info(r.Context(), "browser login refused", append(fields, slog.F("reason", ref.reason))...)
	}
	writePage(w, ref.status, "error", ref.page)
}

// authorizeHandler serves the authorization endpoint of the browser login:
// GET shows the consent page, and POST is its form. It never redirects to
// a client, because nothing yet proves that the browser is the user's.
func (s *Server) browserAuthorizeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setLoginHeaders(w)
		switch r.Method {
		case http.MethodGet:
			s.authorize(w, r)
		case http.MethodPost:
			s.consent(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			s.refuse(w, r, wrongMethod(r.Method))
		}
	})
}

// authorize checks an authorization request, gives the browser the
// request's cookie and shows the consent page. It stores nothing: the form
// of the page carries the request, sealed, until the user presses Continue.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	host := slog.F("client_host", clientHost(r.URL.Query().Get("client_id")))
	request, ref := s.readAuthorization(ctx, r.URL.RawQuery)
	if ref != nil {
		s.refuse(w, r, ref, host)
		return
	}
	id, err := newID()
	if err != nil {
		s.refuse(w, r, failure(err), host)
		return
	}
	browser, err := newSecret("")
	if err != nil {
		s.refuse(w, r, failure(err), host)
		return
	}
	now := s.clock.Now()
	request.ID, request.State, request.BrowserHash = id, AuthRequestPending, HashToken(browser)
	request.CreatedAt, request.ExpiresAt = now, now.Add(authRequestLifetime)
	sealed, err := s.login.keys.sealRequest(request)
	if err != nil {
		s.refuse(w, r, failure(xerrors.Errorf("seal the authorization request: %w", err)), host)
		return
	}
	http.SetCookie(w, loginCookie(browserCookieName(id), browser, http.SameSiteStrictMode))
	s.logger.Debug(ctx, "consent page shown", host, slog.F("request_id", id))
	writePage(w, http.StatusOK, "consent", newConsentPage(request, sealed, derive(consentFormLabel, browser)))
}

// readAuthorization parses an authorization request into the fields of a
// new AuthRequest that the client chooses. It fetches the client's document.
func (s *Server) readAuthorization(ctx context.Context, rawQuery string) (AuthRequest, *refusal) {
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return AuthRequest{}, badAuthorization("the query does not parse")
	}
	for _, values := range q {
		if len(values) > 1 {
			return AuthRequest{}, badAuthorization("each parameter must appear at most once")
		}
	}
	clientID := q.Get("client_id")
	switch {
	case clientID == "":
		return AuthRequest{}, badAuthorization("client_id is required: the URL of the client's metadata document")
	case strings.ContainsFunc(clientHost(clientID), func(r rune) bool { return r > '~' }):
		// A name in another script can look like a known host on the
		// consent page.
		return AuthRequest{}, badAuthorization("the host of client_id must be ASCII: write an internationalized name in its xn-- form")
	case hostKey(clientID) == hostKey(s.issuer):
		return AuthRequest{}, badAuthorization("client_id must not be on faucet's own host: faucet does not fetch documents from itself")
	}
	doc, err := s.login.documents.fetch(ctx, clientID)
	if err != nil {
		ref := badAuthorization(err.Error())
		if xerrors.Is(err, errDocumentUnreachable) {
			// The cause can name a resolver or an address of faucet's own
			// network. The log has it.
			ref.page.Detail = errDocumentUnreachable.Error()
		}
		return AuthRequest{}, ref
	}
	redirectURI, resource, state := q.Get("redirect_uri"), q.Get("resource"), q.Get("state")
	var reason string
	switch {
	case redirectURI == "":
		reason = "redirect_uri is required"
	case !doc.allows(redirectURI):
		reason = "redirect_uri must be one of the redirect_uris of the client's document"
	case q.Get("response_type") != "code":
		reason = "response_type must be code"
	case !validChallenge(q.Get("code_challenge")):
		reason = "code_challenge must be a PKCE challenge made with S256: 43 base64url characters"
	case q.Get("code_challenge_method") != "S256":
		reason = "code_challenge_method must be S256"
	case !slices.Contains(s.resources, resource):
		reason = "resource must be an endpoint of this faucet: " + strings.Join(s.resources, ", ")
	case len(state) > maxClientStateBytes:
		reason = fmt.Sprintf("state must be at most %d bytes", maxClientStateBytes)
	}
	if reason != "" {
		return AuthRequest{}, badAuthorization(reason)
	}
	scopes, err := grantedScopes(q, faucet.AllScopes())
	if err != nil {
		return AuthRequest{}, badAuthorization("scope must name at least one scope of faucet, or be left out to ask for all of them")
	}
	return AuthRequest{
		ClientID: clientID, ClientName: doc.ClientName, RedirectURI: redirectURI, ClientState: state,
		Resource: resource, Scopes: scopes, CodeChallenge: q.Get("code_challenge"),
	}, nil
}

// consent takes the form of the consent page. It goes on only when the
// browser that posts it is the one that got the page: the Origin is faucet's,
// the form token belongs to the request's cookie, and the sealed request
// opens with that cookie. Only then does it store the request. Any other
// post stores nothing, so a page of another site that posts the form makes
// nothing.
func (s *Server) consent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	form, err := readConsentForm(w, r)
	var browser string
	if err == nil {
		browser, err = s.checkConsent(r, form)
	}
	id := form.Get("request")
	var request AuthRequest
	if err == nil {
		request, err = s.login.keys.openRequest(form.Get("login"), id, HashToken(browser))
	}
	if err != nil {
		s.refuseConsent(w, r, form, err)
		return
	}
	g, err := newSecret("")
	if err != nil {
		s.refuse(w, r, failure(err), slog.F("request_id", id))
		return
	}
	state := derive(githubStateLabel, g)
	request.GitHubStateHash = HashToken(state)
	err = s.store.ConsentAuthRequest(ctx, request, s.clock.Now())
	switch {
	case xerrors.Is(err, ErrUnknownAuthRequest):
		s.refuseConsent(w, r, form, xerrors.New("the request has expired, or it went on in another state"))
		return
	case err != nil:
		s.refuse(w, r, failure(xerrors.Errorf("record the consent: %w", err)), slog.F("request_id", id))
		return
	}
	http.SetCookie(w, loginCookie(githubCookieName(id), g, http.SameSiteLaxMode))
	s.logger.Debug(ctx, "consent given, browser sent to GitHub", slog.F("request_id", id))
	w.Header().Set("Location", s.login.github.authorizeURL(s.issuer+GitHubCallbackPath, state, s256(derive(githubPKCELabel, g))))
	w.WriteHeader(http.StatusSeeOther)
}

// refuseConsent refuses a consent. It stores nothing, since anyone can post
// the form.
func (s *Server) refuseConsent(w http.ResponseWriter, r *http.Request, form url.Values, reason error) {
	var ids []string
	for _, id := range form["request"] {
		if wellFormedID(id) {
			ids = append(ids, id)
		}
	}
	s.refuse(w, r, forbiddenConsent(reason.Error()), slog.F("request_id", ids))
}

// readConsentForm reads the body of a consent: a form of at most 16 KiB.
func readConsentForm(w http.ResponseWriter, r *http.Request) (url.Values, error) {
	if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/x-www-form-urlencoded" {
		return nil, xerrors.New("the body is not a form")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxConsentFormBytes))
	if err != nil {
		return nil, xerrors.New("the form could not be read, or is over 16 KiB")
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, xerrors.New("the form does not parse")
	}
	return form, nil
}

// checkConsent returns the value of the request's cookie when the browser
// that posted form is the one that got the consent page.
func (s *Server) checkConsent(r *http.Request, form url.Values) (string, error) {
	for _, values := range form {
		if len(values) > 1 {
			return "", xerrors.New("a field of the form appears more than once")
		}
	}
	id := form.Get("request")
	if !wellFormedID(id) {
		return "", xerrors.New("the form names no well-formed request")
	}
	if origins := r.Header.Values("Origin"); len(origins) != 1 || origins[0] != s.issuer {
		return "", xerrors.New("the Origin header is not faucet's issuer")
	}
	cookie, err := r.Cookie(browserCookieName(id))
	if err != nil || cookie.Value == "" {
		return "", xerrors.New("the browser sent no cookie of the request")
	}
	if subtle.ConstantTimeCompare([]byte(form.Get("token")), []byte(derive(consentFormLabel, cookie.Value))) != 1 {
		return "", xerrors.New("the form token does not belong to the browser's cookie")
	}
	return cookie.Value, nil
}

// GitHubCallbackHandler serves GitHubCallbackPath, where GitHub sends the
// browser back to after the user logged in there. It goes on only in the
// browser that pressed Continue, because GitHub's state derives from that
// browser's cookie. The client then gets a code, or access_denied when the
// user refused at GitHub; these are the only redirects to a client.
func (s *Server) GitHubCallbackHandler() http.Handler {
	if s.login == nil {
		return http.NotFoundHandler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setLoginHeaders(w)
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			s.refuse(w, r, wrongMethod(r.Method))
			return
		}
		s.callback(w, r)
	})
}

func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	state := q.Get("state")
	request, g, ref := s.callbackLogin(r, state)
	fields := []slog.Field{slog.F("request_id", request.ID), slog.F("client_host", clientHost(request.ClientID))}
	if ref != nil {
		s.refuse(w, r, ref, fields...)
		return
	}
	answer := url.Values{}
	if q.Has("error") {
		if err := s.store.ExpireAuthRequest(ctx, request.ID); err != nil {
			s.refuse(w, r, failure(xerrors.Errorf("end the refused login: %w", err)), fields...)
			return
		}
		s.logger.Info(ctx, "the user refused the login at GitHub", append(fields, slog.F("github_error", githubErrorOf(q.Get("error"))))...)
		answer.Set("error", "access_denied")
	} else {
		code, ref := s.approve(ctx, request, state, q.Get("code"), g)
		if ref != nil {
			s.refuse(w, r, ref, fields...)
			return
		}
		s.logger.Info(ctx, "browser login approved", fields...)
		answer.Set("code", code)
	}
	if request.ClientState != "" {
		answer.Set("state", request.ClientState)
	}
	answer.Set("iss", s.issuer)
	http.SetCookie(w, endedCookie(browserCookieName(request.ID), http.SameSiteStrictMode))
	http.SetCookie(w, endedCookie(githubCookieName(request.ID), http.SameSiteLaxMode))
	w.Header().Set("Location", withQuery(request.RedirectURI, answer))
	w.WriteHeader(http.StatusFound)
}

// callbackLogin returns the pending login that GitHub's state names, and the
// value of its GitHub cookie, when this browser is the one that pressed
// Continue.
func (s *Server) callbackLogin(r *http.Request, state string) (AuthRequest, string, *refusal) {
	if state == "" {
		return AuthRequest{}, "", unknownLogin("GitHub sent no state")
	}
	request, err := s.store.AuthRequestByGitHubState(r.Context(), HashToken(state))
	switch {
	case xerrors.Is(err, ErrUnknownAuthRequest):
		return AuthRequest{}, "", unknownLogin("no request holds this state")
	case err != nil:
		return AuthRequest{}, "", failure(xerrors.Errorf("read the request of a GitHub state: %w", err))
	case request.State != AuthRequestPending:
		return request, "", unknownLogin("the request is " + string(request.State))
	case !request.ExpiresAt.After(s.clock.Now()):
		return request, "", unknownLogin("the request expired")
	}
	cookie, err := r.Cookie(githubCookieName(request.ID))
	if err != nil || subtle.ConstantTimeCompare([]byte(derive(githubStateLabel, cookie.Value)), []byte(state)) != 1 {
		return request, "", wrongBrowser("the browser's GitHub cookie does not belong to the state")
	}
	return request, cookie.Value, nil
}

// approve trades GitHub's code for the user's tokens, keeps them sealed in
// the user's connection, and moves the request to approved with a new code
// for the client, which it returns. A failure leaves the request pending.
// The user's other sessions stay only when GitHub still takes the pair that
// the new one replaces, so that the new pair serves no session of an
// authorization that the user revoked at GitHub.
func (s *Server) approve(ctx context.Context, request AuthRequest, state, githubCode, g string) (string, *refusal) {
	if githubCode == "" {
		return "", githubFailure(http.StatusBadRequest, xerrors.New("GitHub sent the browser back with neither a code nor an error"))
	}
	tokens, err := s.login.github.exchange(ctx, githubCode, s.issuer+GitHubCallbackPath, derive(githubPKCELabel, g))
	switch {
	case xerrors.Is(err, errTokensDoNotExpire):
		return "", &refusal{status: http.StatusBadGateway, err: err, page: errorPage{
			Title:   "faucet cannot keep this login",
			Message: "faucet's GitHub App gives tokens that never expire, and faucet keeps none of those. Tell the person who runs faucet; a new login works once they change the App.",
		}}
	case err != nil:
		return "", githubFailure(http.StatusBadGateway, xerrors.Errorf("trade GitHub's code: %w", err))
	}
	user, err := s.login.github.user(ctx, tokens.access)
	if err != nil {
		return "", githubFailure(http.StatusBadGateway, xerrors.Errorf("ask GitHub who logged in: %w", err))
	}
	now := s.clock.Now()
	connection := Connection{
		ID: ConnectionID(ProviderGitHub, user.id), Provider: ProviderGitHub, UpstreamUserID: user.id, Login: user.login,
		AccessExpiresAt: now.Add(tokens.accessLifetime), RefreshExpiresAt: now.Add(tokens.refreshLifetime), UpdatedAt: now,
	}
	if err := s.login.keys.seal(&connection, tokens.access, tokens.refresh); err != nil {
		return "", failure(xerrors.Errorf("seal the GitHub tokens: %w", err))
	}
	code, err := newSecret("")
	if err != nil {
		return "", failure(err)
	}
	err = s.store.ApproveAuthRequest(ctx, Approval{
		RequestID: request.ID, GitHubState: HashToken(state), Connection: connection,
		Code: HashToken(code), Now: now, ExpiresAt: now.Add(codeLifetime),
	}, func(current Connection) bool {
		err := s.access.tokens.authorized(ctx, current)
		if err != nil {
			s.logger.Info(ctx, "GitHub did not take the tokens that a login replaces, the sessions of the connection end", slog.F("connection_id", current.ID), slog.Error(err))
		}
		return err == nil
	})
	switch {
	case xerrors.Is(err, ErrUnknownAuthRequest):
		return "", unknownLogin("the request was no longer pending with this state, or expired, at its approval")
	case err != nil:
		return "", failure(xerrors.Errorf("approve the request: %w", err))
	}
	return code, nil
}

// githubErrorOf is GitHub's error code for the log, when it looks like one.
func githubErrorOf(code string) string {
	if githubErrorCode.MatchString(code) {
		return code
	}
	return "unrecognized"
}

// withQuery appends params to the query of uri as the client wrote it, so
// the client finds its own query byte for byte.
func withQuery(uri string, params url.Values) string {
	switch {
	case !strings.Contains(uri, "?"):
		return uri + "?" + params.Encode()
	case strings.HasSuffix(uri, "?"), strings.HasSuffix(uri, "&"):
		return uri + params.Encode()
	}
	return uri + "&" + params.Encode()
}

// wellFormedID reports whether id has the form that newID makes, so it can
// name a cookie and a row.
func wellFormedID(id string) bool {
	if len(id) != base64.RawURLEncoding.EncodedLen(idBytes) {
		return false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(id)
	return err == nil && len(b) == idBytes
}

// validChallenge reports whether c is a PKCE challenge made with S256: a
// SHA-256 in base64url without padding.
func validChallenge(c string) bool {
	if len(c) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
		return false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(c)
	return err == nil && len(b) == sha256.Size
}

// clientHost returns the host of a client ID, with its port, or "" when it
// is not a URL.
func clientHost(clientID string) string {
	u, err := url.Parse(clientID)
	if err != nil {
		return ""
	}
	return u.Host
}

func browserCookieName(id string) string { return browserCookiePrefix + id }

func githubCookieName(id string) string { return browserCookiePrefix + id + githubCookieSuffix }

// endedCookie deletes the cookie of a login named name.
func endedCookie(name string, sameSite http.SameSite) *http.Cookie {
	c := loginCookie(name, "", sameSite)
	c.MaxAge = -1
	return c
}

// loginCookie is a cookie of a login, which lasts as long as its request.
// The __Host- prefix makes a browser take it only from faucet's own origin
// over https, for the whole origin.
func loginCookie(name, value string, sameSite http.SameSite) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: int(authRequestLifetime / time.Second),
		Secure: true, HttpOnly: true, SameSite: sameSite,
	}
}
