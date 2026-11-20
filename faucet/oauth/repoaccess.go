package oauth

import (
	"cmp"
	"context"
	"sync"
	"time"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/superblink-events/faucet"
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
	// accessDenialRetention is how long a denial stays from the start of its
	// lookup, which keeps out a pass whose lookup started before it. By then
	// every session that could take such a pass has refreshed past it, as its
	// access token expired.
	accessDenialRetention = accessTokenLifetime
)

// repoAccess asks GitHub whether the GitHub token of a user's connection can
// read a repository, and keeps the answer in the connection's access checks:
// a pass stores the check, a denial stores a denial in its place, and any
// other answer changes nothing. UserAccess asks through it, and the refresh
// grant checks again through it.
type repoAccess struct {
	tokens connections
	repos  RepoChecker
}

// ask asks GitHub whether the connection of session can read repo, with its
// GitHub token refreshed first when it expires within a minute. When GitHub
// rejects the token, it refreshes the token once and asks once more. repo is
// in lower case, and the answer counts from the time before the lookup. An
// answer that takes the check away stands even when recording it fails,
// which it logs. A pass that it cannot record is an error.
func (r repoAccess) ask(ctx context.Context, session faucet.OAuthSession, repo string) (int64, error) {
	logger := r.tokens.logger.With(slog.F("session_id", session.SessionID), slog.F("connection_id", session.ConnectionID))
	started := r.tokens.clock.Now()
	token, answer := r.tokens.accessToken(ctx, session.ConnectionID, r.tokens.expiresWithin(userTokenMargin))
	var id int64
	if answer == nil {
		id, answer = r.lookup(ctx, logger, session.ConnectionID, token, repo)
	}
	check := AccessCheck{ConnectionID: session.ConnectionID, Repo: repo, RepoID: id, CheckedAt: started, LastUsedAt: started}
	err := r.record(ctx, check, answer)
	switch {
	case err == nil:
		return id, answer
	case refused(answer):
		logger.Warn(ctx, "could not store the denial of a repository that GitHub refused, the refusal stands", slog.F("repo", repo), slog.Error(err))
		return 0, answer
	}
	return 0, err
}

// lookup asks GitHub with token whether it can read repo. When GitHub
// rejects the token, it refreshes the token of the connection once and asks
// once more with the new one.
func (r repoAccess) lookup(ctx context.Context, logger slog.Logger, connectionID string, token githubToken, repo string) (int64, error) {
	id, err := r.check(ctx, token, repo)
	if !xerrors.Is(err, faucet.ErrInvalidToken) {
		return id, err
	}
	logger.Info(ctx, "GitHub rejected the stored token of a connection, refreshing it")
	rejected := token.access
	fresh, err := r.tokens.accessToken(ctx, connectionID, func(_ Connection, access string) bool { return access == rejected })
	if err != nil {
		return 0, err
	}
	return r.check(ctx, fresh, repo)
}

func (r repoAccess) check(ctx context.Context, token githubToken, repo string) (int64, error) {
	p, err := token.principal()
	if err != nil {
		return 0, err
	}
	return r.repos.RepoIDFresh(ctx, p, repo)
}

// record keeps answer, GitHub's answer to the lookup of c: c is stored when
// GitHub let the token read the repository, and a denial in its place when
// GitHub refused, as refused says. A refusal for one session thus also takes
// the check away from the other sessions of the connection. Either stays
// only when no lookup that started later answered first.
func (r repoAccess) record(ctx context.Context, c AccessCheck, answer error) error {
	// GitHub answered, so the write runs to its end however the caller's
	// request ends: a refusal that is not recorded leaves a check that the
	// other sessions of the connection take.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	switch {
	case answer == nil:
		if err := r.tokens.store.StoreAccessCheck(ctx, c); err != nil {
			return xerrors.Errorf("store the access check of %s: %w", c.Repo, err)
		}
	case refused(answer):
		c.RepoID = 0
		if err := r.tokens.store.StoreAccessCheck(ctx, c); err != nil {
			return xerrors.Errorf("store the denial of %s: %w", c.Repo, err)
		}
	}
	return nil
}

// refused reports whether answer takes the check of a repository away:
// GitHub denied the repository, or rejected the token even after a refresh,
// or refused to refresh it, which ends the sessions of the connection.
func refused(answer error) bool {
	return xerrors.Is(answer, faucet.ErrNoAccess) || xerrors.Is(answer, faucet.ErrInvalidToken)
}

