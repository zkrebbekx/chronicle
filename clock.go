package chronicle

import "time"

// Clock supplies transaction time. It exists so that tests can drive the
// transaction axis deterministically; production code should leave it alone
// and get [time.Now] in UTC.
//
// A Clock is only ever read by chronicle. There is no exported path by which a
// caller can write a transaction timestamp onto a record, and injecting a
// clock does not create one — see [Log.Put].
type Clock interface {
	// Now returns the current instant. chronicle converts the result to UTC
	// and applies the monotonic ratchet described on [NewLog], so a Clock is
	// not required to be monotonic or to return UTC itself.
	Now() time.Time
}

// ClockFunc adapts a function to the [Clock] interface.
type ClockFunc func() time.Time

// Now implements [Clock].
func (f ClockFunc) Now() time.Time { return f() }

// SystemClock is the default clock: [time.Now] in UTC.
var SystemClock Clock = ClockFunc(func() time.Time { return time.Now().UTC() })

// FixedClock is a [Clock] that returns a fixed instant until advanced. It is
// intended for tests and is not safe for concurrent modification while a log
// is writing; set it up before use, or advance it from the same goroutine that
// drives the writes.
//
// A fixed clock does not defeat chronicle's ordering guarantees. Because the
// log ratchets transaction time forward by a nanosecond whenever the clock
// fails to advance, a sequence of writes against a frozen clock still produces
// strictly increasing transaction timestamps — which is what keeps every
// superseded record's TxTo strictly after its TxFrom.
type FixedClock struct {
	// T is the instant the clock reports.
	T time.Time
}

// NewFixedClock returns a [FixedClock] reporting t.
func NewFixedClock(t time.Time) *FixedClock { return &FixedClock{T: t} }

// Now implements [Clock].
func (c *FixedClock) Now() time.Time { return c.T }

// Advance moves the clock forward by d and returns the new instant.
func (c *FixedClock) Advance(d time.Duration) time.Time {
	c.T = c.T.Add(d)
	return c.T
}

// Set moves the clock to t.
func (c *FixedClock) Set(t time.Time) { c.T = t }
