package chronicle

import (
	"encoding/base64"
	"hash/fnv"
	"strconv"
	"strings"
	"time"
)

// Cursor is an opaque pagination token. Treat it as a string with no internal
// structure: it is safe to store, to pass through a URL, and to hand back to
// [Log.Query], and nothing else about it is guaranteed.
//
// The empty Cursor means two different things in the two directions it
// travels, and both are the natural reading. Passed in, it means "start at the
// beginning". Returned, it means "there is no next page".
type Cursor string

// NoCursor is the empty cursor: start from the beginning, or no more results.
const NoCursor Cursor = ""

// IsZero reports whether the cursor is empty.
func (c Cursor) IsZero() bool { return c == NoCursor }

// String implements [fmt.Stringer]. The value is opaque; this exists so that a
// cursor prints legibly in logs, not so that it can be parsed.
func (c Cursor) String() string { return string(c) }

// cursorVersion prefixes the encoded payload so that the format can change
// without old cursors being silently misread as new ones.
const cursorVersion = "c1"

// cursorKey is a cursor's decoded position: the sort key of the last record on
// the previous page. It mirrors the ordering in [compareRecords] exactly, so
// resuming is a matter of keeping records that sort strictly after it.
type cursorKey struct {
	TxFrom    time.Time
	ValidFrom time.Time
	ID        RecordID
}

// encodeCursor renders a record's sort key as an opaque, checksummed token.
func encodeCursor(r Record) Cursor {
	payload := strings.Join([]string{
		cursorVersion,
		encodeCursorTime(r.TxFrom),
		encodeCursorTime(r.ValidFrom),
		string(r.ID),
	}, "\x1f")
	full := payload + "\x1f" + checksumString(payload)
	return Cursor(base64.RawURLEncoding.EncodeToString([]byte(full)))
}

// checksumString is a non-cryptographic integrity check. Its job is to turn a
// mangled cursor into a clean [ErrInvalidCursor] rather than a silently wrong
// page, not to resist an adversary — a caller who wants to forge a position in
// their own result set can simply ask for that page.
func checksumString(payload string) string {
	sum := fnv.New32a()
	// fnv's Write never returns an error.
	_, _ = sum.Write([]byte(payload))
	return strconv.FormatUint(uint64(sum.Sum32()), 36)
}

// decodeCursor parses a token produced by encodeCursor. Every failure mode —
// bad base64, wrong field count, wrong version, unparseable time, bad checksum
// — reports [ErrInvalidCursor], because none of them are distinguishable to a
// caller who is meant to treat the value as opaque.
func decodeCursor(c Cursor) (cursorKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(string(c))
	if err != nil {
		return cursorKey{}, &CursorError{Cursor: c, Reason: "not valid base64", Err: ErrInvalidCursor}
	}
	parts := strings.Split(string(raw), "\x1f")
	if len(parts) != 5 {
		return cursorKey{}, &CursorError{Cursor: c, Reason: "malformed payload", Err: ErrInvalidCursor}
	}
	if parts[0] != cursorVersion {
		return cursorKey{}, &CursorError{Cursor: c, Reason: "unsupported cursor version " + parts[0], Err: ErrInvalidCursor}
	}
	if parts[4] != checksumString(strings.Join(parts[:4], "\x1f")) {
		return cursorKey{}, &CursorError{Cursor: c, Reason: "checksum mismatch", Err: ErrInvalidCursor}
	}
	txFrom, err := decodeCursorTime(parts[1])
	if err != nil {
		return cursorKey{}, &CursorError{Cursor: c, Reason: "unparseable transaction time", Err: ErrInvalidCursor}
	}
	validFrom, err := decodeCursorTime(parts[2])
	if err != nil {
		return cursorKey{}, &CursorError{Cursor: c, Reason: "unparseable valid time", Err: ErrInvalidCursor}
	}
	return cursorKey{TxFrom: txFrom, ValidFrom: validFrom, ID: RecordID(parts[3])}, nil
}

// encodeCursorTime renders an instant, using the empty string for the zero
// time so that an unbounded valid start round-trips as unbounded rather than
// as year 1. RFC 3339 with nanoseconds round-trips exactly.
func encodeCursorTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func decodeCursorTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// after reports whether a record sorts strictly after the cursor position in
// the given direction. This is the whole of keyset pagination: because the
// record ID is the final, unique tiebreaker, a record either sorts strictly
// after the last one returned or it does not, and no record can fall through
// the crack between two pages however many share a timestamp.
func (k cursorKey) after(r Record, descending bool) bool {
	probe := Record{TxFrom: k.TxFrom, ValidFrom: k.ValidFrom, ID: k.ID}
	c := compareRecords(r, probe)
	if descending {
		return c < 0
	}
	return c > 0
}

// CursorError reports a rejected pagination cursor. It wraps
// [ErrInvalidCursor].
type CursorError struct {
	// Cursor is the offending token.
	Cursor Cursor
	// Reason describes what was wrong with it.
	Reason string
	// Err is the wrapped sentinel.
	Err error
}

// Error implements the error interface.
func (e *CursorError) Error() string {
	return "chronicle: invalid cursor: " + e.Reason
}

// Unwrap returns the wrapped sentinel so [errors.Is] matches.
func (e *CursorError) Unwrap() error { return e.Err }