// recheck asks GitHub again, with token, about the most recently used checks
// of the session's connection, which the refresh of the session at now just
// made stale for it, and records each answer. It asks only about the checks
// used within one access token's life before now: one that no session used
// that long is not in use, and its next use asks GitHub. After an answer
// that is neither a pass nor a denial, no more lookups start. The lookups
// end at the earlier of recheckTimeout and recheckShare of the time to
// ctx's deadline, and a lookup still running then is canceled and changes
// no check. The checks left stay stale for the session until their next use
// asks GitHub. The session is rotated by then, so recheck fails nothing and
// logs what it found.
//
// recheck never refreshes the GitHub token: the rotation is stored, and a
// refresh can take a GitHub refresh and a GitHub /user call, which would
// hold the grant's answer past the server's write deadline, so that the
// client loses the pair that the rotation gave it. When GitHub rejects the
// token, the check of that repository and every check that recheck did not
// get an answer for become denials, as of the start of that lookup. Each
// session's next check of one of them then asks GitHub itself, and that
// check refreshes the token, or ends every session of the user when GitHub
// refuses the refresh. With the zero token, which a failed refresh of the
// GitHub token leaves, it asks nothing.
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
		// recorded marks the checks whose answer is stored.
		recorded = make([]bool, len(checks))
		// rejectedAt is the start of the first lookup whose token GitHub
		// rejected, zero while GitHub took the token.
		rejectedAt time.Time
		wg         sync.WaitGroup
	)
	failed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return failure != nil
	}
	slots := make(chan struct{}, recheckLookups)
	for i, c := range checks {
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
			started, answer, recordErr := r.recheckOne(ctx, token, c)
			mu.Lock()
			defer mu.Unlock()
			recorded[i] = recordErr == nil && (answer == nil || refused(answer))
			switch {
			case xerrors.Is(answer, faucet.ErrInvalidToken):
				if rejectedAt.IsZero() || started.Before(rejectedAt) {
					rejectedAt = started
				}
				failure = cmp.Or(failure, xerrors.Errorf("GitHub rejected the token as it was asked about %s: %w", c.Repo, answer))
			case recordErr != nil:
				failure = cmp.Or(failure, recordErr)
			case xerrors.Is(answer, faucet.ErrNoAccess):
				denied = append(denied, c.Repo)
			case answer != nil:
				failure = cmp.Or(failure, xerrors.Errorf("ask GitHub about %s: %w", c.Repo, answer))
			default:
				kept++
			}
		})
	}
	wg.Wait()
	taken := 0
	if !rejectedAt.IsZero() {
		// The denials run to their end however the request ends, as a
		// record does, but share one time limit, so a stalled store holds
		// the grant's answer back by recordTimeout once.
		deny, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
		defer cancel()
		for i, c := range checks {
			if recorded[i] {
				continue
			}
			c.RepoID, c.CheckedAt = 0, rejectedAt
			if err := r.tokens.store.StoreAccessCheck(deny, c); err != nil {
				logger.Warn(ctx, "could not store the denial of a repository after GitHub rejected the token", slog.F("repo", c.Repo), slog.Error(err))
				if deny.Err() != nil {
					break
				}
				continue
			}
			taken++
		}
	}
	fields := []slog.Field{slog.F("checked", kept+len(denied)), slog.F("denied", denied), slog.F("taken_away", taken)}
	switch {
	case failure != nil:
		logger.Warn(ctx, "could not check every repository of a refreshed session again, the rest are checked on their next use", append(fields, slog.Error(failure))...)
	case len(denied) > 0:
		logger.Info(ctx, "GitHub denied repositories that a refreshed session could read before, their checks are gone", fields...)
	default:
		logger.Debug(ctx, "checked the repositories of a refreshed session again", fields...)
	}
}

// recheckOne asks GitHub about c again with token, without a refresh of the
// token, and records the answer. It returns when its lookup started,
// GitHub's answer, and the error of the record.
func (r repoAccess) recheckOne(ctx context.Context, token githubToken, c AccessCheck) (started time.Time, answer, recordErr error) {
	started = r.tokens.clock.Now()
	c.RepoID, answer = r.check(ctx, token, c.Repo)
	c.CheckedAt = started
	return started, answer, r.record(ctx, c, answer)
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
