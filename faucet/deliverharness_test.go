package faucet_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	standardwebhooks "github.com/standard-webhooks/standard-webhooks/libraries/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

type hookRequest struct {
	Path   string
	Header http.Header
	Body   []byte
}

// hookReceiver plays the endpoint behind a callback URL. It answers 200 unless
// respond says otherwise for the path.
type hookReceiver struct {
	t      *testing.T
	server *httptest.Server

	mu      sync.Mutex
	got     []hookRequest
	respond map[string]func(n int) int
	delay   time.Duration
	padded  map[string]bool
	// envelopes answers a control envelope by path and type, ahead of respond.
	envelopes map[string]func() int
}

func newHookReceiver(t *testing.T) *hookReceiver {
	t.Helper()
	r := &hookReceiver{t: t, respond: map[string]func(int) int{}, padded: map[string]bool{}}
	r.server = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.server.Close)
	return r
}

func (r *hookReceiver) serve(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	r.got = append(r.got, hookRequest{Path: req.URL.Path, Header: req.Header.Clone(), Body: body})
	n := 0
	for _, g := range r.got {
		if g.Path == req.URL.Path {
			n++
		}
	}
	respond, delay, padded := r.respond[req.URL.Path], r.delay, r.padded[req.URL.Path]
	var typed struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(body, &typed)
	envelope := r.envelopes[req.URL.Path+" "+typed.Type]
	r.mu.Unlock()
	time.Sleep(delay)
	if padded {
		w.Header().Set("X-Padding", strings.Repeat("x", 20<<10))
	}

	status := http.StatusOK
	switch {
	case envelope != nil:
		status = envelope()
	case respond != nil:
		status = respond(n)
	}
	if status == http.StatusMovedPermanently {
		w.Header().Set("Location", "/elsewhere")
	}
	w.WriteHeader(status)
}

func (r *hookReceiver) url(path string) string { return r.server.URL + path }

func (r *hookReceiver) answer(path string, respond func(n int) int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.respond[path] = respond
}

// answerEnvelope answers the control envelopes of one type at path, and nothing
// else, with what respond returns. respond may block.
func (r *hookReceiver) answerEnvelope(path, kind string, respond func() int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.envelopes == nil {
		r.envelopes = map[string]func() int{}
	}
	r.envelopes[path+" "+kind] = respond
}

func (r *hookReceiver) padHeaders(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.padded[path] = true
}

func always(status int) func(int) int { return func(int) int { return status } }

func (r *hookReceiver) requests(path string) []hookRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []hookRequest
	for _, g := range r.got {
		if g.Path == path {
			out = append(out, g)
		}
	}
	return out
}

func (r *hookReceiver) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func (g hookRequest) messageID() string { return g.Header.Get("webhook-id") }

// signatureValid asks the official Standard Webhooks library, and checks the
// answer against a second reading of the scheme: the key is the base64
// decoding of the whsec_ secret, and the signature is v1, and the base64 of the
// HMAC-SHA256 of "<id>.<timestamp>.<body>" over the bytes that the receiver
// got. Both must accept.
func (g hookRequest) signatureValid(secret string) bool {
	verifier, err := standardwebhooks.NewWebhook(secret)
	if err != nil {
		return false
	}
	return verifier.VerifyIgnoringTimestamp(g.Body, g.Header) == nil && g.hmacValid(secret)
}

func (g hookRequest) hmacValid(secret string) bool {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(g.Header.Get("webhook-id") + "." + g.Header.Get("webhook-timestamp") + "."))
	_, _ = mac.Write(g.Body)
	want := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	for _, candidate := range strings.Fields(g.Header.Get("webhook-signature")) {
		if hmac.Equal([]byte(candidate), []byte(want)) {
			return true
		}
	}
	return false
}

type deliveredBody struct {
	EventID   string         `json:"eventId"`
	Name      string         `json:"name"`
	Timestamp time.Time      `json:"timestamp"`
	Data      map[string]any `json:"data"`
	Cursor    string         `json:"cursor"`
}

