package faucet

import (
	"strconv"
	"strings"
)

// Position is a place on the line of one target. The line of a repo target
// numbers the repository's events 1, 2, 3 in the order the store received
// them, the line of an owner target numbers the events of the owner's
// repositories, and the line of the all target is the event ID. Every number
// that a caller of a target gets is a position on that target's line, so
// that it counts the target's events and no other's.
type Position int64

// PositionOf is the place of ev on the line of t.
func (t Target) PositionOf(ev Event) Position {
	switch t.kind {
	case TargetRepo:
		return ev.RepoPosition
	case TargetOwner:
		return ev.OwnerPosition
	}
	return Position(ev.ID)
}

const (
	cursorPrefix = "v2."
	// legacyCursorPrefix is the prefix of the cursors of builds before lines,
	// whose number is an event ID.
	legacyCursorPrefix = "v1."
)

// cursor is a cursor that a caller sent. A legacy cursor holds an event ID,
// which counts every target's events, so it is never read as a position:
// the read starts at the head and says that events may be missing.
type cursor struct {
	position Position
	legacy   bool
}

func encodeCursor(p Position) string {
	return cursorPrefix + strconv.FormatInt(int64(p), 10)
}

// decodeCursor reads a cursor that some build of faucet could have returned.
func decodeCursor(s string) (cursor, bool) {
	if digits, ok := strings.CutPrefix(s, legacyCursorPrefix); ok {
		_, ok := canonicalNumber(digits)
		return cursor{legacy: true}, ok
	}
	digits, ok := strings.CutPrefix(s, cursorPrefix)
	if !ok {
		return cursor{}, false
	}
	n, ok := canonicalNumber(digits)
	return cursor{position: Position(n)}, ok
}

// canonicalNumber reads a non-negative decimal number written as
// strconv.FormatInt writes it.
func canonicalNumber(digits string) (int64, bool) {
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != digits {
		return 0, false
	}
	return n, true
}
