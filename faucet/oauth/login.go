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

	maxConsentFormBytes = 4 << 10
	maxClientStateBytes = 2048
)

// refusal is a step of a browser login that does not go on: the page that
// the browser gets, and the reason that the log gets.
type refusal struct {
	status int
	page   errorPage
	reason string
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
		Message: "faucet cannot tell that this browser started this login, so it ended the login. Go back to your agent and start the login again.",
	}}
}

func (s *Server) refuse(w http.ResponseWriter, r *http.Request, ref *refusal, fields ...slog.Field) {
	fields = append(fields, slog.F("path", r.URL.Path), slog.F("status", ref.status), slog.F("reason", ref.reason))
	s.logger.Info(r.Context(), "browser login refused", fields...)
	writePage(w, ref.status, "error", ref.page)
}

// fail answers a step that faucet could not finish. err holds no secret.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error, fields ...slog.Field) {
	fields = append(fields, slog.F("path", r.URL.Path), slog.Error(err))
	s.logger.Error(r.Context(), "browser login failed", fields...)
	writePage(w, http.StatusInternalServerError, "error", errorPage{
		Title:   "Something went wrong",
		Message: "faucet could not finish this step of the login. Go back to your agent and start the login again.",
	})
}

// authorizeHandler serves the authorization endpoint of the browser login:
// GET shows the consent page, and POST is its form. It never redirects to
// a client, because nothing yet proves that the browser is the user's.
//
//nolint:revive // AuthorizeHandler serves this handler when the browser login is on.
func (s *Server) authorizeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setLoginHeaders(w)
		switch r.Method {
		case http.MethodGet:
			s.authorize(w, r)
		case http.MethodPost:
			s.consent(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			s.refuse(w, r, &refusal{status: http.StatusMethodNotAllowed, reason: "method " + r.Method, page: errorPage{
				Title: "This login cannot start", Message: "faucet's login page takes a link from your agent. Go back to your agent and start the login again.",
			}})
		}
	})
}

// authorize checks an authorization request, stores it, gives the browser
// the request's cookie and shows the consent page.
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
		s.fail(w, r, err, host)
		return
	}
	browser, err := newSecret("")
	if err != nil {
		s.fail(w, r, err, host)
		return
	}
	now := s.clock.Now()
	request.ID, request.State, request.BrowserHash = id, AuthRequestPending, HashToken(browser)
	request.CreatedAt, request.ExpiresAt = now, now.Add(authRequestLifetime)
	if err := s.store.InsertAuthRequest(ctx, request); err != nil {
		s.fail(w, r, xerrors.Errorf("store the authorization request: %w", err), host)
		return
	}
	http.SetCookie(w, loginCookie(browserCookieName(id), browser, http.SameSiteStrictMode))
	s.logger.Debug(ctx, "consent page shown", host, slog.F("request_id", id))
	writePage(w, http.StatusOK, "consent", newConsentPage(request, derive(consentFormLabel, browser)))
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
	}
	doc, err := s.login.documents.fetch(ctx, clientID)
	if err != nil {
		return AuthRequest{}, badAuthorization(err.Error())
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
// and the form token belongs to the request's cookie. Any other post ends
// the login, so a page of another site that posts the form makes nothing.
func (s *Server) consent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	form, err := readConsentForm(w, r)
	var browser string
	if err == nil {
		browser, err = s.checkConsent(r, form)
	}
	id := form.Get("request")
	if err != nil {
		s.endConsent(w, r, form, err)
		return
	}
	g, err := newSecret("")
	if err != nil {
		s.fail(w, r, err, slog.F("request_id", id))
		return
	}
	state := derive(githubStateLabel, g)
	err = s.store.ConsentAuthRequest(ctx, id, HashToken(browser), HashToken(state), s.clock.Now())
	switch {
	case xerrors.Is(err, ErrUnknownAuthRequest):
		s.endConsent(w, r, form, xerrors.New("no pending request of this browser has this ID, or it has expired"))
		return
	case err != nil:
		s.fail(w, r, xerrors.Errorf("record the consent: %w", err), slog.F("request_id", id))
		return
	}
	http.SetCookie(w, loginCookie(githubCookieName(id), g, http.SameSiteLaxMode))
	s.logger.Debug(ctx, "consent given, browser sent to GitHub", slog.F("request_id", id))
	w.Header().Set("Location", s.login.github.authorizeURL(s.issuer+GitHubCallbackPath, state, s256(derive(githubPKCELabel, g))))
	w.WriteHeader(http.StatusSeeOther)
}

// endConsent refuses a consent and ends every request that its form names.
func (s *Server) endConsent(w http.ResponseWriter, r *http.Request, form url.Values, reason error) {
	var ids []string
	for _, id := range form["request"] {
		if !wellFormedID(id) {
			continue
		}
		ids = append(ids, id)
		if err := s.store.ExpireAuthRequest(r.Context(), id); err != nil {
			s.logger.Error(r.Context(), "could not end a refused login", slog.F("request_id", id), slog.Error(err))
		}
	}
	s.refuse(w, r, forbiddenConsent(reason.Error()), slog.F("request_id", ids))
}

// readConsentForm reads the body of a consent: a form of at most 4 KiB.
func readConsentForm(w http.ResponseWriter, r *http.Request) (url.Values, error) {
	if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/x-www-form-urlencoded" {
		return nil, xerrors.New("the body is not a form")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxConsentFormBytes))
	if err != nil {
		return nil, xerrors.New("the form could not be read, or is over 4 KiB")
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
// browser back to after the user logged in there.
func (s *Server) GitHubCallbackHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not built yet", http.StatusNotImplemented)
	})
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

// loginCookie is a cookie of a login, which lasts as long as its request.
// The __Host- prefix makes a browser take it only from faucet's own origin
// over https, for the whole origin.
func loginCookie(name, value string, sameSite http.SameSite) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: int(authRequestLifetime / time.Second),
		Secure: true, HttpOnly: true, SameSite: sameSite,
	}
}
