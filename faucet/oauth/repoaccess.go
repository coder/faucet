package oauth

import (
	"context"
	"sync"
	"time"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/faucet/faucet"
)

// A refresh of a session asks GitHub again about the checks of its
// connection that were used most recently, so that a live agent sees a
// revoked access within one access token's life.
const (
	// recheckLimit is how many checks a refresh asks about. The others are
	// stale for the session after its refresh and asked on their next use.
	recheckLimit = 50
	// recheckLookups is how many of the lookups run at once.
	recheckLookups = 4
	// recheckTimeout is the most time that the lookups of a refresh take.
	recheckTimeout = 5 * time.Second
	// recheckShare is the share of the time to the deadline of the refresh
	// request that the lookups may take, so that the grant still answers.
	recheckShare = 0.5
	// accessCheckRetention is how long a check that no session uses stays,
	// until the purge deletes it.
	accessCheckRetention = 24 * time.Hour
	// accessCheckUseStep is how old the last use of a check must be before
	// a use moves it, so that most uses write nothing. It is far below an
	// access token's life, so a check in use stays among those that a
	// refresh checks again.
	accessCheckUseStep = time.Minute
	// recordTimeout bounds the store write that keeps an answer of GitHub,
	// which runs to its end however the caller's request ends.
	recordTimeout = 5 * time.Second
)

// repoAccess asks GitHub whether the GitHub token of a user's connection can
// read a repository, and keeps the answer in the connection's access checks:
// a pass stores the check, a denial deletes it, and any other answer changes
// nothing. UserAccess asks through it, and the refresh grant checks again
// through it.
type repoAccess struct {
	tokens connections
	repos  RepoChecker
}

// ask asks GitHub whether the connection of session can read repo, with its
// GitHub token refreshed first when it expires within a minute. When GitHub
// rejects the token, it refreshes the token once and asks once more. repo is
// in lower case, and the answer counts from the time before the lookup. A
// denial stands even when recording it fails, which it logs. A pass that it
// cannot record is an error.
func (r repoAccess) ask(ctx context.Context, session faucet.OAuthSession, repo string) (int64, error) {
	started := r.tokens.clock.Now()
	id, answer := r.lookup(ctx, session, repo)
	check := AccessCheck{ConnectionID: session.ConnectionID, Repo: repo, RepoID: id, CheckedAt: started, LastUsedAt: started}
	err := r.record(ctx, check, answer)
	switch {
	case err == nil:
		return id, answer
	case xerrors.Is(answer, faucet.ErrNoAccess):
		r.tokens.logger.Warn(ctx, "could not delete the access check of a repository that GitHub denied, the denial stands", slog.F("session_id", session.SessionID), slog.F("connection_id", session.ConnectionID), slog.F("repo", repo), slog.Error(err))
		return 0, answer
	}
	return 0, err
}

func (r repoAccess) lookup(ctx context.Context, session faucet.OAuthSession, repo string) (int64, error) {
	token, err := r.tokens.accessToken(ctx, session.ConnectionID, r.tokens.expiresWithin(userTokenMargin))
	if err != nil {
		return 0, err
	}
	id, err := r.check(ctx, token, repo)
	if !xerrors.Is(err, faucet.ErrInvalidToken) {
		return id, err
	}
	r.tokens.logger.Info(ctx, "GitHub rejected the stored token of a connection, refreshing it", slog.F("session_id", session.SessionID), slog.F("connection_id", session.ConnectionID))
	rejected := token.access
	token, err = r.tokens.accessToken(ctx, session.ConnectionID, func(_ Connection, access string) bool { return access == rejected })
	if err != nil {
		return 0, err
	}
	return r.check(ctx, token, repo)
}

func (r repoAccess) check(ctx context.Context, token githubToken, repo string) (int64, error) {
	p, err := token.principal()
	if err != nil {
		return 0, err
	}
	return r.repos.RepoID(ctx, p, repo)
}

