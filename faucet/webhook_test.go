package faucet_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"cdr.dev/slog/v3/sloggers/sloghuman"

	"github.com/coder/faucet/faucet"
)

const (
	testSecret   = "whsec-test-0123456789abcdef"
	maxBodyBytes = 25 << 20
)

type fakeStore struct {
	mu     sync.Mutex
	events []faucet.Event
	calls  int
	err    error
}

func (s *fakeStore) InsertEvent(_ context.Context, ev faucet.Event) (faucet.Event, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return faucet.Event{}, false, s.err
	}
	if err := ev.Validate(); err != nil {
		return faucet.Event{}, false, err
	}
	for _, e := range s.events {
		if e.DeliveryID == ev.DeliveryID {
			return e, false, nil
		}
	}
	ev.ID = int64(len(s.events) + 1)
	s.events = append(s.events, ev)
	return ev, true, nil
}

func (s *fakeStore) EventsAfter(_ context.Context, afterID int64, limit int32) ([]faucet.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []faucet.Event
	for _, e := range s.events {
		if e.ID > afterID && len(out) < int(limit) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *fakeStore) insertCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *fakeStore) stored() []faucet.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]faucet.Event(nil), s.events...)
}

type harness struct {
	t       *testing.T
	store   *fakeStore
	logs    *bytes.Buffer
	handler http.Handler
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store := &fakeStore{}
	logs := &bytes.Buffer{}
	svc, err := faucet.New(faucet.Deps{
		Store:  store,
		Secret: []byte(testSecret),
		Logger: slog.Make(sloghuman.Sink(logs)),
	})
	require.NoError(t, err)
	return &harness{t: t, store: store, logs: logs, handler: svc.WebhookHandler()}
}

func (h *harness) serve(req *http.Request) *httptest.ResponseRecorder {
	h.t.Helper()
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	response := rec.Body.String() + fmt.Sprint(rec.Header())
	require.Zero(h.t, strings.Count(response, testSecret), "secret in response")
	require.Zero(h.t, strings.Count(h.logs.String(), testSecret), "secret in logs")
	return rec
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func newRequest(method string, body []byte, event, delivery string) *http.Request {
	req := httptest.NewRequest(method, "/webhook/github", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(testSecret, body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	return req
}

type countingBody struct {
	remaining int64
	read      int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), b.remaining)
	for i := range p[:n] {
		p[i] = 'a'
	}
	b.remaining -= n
	b.read += n
	return int(n), nil
}

func newStreamedRequest(body *countingBody, event, delivery string) *http.Request {
	req := newRequest(http.MethodPost, nil, event, delivery)
	req.Body = io.NopCloser(body)
	req.ContentLength = -1
	return req
}

func TestNew(t *testing.T) {
	t.Parallel()

	t.Run("RequiresStore", func(t *testing.T) {
		t.Parallel()

		svc, err := faucet.New(faucet.Deps{Secret: []byte(testSecret)})
		require.Error(t, err)
		require.Nil(t, svc)
	})

	t.Run("RequiresLongSecret", func(t *testing.T) {
		t.Parallel()

		short := "0123456789abcde"
		svc, err := faucet.New(faucet.Deps{Store: &fakeStore{}, Secret: []byte(short)})
		require.Error(t, err)
		require.Nil(t, svc)
		require.NotContains(t, err.Error(), short)

		_, err = faucet.New(faucet.Deps{Store: &fakeStore{}, Secret: []byte(short + "f")})
		require.NoError(t, err)
	})

	t.Run("SurvivesCallerChangingSecret", func(t *testing.T) {
		t.Parallel()

		secret := []byte(testSecret)
		svc, err := faucet.New(faucet.Deps{Store: &fakeStore{}, Secret: secret})
		require.NoError(t, err)
		clear(secret)

		body := []byte("{}")
		rec := httptest.NewRecorder()
		svc.WebhookHandler().ServeHTTP(rec, newRequest(http.MethodPost, body, "ping", "d-1"))
		require.Equal(t, http.StatusOK, rec.Code)
	})
}

func TestWebhookHandlerStoresEvents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		fixture  string
		event    string
		wantType string
	}{
		{"workflow_run_completed", "workflow_run", "ci.run_completed"},
		{"check_run_completed", "check_run", "ci.check_completed"},
		{"pull_request_review_submitted", "pull_request_review", "pr.review_submitted"},
		{"pull_request_closed", "pull_request", "pr.closed"},
		{"issue_comment_created_on_pr", "issue_comment", "pr.comment_created"},
	}
	for _, tt := range tests {
		t.Run(tt.wantType, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			rec := h.serve(newRequest(http.MethodPost, fixture(t, tt.fixture), tt.event, "delivery-1"))
			require.Equal(t, http.StatusOK, rec.Code)

			stored := h.store.stored()
			require.Len(t, stored, 1)
			require.Equal(t, tt.wantType, stored[0].Type)
			require.Equal(t, "delivery-1", stored[0].DeliveryID)
			require.Equal(t, "github", stored[0].Source)
			require.NotZero(t, stored[0].RepoID)
		})
	}

	t.Run("LogsDeliveryWithoutSecret", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		h.serve(newRequest(http.MethodPost, fixture(t, "workflow_run_completed"), "workflow_run", "delivery-log"))
		require.Contains(t, h.logs.String(), "delivery-log")
	})
}

