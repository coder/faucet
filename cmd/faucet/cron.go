package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"cdr.dev/slog/v3"
	"github.com/coder/faucet/faucet"
	"github.com/coder/quartz"
)

// cronTimeout keeps a run inside the server's write deadline.
const cronTimeout = 25 * time.Second

// cronRoutes serves the job that Vercel Cron triggers with GET and
// Authorization: Bearer $CRON_SECRET.
type cronRoutes struct {
	// secret is CRON_SECRET. Empty means the routes answer 503 and run nothing.
	secret    []byte
	svc       *faucet.Service
	retention time.Duration
	clock     quartz.Clock
	logger    slog.Logger
}

func (c cronRoutes) mount(r chi.Router) {
	r.Get("/cron/purge", c.guard(c.purge))
}

// guard runs next only for the right secret. Both sides are hashed first, so
// the comparison takes the same time whatever the guess is and whatever the
// secret's length. A refusal never says which part was wrong.
func (c cronRoutes) guard(next http.HandlerFunc) http.HandlerFunc {
	want := sha256.Sum256(c.secret)
	return func(w http.ResponseWriter, r *http.Request) {
		if len(c.secret) == 0 {
			http.Error(w, "cron is not configured", http.StatusServiceUnavailable)
			return
		}
		token, ok := bearerToken(r.Header.Get("Authorization"))
		got := sha256.Sum256([]byte(token))
		if !ok || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			unauthorized(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), cronTimeout)
		defer cancel()
		w.Header().Set("Cache-Control", "no-store")
		next(w, r.WithContext(ctx))
	}
}

func (c cronRoutes) purge(w http.ResponseWriter, r *http.Request) {
	res, err := c.svc.Purge(r.Context(), c.clock.Now().Add(-c.retention))
	if err != nil {
		c.logger.Error(r.Context(), "cron purge failed", slog.Error(err))
		http.Error(w, "purge failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, struct {
		Deleted       int64 `json:"deleted"`
		PurgedThrough int64 `json:"purged_through"`
	}{res.Deleted, res.PurgedThrough})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
