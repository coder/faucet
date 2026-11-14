package faucet

import (
	"sync"

	"golang.org/x/xerrors"
)

const defaultMaxWaitsPerLogin = 10

var errTooManyWaits = xerrors.New("too many waits are open for this account, try again when one ends")

// waitLimiter counts the waits each login holds open. A login's entry exists
// only while a wait holds it, so the map is never larger than the number of
// open waits.
type waitLimiter struct {
	limit int
	mu    sync.Mutex
	open  map[string]int
}

func newWaitLimiter(limit int) *waitLimiter {
	if limit == 0 {
		limit = defaultMaxWaitsPerLogin
	}
	return &waitLimiter{limit: limit, open: map[string]int{}}
}

// acquire takes one slot of login and returns the function that gives it back.
// It returns false when the login holds the limit already.
func (l *waitLimiter) acquire(login string) (release func(), ok bool) {
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