func TestWebhookHandlerDuplicateDelivery(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	body := fixture(t, "workflow_run_completed")

	first := h.serve(newRequest(http.MethodPost, body, "workflow_run", "delivery-1"))
	second := h.serve(newRequest(http.MethodPost, body, "workflow_run", "delivery-1"))

	require.Equal(t, http.StatusOK, first.Code)
	require.Equal(t, http.StatusOK, second.Code)
	require.Len(t, h.store.stored(), 1)
}

func TestWebhookHandlerAcceptsWithoutStoring(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		event      string
		body       func(t *testing.T) []byte
		wantStatus int
	}{
		{"Ping", "ping", func(*testing.T) []byte { return []byte(`{"zen":"Keep it logically awesome.","hook_id":1}`) }, http.StatusOK},
		{"UnhandledEvent", "push", func(*testing.T) []byte { return []byte(`{"ref":"refs/heads/main"}`) }, http.StatusAccepted},
		{"UnhandledAction", "workflow_run", func(t *testing.T) []byte {
			return mutate(t, "workflow_run_completed", func(doc map[string]any) { doc["action"] = "requested" })
		}, http.StatusAccepted},
		{"CommentOnPlainIssue", "issue_comment", func(t *testing.T) []byte { return fixture(t, "issue_comment_created") }, http.StatusAccepted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			rec := h.serve(newRequest(http.MethodPost, tt.body(t), tt.event, "delivery-1"))
			require.Equal(t, tt.wantStatus, rec.Code)
			require.Empty(t, h.store.stored())
		})
	}
}

func TestWebhookHandlerRejects(t *testing.T) {
	t.Parallel()

	body := fixture(t, "workflow_run_completed")
	tests := []struct {
		name       string
		request    func(t *testing.T) *http.Request
		wantStatus int
	}{
		{"WrongSecret", func(*testing.T) *http.Request {
			req := newRequest(http.MethodPost, body, "workflow_run", "d-1")
			req.Header.Set("X-Hub-Signature-256", sign("another-secret-0123456789", body))
			return req
		}, http.StatusUnauthorized},
		{"BodyChangedAfterSigning", func(*testing.T) *http.Request {
			req := newRequest(http.MethodPost, append([]byte(" "), body...), "workflow_run", "d-1")
			req.Header.Set("X-Hub-Signature-256", sign(testSecret, body))
			return req
		}, http.StatusUnauthorized},
		{"MissingSignature", func(*testing.T) *http.Request {
			req := newRequest(http.MethodPost, body, "workflow_run", "d-1")
			req.Header.Del("X-Hub-Signature-256")
			return req
		}, http.StatusUnauthorized},
		{"SHA1SignatureOnly", func(*testing.T) *http.Request {
			req := newRequest(http.MethodPost, body, "workflow_run", "d-1")
			req.Header.Del("X-Hub-Signature-256")
			req.Header.Set("X-Hub-Signature", "sha1=0000000000000000000000000000000000000000")
			return req
		}, http.StatusUnauthorized},
		{"SignatureWithoutPrefix", func(*testing.T) *http.Request {
			req := newRequest(http.MethodPost, body, "workflow_run", "d-1")
			req.Header.Set("X-Hub-Signature-256", strings.TrimPrefix(sign(testSecret, body), "sha256="))
			return req
		}, http.StatusUnauthorized},
		{"SignatureNotHex", func(*testing.T) *http.Request {
			req := newRequest(http.MethodPost, body, "workflow_run", "d-1")
			req.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("z", 64))
			return req
		}, http.StatusUnauthorized},
		{"SignatureTooShort", func(*testing.T) *http.Request {
			req := newRequest(http.MethodPost, body, "workflow_run", "d-1")
			req.Header.Set("X-Hub-Signature-256", sign(testSecret, body)[:20])
			return req
		}, http.StatusUnauthorized},
		{"BadSignatureOnPing", func(*testing.T) *http.Request {
			req := newRequest(http.MethodPost, []byte(`{}`), "ping", "d-1")
			req.Header.Set("X-Hub-Signature-256", sign("another-secret-0123456789", []byte(`{}`)))
			return req
		}, http.StatusUnauthorized},
		{"BadSignatureBeatsMissingHeaders", func(*testing.T) *http.Request {
			req := newRequest(http.MethodPost, body, "workflow_run", "d-1")
			req.Header.Set("X-Hub-Signature-256", sign("another-secret-0123456789", body))
			req.Header.Del("X-GitHub-Event")
			req.Header.Del("X-GitHub-Delivery")
			return req
		}, http.StatusUnauthorized},
		{"BadSignatureBeatsMalformedPayload", func(*testing.T) *http.Request {
			payload := []byte(`{"action":"completed"`)
			req := newRequest(http.MethodPost, payload, "workflow_run", "d-1")
			req.Header.Set("X-Hub-Signature-256", sign("another-secret-0123456789", payload))
			return req
		}, http.StatusUnauthorized},
		{"BadSignatureBeatsPayloadWithoutRepositoryID", func(t *testing.T) *http.Request {
			payload := mutate(t, "workflow_run_completed", func(doc map[string]any) { delete(doc, "repository") })
			req := newRequest(http.MethodPost, payload, "workflow_run", "d-1")
			req.Header.Set("X-Hub-Signature-256", sign("another-secret-0123456789", payload))
			return req
		}, http.StatusUnauthorized},
		{"MissingEventHeader", func(*testing.T) *http.Request {
			req := newRequest(http.MethodPost, body, "workflow_run", "d-1")
			req.Header.Del("X-GitHub-Event")
			return req
		}, http.StatusBadRequest},
		{"MissingDeliveryHeader", func(*testing.T) *http.Request {
			req := newRequest(http.MethodPost, body, "workflow_run", "d-1")
			req.Header.Del("X-GitHub-Delivery")
			return req
		}, http.StatusBadRequest},
		{"MalformedPayload", func(*testing.T) *http.Request {
			return newRequest(http.MethodPost, []byte(`{"action":"completed"`), "workflow_run", "d-1")
		}, http.StatusBadRequest},
		{"PayloadWithoutRepositoryID", func(t *testing.T) *http.Request {
			payload := mutate(t, "workflow_run_completed", func(doc map[string]any) { delete(doc, "repository") })
			return newRequest(http.MethodPost, payload, "workflow_run", "d-1")
		}, http.StatusBadRequest},
		{"BodyOverLimit", func(*testing.T) *http.Request {
			return newRequest(http.MethodPost, bytes.Repeat([]byte("a"), maxBodyBytes+1), "push", "d-1")
		}, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			rec := h.serve(tt.request(t))
			require.Equal(t, tt.wantStatus, rec.Code)
			require.Empty(t, h.store.stored())
			require.Zero(t, h.store.insertCalls())
		})
	}

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodHead} {
		t.Run("Method"+method, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			rec := h.serve(newRequest(method, body, "workflow_run", "d-1"))
			require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
			require.Equal(t, http.MethodPost, rec.Header().Get("Allow"))
			require.Empty(t, h.store.stored())
		})
	}
}

