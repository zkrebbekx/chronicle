package chronicle

import (
	"errors"
	"fmt"
)

// Sentinel errors returned, usually wrapped, by this package. Match them with
// [errors.Is] rather than by comparing error strings.
var (
	// ErrNotFound is returned when no record satisfies a lookup — no state was
	// believed for that entity at that point on both axes. It is distinct from
	// an empty result: [Log.Get] reports it, while [Log.History] and
	// [Log.Query] simply return nothing.
	ErrNotFound = errors.New("chronicle: not found")

	// ErrInvalidInterval is returned for an interval that is empty or
	// inverted — a bounded upper end that does not strictly follow the lower
	// end. Such intervals are rejected at the API boundary rather than stored,
	// because a zero-width valid interval asserts nothing and an inverted one
	// asserts a contradiction. Errors wrapping this are always an
	// [*IntervalError].
	ErrInvalidInterval = errors.New("chronicle: invalid interval")

	// ErrMissingActor is returned by every write path when the actor has no
	// ID. chronicle has no ambient default actor; see [Actor] for why.
	ErrMissingActor = errors.New("chronicle: actor required")

	// ErrUnknownKind is returned for an empty kind, and for any kind outside
	// the allow-list when the log was constructed [WithKinds].
	ErrUnknownKind = errors.New("chronicle: unknown kind")

	// ErrCodec is returned when a record's data cannot be decoded for
	// structural comparison. [Log.Diff] reports it rather than silently
	// reporting no changes: under-reporting a change is the one failure mode a
	// change log must not have.
	ErrCodec = errors.New("chronicle: codec")

	// ErrInvalidCursor is returned when a pagination cursor is malformed,
	// truncated, or fails its checksum.
	ErrInvalidCursor = errors.New("chronicle: invalid cursor")

	// ErrMissingEntityID is returned when a write or lookup names no entity.
	// An empty entity ID is always a caller bug rather than a wildcard;
	// treating it as one would let a typo write into a shared phantom history.
	ErrMissingEntityID = errors.New("chronicle: entity ID required")

	// ErrClosed is returned by a store that has been closed.
	ErrClosed = errors.New("chronicle: store closed")
)

// IntervalError reports a malformed interval, carrying the offending bounds so
// that the caller can see which write was rejected. It wraps
// [ErrInvalidInterval].
type IntervalError struct {
	// Field names the interval that was rejected, when the operation involved
	// more than one. Empty when unambiguous.
	Field string
	// Interval is the offending interval.
	Interval Interval
	// Err is the wrapped sentinel.
	Err error
}

// Error implements the error interface.
func (e *IntervalError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("chronicle: invalid %s interval %s", e.Field, e.Interval)
	}
	return fmt.Sprintf("chronicle: invalid interval %s", e.Interval)
}

// Unwrap returns the wrapped sentinel so [errors.Is] matches.
func (e *IntervalError) Unwrap() error { return e.Err }

// KindError reports a rejected entity kind. It wraps [ErrUnknownKind].
type KindError struct {
	// Kind is the offending kind, empty if the caller supplied none.
	Kind string
	// Err is the wrapped sentinel.
	Err error
}

// Error implements the error interface.
func (e *KindError) Error() string {
	if e.Kind == "" {
		return "chronicle: kind required"
	}
	return fmt.Sprintf("chronicle: unknown kind %q", e.Kind)
}

// Unwrap returns the wrapped sentinel so [errors.Is] matches.
func (e *KindError) Unwrap() error { return e.Err }

// CodecError reports a failure to encode or decode record data, carrying the
// codec's name and the record involved. It wraps [ErrCodec].
type CodecError struct {
	// Codec is the name of the codec that failed.
	Codec string
	// RecordID identifies the record whose data could not be handled, when
	// known.
	RecordID RecordID
	// Err is the underlying failure.
	Err error
}

// Error implements the error interface.
func (e *CodecError) Error() string {
	if e.RecordID != "" {
		return fmt.Sprintf("chronicle: codec %s: record %s: %v", e.Codec, e.RecordID, e.Err)
	}
	return fmt.Sprintf("chronicle: codec %s: %v", e.Codec, e.Err)
}

// Unwrap returns the underlying failure.
func (e *CodecError) Unwrap() error { return e.Err }

// Is reports that a CodecError matches [ErrCodec], in addition to whatever the
// wrapped error matches.
func (e *CodecError) Is(target error) bool { return target == ErrCodec }

// NotFoundError reports that no record satisfied a lookup, carrying the
// coordinates that were searched. It wraps [ErrNotFound].
type NotFoundError struct {
	// Kind and EntityID identify the entity that was looked up.
	Kind, EntityID string
	// As is the point on both axes at which the lookup was made.
	As As
}

// Error implements the error interface.
func (e *NotFoundError) Error() string {
	return fmt.Sprintf("chronicle: no record for %s/%s valid at %s as known at %s",
		e.Kind, e.EntityID,
		boundString(e.As.ValidAt, "now"), boundString(e.As.TxAt, "now"))
}

// Unwrap returns [ErrNotFound].
func (e *NotFoundError) Unwrap() error { return ErrNotFound }
