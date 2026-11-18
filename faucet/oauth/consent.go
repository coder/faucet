package oauth

import (
	"bytes"
	"html/template"
	"net/http"
	"time"

	"github.com/coder/faucet/faucet"
)

// pageCSP lets a page of the browser login load nothing and run no script.
// It names no form-action, because Chrome applies form-action to the
// redirect that follows the consent form, and that redirect goes to GitHub.
const pageCSP = "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'"

// scopeDescriptions say in plain words what each scope lets a client do.
var scopeDescriptions = map[faucet.Scope]string{
	"events:read":      "wait for and read the GitHub events (CI results, reviews, comments) of the repositories that you can see",
	"events:subscribe": "have faucet send those events to a web address that the app chooses",
}

// setLoginHeaders sets the headers of every answer of the browser login. No
// cache keeps one, no other site frames a page, and Referrer-Policy
// same-origin keeps the real Origin on the consent form: no-referrer makes a
// browser send Origin: null there.
func setLoginHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", pageCSP)
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Content-Type-Options", "nosniff")
}

type consentScope struct {
	Name, Description string
}

// consentPage is what the consent page shows. Every field but RequestID and
// FormToken comes from the client or its document, and the template escapes
// each one.
type consentPage struct {
	ClientHost   string
	ClientName   string
	ClientID     string
	Resource     string
	Scopes       []consentScope
	RedirectURI  string
	Action       string
	RequestID    string
	FormToken    string
	Minutes      int
	TokenMinutes int
}

func newConsentPage(r AuthRequest, formToken string) consentPage {
	p := consentPage{
		ClientHost: clientHost(r.ClientID), ClientName: r.ClientName, ClientID: r.ClientID, Resource: r.Resource,
		RedirectURI: r.RedirectURI, Action: AuthorizePath, RequestID: r.ID, FormToken: formToken,
		Minutes: int(authRequestLifetime / time.Minute), TokenMinutes: int(accessTokenLifetime / time.Minute),
	}
	for _, s := range r.Scopes {
		description, ok := scopeDescriptions[s]
		if !ok {
			description = "a permission that faucet has not described"
		}
		p.Scopes = append(p.Scopes, consentScope{Name: string(s), Description: description})
	}
	return p
}

// errorPage says what happened and what the person should do. Detail is
// for the developer of a client, and is empty when the reason would help
// only an attacker.
type errorPage struct {
	Title   string
	Message string
	Detail  string
}

var pages = template.Must(template.New("").Parse(`
{{define "top"}}<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.}} · faucet</title>
<style>
:root { color-scheme: light dark; --fg: #1f2328; --muted: #59636e; --line: #d1d9e0; --card: #ffffff; --bg: #f6f8fa; --button: #1f883d; }
@media (prefers-color-scheme: dark) { :root { --fg: #f0f6fc; --muted: #9198a1; --line: #3d444d; --card: #151b23; --bg: #0d1117; --button: #238636; } }
* { box-sizing: border-box; }
body { margin: 0; padding: 2rem 1rem; background: var(--bg); color: var(--fg); font: 16px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif; }
main { max-width: 36rem; margin: 0 auto; padding: 2rem; background: var(--card); border: 1px solid var(--line); border-radius: 12px; }
.eyebrow { margin: 0; color: var(--muted); font-size: .875rem; text-transform: uppercase; letter-spacing: .05em; }
h1 { margin: .25rem 0 0; font-size: 2.25rem; line-height: 1.2; overflow-wrap: anywhere; }
.says { margin: .25rem 0 1.5rem; font-size: 1.125rem; }
dl { margin: 0 0 1.5rem; }
dt { margin-top: 1rem; font-weight: 600; }
dd { margin: .25rem 0 0; }
ul { margin: 0; padding-left: 1.25rem; }
code { font: .9rem ui-monospace, SFMono-Regular, Menlo, monospace; overflow-wrap: anywhere; }
button { width: 100%; padding: .75rem 1rem; border: 0; border-radius: 8px; background: var(--button); color: #fff; font: inherit; font-weight: 600; cursor: pointer; }
.note { color: var(--muted); font-size: .875rem; }
</style>
</head>
<body>
<main>
{{end}}

{{define "bottom"}}</main>
</body>
</html>
{{end}}

{{define "consent"}}{{template "top" "Log in"}}
<p class="eyebrow">faucet login</p>
<h1>{{.ClientHost}}</h1>
{{if .ClientName}}<p class="says">says it is <strong>{{.ClientName}}</strong>. faucet has not checked this name.</p>
{{else}}<p class="says">gives no name.</p>
{{end}}
<p>This app asks to use faucet in your name.</p>
<dl>
<dt>Endpoint</dt>
<dd><code>{{.Resource}}</code></dd>
<dt>It may</dt>
<dd><ul>
{{range .Scopes}}<li><code>{{.Name}}</code>: {{.Description}}</li>
{{end}}</ul></dd>
<dt>Your browser then returns to</dt>
<dd><code>{{.RedirectURI}}</code></dd>
<dt>Client ID</dt>
<dd><code>{{.ClientID}}</code></dd>
</dl>
<p>Continue takes you to GitHub to log in. faucet keeps the GitHub token that it gets there for you, sealed, and gives this app only its own faucet token, which lasts {{.TokenMinutes}} minutes at a time. The app never sees your GitHub token.</p>
<form method="post" action="{{.Action}}">
<input type="hidden" name="request" value="{{.RequestID}}">
<input type="hidden" name="token" value="{{.FormToken}}">
<button type="submit">Continue with GitHub</button>
</form>
<p class="note">If you did not just start a login from your agent, close this page. This page works for {{.Minutes}} minutes.</p>
{{template "bottom"}}{{end}}

{{define "error"}}{{template "top" .Title}}
<p class="eyebrow">faucet login</p>
<h1>{{.Title}}</h1>
<p>{{.Message}}</p>
{{if .Detail}}<p class="note">Details for the app's developer: {{.Detail}}</p>
{{end}}{{template "bottom"}}{{end}}
`))

// writePage renders the page name with data and sends it with status. A
// page that does not render is a plain 500.
func writePage(w http.ResponseWriter, status int, name string, data any) {
	var body bytes.Buffer
	if err := pages.ExecuteTemplate(&body, name, data); err != nil {
		http.Error(w, "faucet could not show this page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body.Bytes())
}