// record keeps answer, GitHub's answer to the lookup of c: c is stored when
// GitHub let the token read the repository, and deleted when GitHub denied
// it. A denial for one session thus also takes the check away from the
// other sessions of the connection.
func (r repoAccess) record(ctx context.Context, c AccessCheck, answer error) error {
	// GitHub answered, so the write runs to its end however the caller's
	// request ends: a denial that is not recorded leaves a check that the
	// other sessions of the connection take.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	switch {
	case answer == nil:
		if err := r.tokens.store.StoreAccessCheck(ctx, c); err != nil {
			return xerrors.Errorf("store the access check of %s: %w", c.Repo, err)
		}
	case xerrors.Is(answer, faucet.ErrNoAccess):
		if err := r.tokens.store.DeleteAccessCheck(ctx, c.ConnectionID, c.Repo); err != nil {
			return xerrors.Errorf("delete the access check of %s: %w", c.Repo, err)
		}
	}
	return nil
}

// recheck asks GitHub again, with token, about the most recently used checks
// of the session's connection, which the refresh of the session at now just
// made stale for it, and records each answer. It asks only about the checks
// used within one access token's life before now: one that no session used
// that long is not in use, and its next use asks GitHub. After an answer
// that is neither a pass nor a denial, no more lookups start. The lookups
// end at the earlier of recheckTimeout and recheckShare of the time to
// ctx's deadline, and a lookup still running then is canceled and changes
// nothing. The checks left stay stale for the session until their next use
// asks GitHub. The session is rotated by then, so recheck fails nothing and
// logs what it found. With the zero token, which a failed refresh of the
// GitHub token leaves, it asks nothing, so that no second GitHub refresh can
// run past the deadline.
func (r repoAccess) recheck(ctx context.Context, session Session, now time.Time, token githubToken) {
	logger := r.tokens.logger.With(slog.F("session_id", session.ID), slog.F("connection_id", session.ConnectionID))
	if token == (githubToken{}) {
		logger.Debug(ctx, "the refresh has no GitHub token, so it checks no repository again")
		return
	}
	ctx, cancel := recheckContext(ctx)
	defer cancel()
	checks, err := r.tokens.store.RecentAccessChecks(ctx, session.ConnectionID, now.Add(-accessTokenLifetime), recheckLimit)
	if err != nil {
		logger.Warn(ctx, "could not read the access checks of a refreshed session, each is checked on its next use", slog.Error(err))
		return
	}
	var (
		mu      sync.Mutex
		kept    int
		denied  []string
		failure error
		wg      sync.WaitGroup
	)
	failed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return failure != nil
	}
	slots := make(chan struct{}, recheckLookups)
	for _, c := range checks {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil || failed() {
			break
		}
		wg.Go(func() {
			// The slot goes back after the outcome counts, so the loop sees a
			// failure before it starts the next lookup.
			defer func() { <-slots }()
			gone, err := r.recheckOne(ctx, token, c)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				if failure == nil {
					failure = err
				}
			case gone:
				denied = append(denied, c.Repo)
			default:
				kept++
			}
		})
	}
	wg.Wait()
	fields := []slog.Field{slog.F("checked", kept+len(denied)), slog.F("denied", denied)}
	switch {
	case failure != nil:
		logger.Warn(ctx, "could not check every repository of a refreshed session again, the rest are checked on their next use", append(fields, slog.Error(failure))...)
	case len(denied) > 0:
		logger.Info(ctx, "GitHub denied repositories that a refreshed session could read before, their checks are gone", fields...)
	default:
		logger.Debug(ctx, "checked the repositories of a refreshed session again", fields...)
	}
}

// recheckOne asks GitHub about c again, records the answer, and reports
// whether GitHub denied it.
func (r repoAccess) recheckOne(ctx context.Context, token githubToken, c AccessCheck) (bool, error) {
	started := r.tokens.clock.Now()
	id, answer := r.check(ctx, token, c.Repo)
	c.RepoID, c.CheckedAt = id, started
	if err := r.record(ctx, c, answer); err != nil {
		return false, err
	}
	denied := xerrors.Is(answer, faucet.ErrNoAccess)
	if answer != nil && !denied {
		return false, xerrors.Errorf("ask GitHub about %s: %w", c.Repo, answer)
	}
	return denied, nil
}

// recheckContext returns ctx cut at the end of the lookups of a refresh. A
// context deadline is wall time, so the end does not use faucet's clock.
func recheckContext(ctx context.Context) (context.Context, context.CancelFunc) {
	start := time.Now()
	end := start.Add(recheckTimeout)
	if deadline, ok := ctx.Deadline(); ok {
		if share := start.Add(time.Duration(float64(deadline.Sub(start)) * recheckShare)); share.Before(end) {
			end = share
		}
	}
	return context.WithDeadline(ctx, end)
}