func TestWebhookHandlerBodyLimit(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	rec := h.serve(newRequest(http.MethodPost, bytes.Repeat([]byte("a"), maxBodyBytes), "push", "d-1"))
	require.Equal(t, http.StatusAccepted, rec.Code, "a body of exactly 25 MiB is accepted")
}

func TestWebhookHandlerStopsReadingAtTheCap(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	body := &countingBody{remaining: maxBodyBytes + 4<<20}
	rec := h.serve(newStreamedRequest(body, "push", "d-1"))

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	require.LessOrEqual(t, body.read, int64(maxBodyBytes)+1, "bytes read from the body")
	require.Zero(t, h.store.insertCalls())
}

func TestWebhookHandlerRejectsBadSignatureHeaderWithoutReadingBody(t *testing.T) {
	t.Parallel()

	valid := sign(testSecret, []byte("{}"))
	tests := []struct {
		name   string
		header func(req *http.Request)
	}{
		{"Missing", func(req *http.Request) { req.Header.Del("X-Hub-Signature-256") }},
		{"WithoutPrefix", func(req *http.Request) {
			req.Header.Set("X-Hub-Signature-256", strings.TrimPrefix(valid, "sha256="))
		}},
		{"NotHex", func(req *http.Request) {
			req.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("z", 64))
		}},
		{"TooShort", func(req *http.Request) { req.Header.Set("X-Hub-Signature-256", valid[:20]) }},
		{"TooLong", func(req *http.Request) { req.Header.Set("X-Hub-Signature-256", valid+"00") }},
		{"SHA1Only", func(req *http.Request) {
			req.Header.Del("X-Hub-Signature-256")
			req.Header.Set("X-Hub-Signature", "sha1=0000000000000000000000000000000000000000")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			body := &countingBody{remaining: 1 << 10}
			req := newStreamedRequest(body, "push", "d-1")
			tt.header(req)
			rec := h.serve(req)

			require.Equal(t, http.StatusUnauthorized, rec.Code)
			require.Zero(t, body.read, "bytes read from the body")
			require.Zero(t, h.store.insertCalls())
		})
	}
}

func TestWebhookHandlerStoreFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.store.err = xerrors.New("connection to db-host-canary refused")

	rec := h.serve(newRequest(http.MethodPost, fixture(t, "workflow_run_completed"), "workflow_run", "d-1"))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.NotContains(t, rec.Body.String(), "db-host-canary")
	require.Empty(t, h.store.stored())
}
