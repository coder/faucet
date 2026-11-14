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
	max  int
	mu   sync.Mutex
	open map[string]int
}

func newWaitLimiter(max int) *waitLimiter {
	if max == 0 {
		max = defaultMaxWaitsPerLogin
	}
	return &waitLimiter{max: max, open: map[string]int{}}
}

// acquire takes one slot of login and returns the function that gives it back.
// It returns false when the login holds max slots already.
func (l *waitLimiter) acquire(login string) (release func(), ok bool) {
	if l.max < 0 {
		return func() {}, true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open[login] >= l.max {
		return nil, false
	}
	l.open[login]++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.open[login]--; l.open[login] == 0 {
			delete(l.open, login)
		}
	}, true
}
