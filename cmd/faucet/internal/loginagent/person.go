package main

import (
	"context"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/xerrors"
)

const maxPageBytes = 64 << 10

var (
	formAction  = regexp.MustCompile(`<form method="post" action="([^"]+)">`)
	hiddenField = regexp.MustCompile(`<input type="hidden" name="([a-z]+)" value="([^"]*)">`)
	pageHeading = regexp.MustCompile(`<h1>([^<]*)</h1>`)
)

// person is the person at the browser of the agent's login. It opens the
// authorization URL, reads the consent page, presses Continue, and follows
// the browser through GitHub and back until faucet sends it to the agent's
// redirect URI, whose query it hands to go-sdk. Nothing listens at the
// redirect URI: the browser stops at the redirect that names it. GitHub is
// the stub, at which the person authorized faucet's App before, so it sends
// the browser straight back.
type person struct {
	browser     *http.Client
	redirectURI *url.URL
	logins      int
}

func newPerson(redirectURI string) (*person, error) {
	back, err := url.Parse(redirectURI)
	if err != nil || back.Host == "" {
		return nil, xerrors.New("-redirect-uri must be an absolute URL")
	}
	p := &person{redirectURI: back}
	p.browser = &http.Client{
		Jar:     &loopbackJar{hosts: map[string]map[string]string{}},
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			switch {
			case sameEndpoint(req.URL, back):
				return http.ErrUseLastResponse
			case len(via) >= 10:
				return xerrors.New("the login redirected 10 times")
			}
			return nil
		},
	}
	return p, nil
}

func (p *person) login(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
	p.logins++
	consent, err := p.open(ctx, http.MethodGet, args.URL, nil, "")
	if err != nil {
		return nil, err
	}
	action, form := formAction.FindStringSubmatch(consent.body), url.Values{}
	for _, field := range hiddenField.FindAllStringSubmatch(consent.body, -1) {
		form.Set(field[1], html.UnescapeString(field[2]))
	}
	if consent.status != http.StatusOK || action == nil || len(form) != 3 {
		return nil, xerrors.Errorf("the consent page answered %d %q", consent.status, consent.heading())
	}
	target, err := consent.url.Parse(html.UnescapeString(action[1]))
	if err != nil {
		return nil, xerrors.Errorf("the consent form's action: %w", err)
	}
	origin := consent.url.Scheme + "://" + consent.url.Host
	back, err := p.open(ctx, http.MethodPost, target.String(), strings.NewReader(form.Encode()), origin)
	if err != nil {
		return nil, err
	}
	location, err := url.Parse(back.location)
	if back.status != http.StatusFound || err != nil || !sameEndpoint(location, p.redirectURI) {
		return nil, xerrors.Errorf("the login ended at %d %s %q", back.status, back.url.Path, back.heading())
	}
	q := location.Query()
	return &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}, nil
}

// page is the answer that a request of the browser ends at.
type page struct {
	status   int
	url      *url.URL
	location string
	body     string
}

// open makes a request of the browser, with the Origin that a browser sends
// on a form post, and reads the page that it ends at.
func (p *person) open(ctx context.Context, method, target string, form io.Reader, origin string) (page, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, form)
	if err != nil {
		return page{}, xerrors.Errorf("build the request: %w", err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", origin)
	}
	resp, err := p.browser.Do(req)
	if err != nil {
		return page{}, xerrors.Errorf("%s %s: %w", method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes))
	if err != nil {
		return page{}, xerrors.Errorf("read the page: %w", err)
	}
	return page{status: resp.StatusCode, url: resp.Request.URL, location: resp.Header.Get("Location"), body: string(body)}, nil
}

// heading is the title of a page of faucet's login, which says what went
// wrong on an error page.
func (p page) heading() string {
	if m := pageHeading.FindStringSubmatch(p.body); m != nil {
		return html.UnescapeString(m[1])
	}
	return ""
}

func sameEndpoint(u, endpoint *url.URL) bool {
	return u.Scheme == endpoint.Scheme && u.Host == endpoint.Host && u.Path == endpoint.Path
}

// loopbackJar keeps the cookies of each host the way Chromium does on
// http://127.0.0.1, which it counts as a secure origin, so it keeps and
// sends Secure and __Host- cookies there. Go's cookiejar sends a Secure
// cookie over https only. Every cookie of faucet's login has Path=/ and no
// Domain, and every host of the tests is on one site, so the jar keeps
// neither path nor SameSite.
type loopbackJar struct {
	mu    sync.Mutex
	hosts map[string]map[string]string
}

func (j *loopbackJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	jar := j.hosts[u.Hostname()]
	if jar == nil {
		jar = map[string]string{}
		j.hosts[u.Hostname()] = jar
	}
	for _, c := range cookies {
		if c.MaxAge < 0 {
			delete(jar, c.Name)
		} else {
			jar[c.Name] = c.Value
		}
	}
}

func (j *loopbackJar) Cookies(u *url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	var cookies []*http.Cookie
	for name, value := range j.hosts[u.Hostname()] {
		cookies = append(cookies, &http.Cookie{Name: name, Value: value})
	}
	return cookies
}
