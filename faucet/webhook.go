package faucet

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
)

const (
	// maxBodyBytes is GitHub's cap on a webhook payload.
	maxBodyBytes = 25 << 20

	signatureHeader = "X-Hub-Signature-256"
	signaturePrefix = "sha256="
	eventHeader     = "X-GitHub-Event"
	deliveryHeader  = "X-GitHub-Delivery"
)

// WebhookHandler returns the handler for GitHub webhook deliveries.
//
// A delivery is rejected until its HMAC-SHA256 signature matches the secret.
// Responses:
//
//	405 not a POST
//	401 missing or wrong signature
//	413 body over 25 MiB
//	400 missing event or delivery header, or a malformed payload
//	200 ping, a stored event, or a delivery that was already stored
//	202 an event faucet ignores
//	500 the store failed, so GitHub records a failed delivery
func (s *Service) WebhookHandler() http.Handler {
	return http.HandlerFunc(s.serveWebhook)
}

func (s *Service) serveWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		s.reject(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	got, ok := parseSignature(r.Header.Get(signatureHeader))
	if !ok {
		s.reject(w, r, http.StatusUnauthorized, "invalid signature")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if xerrors.As(err, &tooLarge) {
			s.reject(w, r, http.StatusRequestEntityTooLarge, "body too large")
			return
		}
		s.reject(w, r, http.StatusBadRequest, "could not read body")
		return
	}
	if !hmac.Equal(got, sign(s.secret, body)) {
		s.reject(w, r, http.StatusUnauthorized, "invalid signature")
		return
	}

	eventName := r.Header.Get(eventHeader)
	deliveryID := r.Header.Get(deliveryHeader)
	if eventName == "" || deliveryID == "" {
		s.reject(w, r, http.StatusBadRequest, "missing event or delivery header")
		return
	}
	logger := s.logger.With(slog.F("github_event", eventName), slog.F("delivery_id", deliveryID))

	if eventName == "ping" {
		logger.Debug(r.Context(), "webhook ping")
		reply(w, http.StatusOK, "pong")
		return
	}
	ev, err := Normalize(eventName, body, deliveryID)
	if err != nil {
		logger.Warn(r.Context(), "webhook payload rejected", slog.Error(err))
		reply(w, http.StatusBadRequest, "malformed payload: "+err.Error())
		return
	}
	if ev == nil {
		logger.Debug(r.Context(), "webhook ignored")
		reply(w, http.StatusAccepted, "ignored")
		return
	}
	stored, inserted, err := s.store.InsertEvent(r.Context(), *ev)
	if err != nil {
		logger.Error(r.Context(), "store event", slog.Error(err))
		reply(w, http.StatusInternalServerError, "could not store event")
		return
	}
	logger.Info(r.Context(), "webhook stored",
		slog.F("type", stored.Type), slog.F("event_id", stored.ID), slog.F("inserted", inserted))
	if !inserted {
		reply(w, http.StatusOK, "duplicate")
		return
	}
	reply(w, http.StatusOK, "stored")
}

func parseSignature(header string) ([]byte, bool) {
	digest, ok := strings.CutPrefix(header, signaturePrefix)
	if !ok {
		return nil, false
	}
	sig, err := hex.DecodeString(digest)
	if err != nil || len(sig) != sha256.Size {
		return nil, false
	}
	return sig, true
}

func sign(secret, body []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return mac.Sum(nil)
}

func (s *Service) reject(w http.ResponseWriter, r *http.Request, status int, msg string) {
	s.logger.Warn(r.Context(), "webhook rejected", slog.F("status", status), slog.F("reason", msg))
	reply(w, status, msg)
}

// reply uses http.Error even for 2xx so every response carries the same headers.
func reply(w http.ResponseWriter, status int, msg string) {
	http.Error(w, msg, status)
}