func (g hookRequest) body(t *testing.T) deliveredBody {
	t.Helper()
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(g.Body, &fields))
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	require.ElementsMatch(t, []string{"eventId", "name", "timestamp", "data", "cursor"}, keys, "an event body has these members and no others")
	var b deliveredBody
	require.NoError(t, json.Unmarshal(g.Body, &b))
	return b
}

// deliverHarness is a webhook harness whose client reaches the loopback
// hookReceiver, and a ledger of what the runs reported.
type deliverHarness struct {
	*webhookHarness
	rcv *hookReceiver

	ledger     faucet.DeliverResult
	ledgerSkip bool
}

func newDeliverHarness(t *testing.T, mutate ...func(*faucet.Deps)) *deliverHarness {
	t.Helper()
	allowLoopback := func(d *faucet.Deps) { d.Webhook.AllowInsecureLoopback = true }
	d := &deliverHarness{
		webhookHarness: newWebhookHarness(t, append([]func(*faucet.Deps){allowLoopback}, mutate...)...),
		rcv:            newHookReceiver(t),
		ledger:         faucet.DeliverResult{GaveUp: map[faucet.GiveUpReason]int{}},
	}
	t.Cleanup(d.requireEveryDeliveryAccountedFor)
	return d
}

// requireEveryDeliveryAccountedFor checks the books at the end of a test: every
// delivery that the runs queued is delivered, ended with a counted reason, or
// still queued. A delivery that left the queue without being counted breaks the
// sum.
func (d *deliverHarness) requireEveryDeliveryAccountedFor() {
	if d.ledgerSkip || d.t.Failed() {
		return
	}
	pending := len(d.store.deliveryRows())
	require.Equal(d.t, d.ledger.Enqueued, d.ledger.Delivered+d.ledger.GaveUpTotal()+pending,
		"queued %d, delivered %d, ended %v, still queued %d", d.ledger.Enqueued, d.ledger.Delivered, d.ledger.GaveUp, pending)
}

func (d *deliverHarness) deliverDueWith(ctx context.Context) faucet.DeliverResult {
	d.t.Helper()
	res, err := d.svc.DeliverDue(ctx)
	require.NoError(d.t, err)
	d.record(res)
	return res
}

func (d *deliverHarness) record(res faucet.DeliverResult) {
	d.ledger.Enqueued += res.Enqueued
	d.ledger.Delivered += res.Delivered
	for reason, n := range res.GaveUp {
		d.ledger.GaveUp[reason] += n
	}
}

func (d *deliverHarness) deliverDue() faucet.DeliverResult {
	d.t.Helper()
	return d.deliverDueWith(d.ctx)
}

type hookEndpoint struct {
	path   string
	secret string
	args   map[string]any
}

func (d *deliverHarness) endpoint(path string, arguments ...any) hookEndpoint {
	d.t.Helper()
	return hookEndpoint{path: path, secret: newSecret(d.t, 32), args: repoArgs(arguments...)}
}

func (d *deliverHarness) subscribeEndpoint(e hookEndpoint) subscribed {
	d.t.Helper()
	return d.subscribe(subscribeTo(e.args, d.rcv.url(e.path), e.secret))
}

// runDeliverDue starts a run in the background and returns what it reports.
func (d *deliverHarness) runDeliverDue() func() faucet.DeliverResult {
	d.t.Helper()
	done := make(chan faucet.DeliverResult, 1)
	go func() {
		res, err := d.svc.DeliverDue(d.ctx)
		assert.NoError(d.t, err)
		done <- res
	}()
	return func() faucet.DeliverResult { return <-done }
}

func (d *deliverHarness) advance(by time.Duration) { d.clock.Advance(by) }

func (d *deliverHarness) newRunEvent(prs ...int32) faucet.Event {
	return d.insert(repoID, "ci.run_completed", "", prs...)
}

func decodedSecret(t *testing.T, secret string) []byte {
	t.Helper()
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	require.NoError(t, err)
	return key
}

func (d *deliverHarness) logsContain(needle string) bool {
	return strings.Contains(d.logs.String(), needle)
}
