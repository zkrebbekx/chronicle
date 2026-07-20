package chronicle

import (
	"fmt"
	"time"
)

// Interval is a half-open time interval [From, To).
//
// Both endpoints may be unbounded, and an unbounded endpoint is always the
// zero [time.Time] — never a sentinel maximum timestamp. A zero From means
// "since the beginning of time"; a zero To means "and it still holds". The
// zero Interval is therefore the whole of time, which is why it is a sensible
// default for a filter that means "no restriction".
//
// Every temporal comparison in chronicle is expressed through this type. That
// is deliberate: the zero-value-means-unbounded convention is the single most
// bug-prone part of a bitemporal engine, and scattering IsZero checks through
// query and write paths is how those bugs get in. If you need to reason about
// two chronicle intervals, use these methods rather than comparing the
// endpoints yourself.
type Interval struct {
	// From is the inclusive lower bound. Zero means unbounded below.
	From time.Time
	// To is the exclusive upper bound. Zero means unbounded above.
	To time.Time
}

// Between returns the half-open interval [from, to). Either endpoint may be
// the zero time to indicate an unbounded end.
func Between(from, to time.Time) Interval { return Interval{From: from, To: to} }

// Since returns the interval [from, ∞) — true from the given instant onwards,
// with no known end.
func Since(from time.Time) Interval { return Interval{From: from} }

// Until returns the interval [-∞, to) — true up to but not including the given
// instant, with no known beginning.
func Until(to time.Time) Interval { return Interval{To: to} }

// Always returns the interval covering all of time. It is the zero Interval.
func Always() Interval { return Interval{} }

// StartsUnbounded reports whether the interval has no lower bound.
func (iv Interval) StartsUnbounded() bool { return iv.From.IsZero() }

// EndsUnbounded reports whether the interval has no upper bound. On the
// transaction axis this is what "current belief" means; on the valid axis it
// is what "and it still holds" means.
func (iv Interval) EndsUnbounded() bool { return iv.To.IsZero() }

// IsAlways reports whether the interval covers all of time.
func (iv Interval) IsAlways() bool { return iv.From.IsZero() && iv.To.IsZero() }

// UTC returns the interval with both bounds converted to UTC. Zero bounds are
// left as the zero time so that they keep meaning "unbounded".
func (iv Interval) UTC() Interval {
	out := iv
	if !out.From.IsZero() {
		out.From = out.From.UTC()
	}
	if !out.To.IsZero() {
		out.To = out.To.UTC()
	}
	return out
}

// Validate reports whether the interval is well formed. An interval is
// well formed unless it is bounded at both ends and its upper bound does not
// strictly follow its lower bound: empty and inverted intervals are both
// rejected, wrapping [ErrInvalidInterval].
//
// An unbounded end is never invalid, because there is nothing for it to be
// inverted against.
func (iv Interval) Validate() error {
	if iv.From.IsZero() || iv.To.IsZero() {
		return nil
	}
	if !iv.To.After(iv.From) {
		return &IntervalError{Interval: iv, Err: ErrInvalidInterval}
	}
	return nil
}

// Contains reports whether the instant t falls within the interval, honouring
// the half-open convention: the lower bound is included, the upper bound is
// not.
func (iv Interval) Contains(t time.Time) bool {
	return !startAfter(iv.From, t) && endAfterInstant(iv.To, t)
}

// Overlaps reports whether the two intervals share at least one instant.
// Adjacent intervals — where one ends exactly where the other begins — do not
// overlap, which is the point of the half-open convention.
func (iv Interval) Overlaps(o Interval) bool {
	return startBeforeEnd(iv.From, o.To) && startBeforeEnd(o.From, iv.To)
}

// StartsBefore reports whether this interval begins strictly before the other.
// An unbounded start precedes every bounded start.
func (iv Interval) StartsBefore(o Interval) bool { return startBeforeStart(iv.From, o.From) }

// ExtendsBeyond reports whether this interval ends strictly after the other.
// An unbounded end extends beyond every bounded end, and nothing extends
// beyond an unbounded end.
func (iv Interval) ExtendsBeyond(o Interval) bool { return endAfterEnd(iv.To, o.To) }

// Duration returns the length of the interval and whether it is finite. It
// returns false for any interval with an unbounded end.
func (iv Interval) Duration() (time.Duration, bool) {
	if iv.From.IsZero() || iv.To.IsZero() {
		return 0, false
	}
	return iv.To.Sub(iv.From), true
}

// String renders the interval in half-open notation, using -∞ and ∞ for
// unbounded ends.
func (iv Interval) String() string {
	return fmt.Sprintf("[%s, %s)", boundString(iv.From, "-∞"), boundString(iv.To, "∞"))
}

func boundString(t time.Time, unbounded string) string {
	if t.IsZero() {
		return unbounded
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// The four primitive comparisons below are the whole of chronicle's temporal
// reasoning. Each one names which side of an interval its arguments come from,
// because that is what decides how a zero time is read: a zero lower bound is
// -∞ and a zero upper bound is +∞, and mixing the two up is the classic
// bitemporal bug.

// startAfter reports whether the lower bound from lies strictly after the
// instant t. An unbounded lower bound is after nothing.
func startAfter(from, t time.Time) bool {
	if from.IsZero() {
		return false
	}
	return from.After(t)
}

// endAfterInstant reports whether the upper bound to lies strictly after the
// instant t. An unbounded upper bound is after everything.
func endAfterInstant(to, t time.Time) bool {
	if to.IsZero() {
		return true
	}
	return to.After(t)
}

// startBeforeEnd reports whether the lower bound from lies strictly before the
// upper bound to. Unbounded on either side makes this true, since -∞ precedes
// every upper bound and every lower bound precedes +∞.
func startBeforeEnd(from, to time.Time) bool {
	if from.IsZero() || to.IsZero() {
		return true
	}
	return from.Before(to)
}

// startBeforeStart reports whether lower bound a lies strictly before lower
// bound b. An unbounded lower bound precedes every bounded one and ties with
// another unbounded one.
func startBeforeStart(a, b time.Time) bool {
	if a.IsZero() {
		return !b.IsZero()
	}
	if b.IsZero() {
		return false
	}
	return a.Before(b)
}

// endAfterEnd reports whether upper bound a lies strictly after upper bound b.
// An unbounded upper bound follows every bounded one, and nothing follows an
// unbounded one.
func endAfterEnd(a, b time.Time) bool {
	if a.IsZero() {
		return !b.IsZero()
	}
	if b.IsZero() {
		return false
	}
	return a.After(b)
}

// compareStarts orders two lower bounds, treating an unbounded start as -∞.
func compareStarts(a, b time.Time) int {
	switch {
	case startBeforeStart(a, b):
		return -1
	case startBeforeStart(b, a):
		return 1
	default:
		return 0
	}
}
