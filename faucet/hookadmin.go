package faucet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"golang.org/x/xerrors"
)

const (
	deliveriesPerPage = 100
	maxTokenBytes     = 1024
	maxCursorBytes    = 512
	maxLinkBytes      = 8 << 10
)

var nextLink = regexp.MustCompile(`<([^>]*)>\s*;\s*rel="?next"?`)

// HookDelivery is one attempt by GitHub to deliver a webhook to faucet.
type HookDelivery struct {
	// ID identifies this attempt. GitHub's redelivery endpoint takes it.
	ID int64
	// GUID is the X-GitHub-Delivery header. Every attempt at one event has the
	// same GUID, and a stored Event carries it as DeliveryID.
	GUID        string
	DeliveredAt time.Time
	// StatusCode is the HTTP status that faucet answered with. It is 0 when
	// GitHub got no HTTP response.
	StatusCode int
	// Redelivery is true when this attempt answers a redelivery request. Such
	// an attempt has its own, later DeliveredAt, so only the attempts with
	// Redelivery false carry the time at which the event first happened.
	Redelivery bool
}

// DeliveryPage is one page of HookAdmin.ListDeliveries.
type DeliveryPage struct {
	// Deliveries are the attempts on this page, newest first.
	Deliveries []HookDelivery
	// Next is the cursor of the next, older page. It is empty on the last page.
	Next string
}

// HookAdmin is the part of GitHub's webhook API that Service.Redeliver needs.
// A token that can call these two methods can usually also edit or delete the
// webhook, so an implementation must call nothing else.
type HookAdmin interface {
	// ListDeliveries returns one page of delivery attempts, newest first.
	// cursor is empty for the first page and a DeliveryPage.Next after that.
	ListDeliveries(ctx context.Context, cursor string) (DeliveryPage, error)
	// Redeliver asks GitHub to send the attempt with this ID again.
	Redeliver(ctx context.Context, attemptID int64) error
}

// HookTarget names the webhook that GitHubHookAdmin manages.
type HookTarget struct {
	// Org is the organization that owns the webhook. Set one of Org and Repo.
	Org string
	// Repo is the owner/name of the repository that owns the webhook.
	Repo string
	// HookID is the webhook's numeric ID.
	HookID int64
}

// GitHubHookAdmin implements HookAdmin with GitHub's REST API. It calls
//
//	GET  {base}/orgs/{org}/hooks/{id}/deliveries
//	POST {base}/orgs/{org}/hooks/{id}/deliveries/{attempt}/attempts
//
// or the same two paths under /repos/{owner}/{name}, and nothing else. It sends
// its token only to the base URL: a redirect to another host fails the call.
// Every fmt verb prints no token.
type GitHubHookAdmin struct {
	base     *url.URL
	token    string
	hookPath string
	client   *http.Client
}

var _ HookAdmin = GitHubHookAdmin{}

// NewGitHubHookAdmin returns a GitHubHookAdmin. baseURL follows the rules of
// TokenAuthorizer.BaseURL. token must be a valid header value, which GitHub
// tokens are. client defaults to http.DefaultClient and is never modified. No
// error carries the token or the base URL.
func NewGitHubHookAdmin(baseURL, token string, target HookTarget, client *http.Client) (GitHubHookAdmin, error) {
	base, err := parseBaseURL(baseURL)
	if err != nil {
		return GitHubHookAdmin{}, err
	}
	if !validHeaderToken(token) {
		return GitHubHookAdmin{}, xerrors.Errorf("hook token must be 1 to %d printable ASCII characters without spaces", maxTokenBytes)
	}
	if target.HookID <= 0 {
		return GitHubHookAdmin{}, xerrors.New("hook ID must be positive")
	}
	var scope string
	switch {
	case target.Org != "" && target.Repo != "":
		return GitHubHookAdmin{}, xerrors.New("set the hook's org or its repo, not both")
	case target.Org != "":
		if !ownerPattern.MatchString(target.Org) {
			return GitHubHookAdmin{}, xerrors.New("hook org is not a valid organization name")
		}
		scope = "/orgs/" + url.PathEscape(target.Org)
	case target.Repo != "":
		owner, name, err := parseRepo(target.Repo)
		if err != nil {
			return GitHubHookAdmin{}, xerrors.New("hook repo must be written owner/name")
		}
		scope = "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	default:
		return GitHubHookAdmin{}, xerrors.New("the hook's org or its repo is required")
	}
	hookPath := scope + "/hooks/" + strconv.FormatInt(target.HookID, 10)
	return GitHubHookAdmin{base: base, token: token, hookPath: hookPath, client: client}, nil
}

