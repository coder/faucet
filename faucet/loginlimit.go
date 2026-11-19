package faucet

import (
	"sync"

	"golang.org/x/xerrors"
)

const (
	defaultMaxWaitsPerLogin = 10
	// defaultMaxPollsPerLogin is half of the connections that cmd/faucet
	// opens, so one login's polls cannot hold every one of them.
	defaultMaxPollsPerLogin = 5
)

var (
	errTooManyWaits = xerrors.New("too many waits are open for this account, try again when one ends")
	errTooManyPolls = xerrors.New("too many polls are running for this account, try again when one ends")
)

// loginLimiter counts the calls of one kind that each login holds open. A
// login's entry exists only while a call holds it, so the map is never larger
// than the number of open calls.
type loginLimiter struct {
	limit int
	mu    sync.Mutex
	open  map[string]int
}

// newLoginLimiter allows limit calls per login, or byDefault when limit is
// zero. A negative limit allows any number.
func newLoginLimiter(limit, byDefault int) *loginLimiter {
	if limit == 0 {
		limit = byDefault
	}
	return &loginLimiter{limit: limit, open: map[string]int{}}
}

// acquire takes one slot of login and returns the function that gives it back.
// It returns false when the login holds the limit already.
func (l *loginLimiter) acquire(login string) (release func(), ok bool) {
	if l.limit < 0 {
		return func() {}, true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open[login] >= l.limit {
		return nil, false
	}
	l.open[login]++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.open[login]--
		if l.open[login] == 0 {
			delete(l.open, login)
		}
	}, true
}