func validHeaderToken(token string) bool {
	if token == "" || len(token) > maxTokenBytes {
		return false
	}
	for i := range len(token) {
		if token[i] <= ' ' || token[i] > '~' {
			return false
		}
	}
	return true
}

// Format implements fmt.Formatter so that no verb can print the token.
func (a GitHubHookAdmin) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "GitHubHookAdmin{%s}", a.hookPath)
}

// deliveryJSON has the fields of HookDelivery, in the same order, so that
// HookDelivery(d) converts it.
type deliveryJSON struct {
	ID          int64     `json:"id"`
	GUID        string    `json:"guid"`
	DeliveredAt time.Time `json:"delivered_at"`
	StatusCode  int       `json:"status_code"`
	Redelivery  bool      `json:"redelivery"`
}

// ListDeliveries implements HookAdmin. It reads the rel="next" cursor from the
// Link header and never follows the URL in it.
func (a GitHubHookAdmin) ListDeliveries(ctx context.Context, cursor string) (DeliveryPage, error) {
	query := url.Values{"per_page": {strconv.Itoa(deliveriesPerPage)}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	resp, err := callGitHub(ctx, a.client, a.base, a.token, http.MethodGet, a.hookPath+"/deliveries", query.Encode())
	if err != nil {
		return DeliveryPage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAPIResponseBytes))
		return DeliveryPage{}, xerrors.Errorf("GitHub answered the delivery list with status %d", resp.StatusCode)
	}
	var raw []deliveryJSON
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIResponseBytes)).Decode(&raw); err != nil {
		return DeliveryPage{}, xerrors.Errorf("decode GitHub deliveries: %w", err)
	}
	next, err := nextCursor(resp.Header.Get("Link"))
	if err != nil {
		return DeliveryPage{}, err
	}
	page := DeliveryPage{Deliveries: make([]HookDelivery, len(raw)), Next: next}
	for i, d := range raw {
		if d.ID <= 0 || d.GUID == "" || d.DeliveredAt.IsZero() {
			return DeliveryPage{}, xerrors.New("GitHub returned a delivery without an ID, GUID or time")
		}
		page.Deliveries[i] = HookDelivery(d)
	}
	return page, nil
}

// Redeliver implements HookAdmin with POST .../deliveries/{id}/attempts.
func (a GitHubHookAdmin) Redeliver(ctx context.Context, attemptID int64) error {
	if attemptID <= 0 {
		return xerrors.New("attempt ID must be positive")
	}
	path := a.hookPath + "/deliveries/" + strconv.FormatInt(attemptID, 10) + "/attempts"
	resp, err := callGitHub(ctx, a.client, a.base, a.token, http.MethodPost, path, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAPIResponseBytes))
	if resp.StatusCode != http.StatusAccepted {
		return xerrors.Errorf("GitHub answered the redelivery request with status %d", resp.StatusCode)
	}
	return nil
}

// nextCursor takes the cursor out of the rel="next" link. The link's URL is
// not used for anything else, because it comes from the network.
func nextCursor(link string) (string, error) {
	if len(link) > maxLinkBytes {
		return "", xerrors.New("GitHub sent a Link header that is too long")
	}
	m := nextLink.FindStringSubmatch(link)
	if m == nil {
		return "", nil
	}
	u, err := url.Parse(m[1])
	if err != nil {
		return "", xerrors.New("GitHub sent a next link that is not a URL")
	}
	cursor := u.Query().Get("cursor")
	if cursor == "" || len(cursor) > maxCursorBytes {
		return "", xerrors.New("GitHub sent a next link without a usable cursor")
	}
	for i := range len(cursor) {
		if cursor[i] < ' ' || cursor[i] == 0x7f {
			return "", xerrors.New("GitHub sent a cursor with a control character")
		}
	}
	return cursor, nil
}
