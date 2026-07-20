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

	// ErrUnknownIntent is returned for an intent value chronicle does not
	// define — a [Query] whose intent filter is enabled but names no defined
	// [Intent]. Errors wrapping it are always an [*IntentError].
	ErrUnknownIntent = errors.New("chronicle: unknown intent")

	// ErrInvalidMeta is returned by every write path for caller-supplied
	// metadata no store can hold: a key or value containing a NUL byte.
	// PostgreSQL jsonb cannot represent NUL inside a string, so accepting one
	// would make the same write succeed on [MemStore] and fail on pgstore —
	// the write is rejected at the API boundary instead, identically
	// everywhere. Distinct from [ErrReservedMeta], which rejects keys chronicle
	// reserves for itself rather than values no backend can store.
	ErrInvalidMeta = errors.New("chronicle: invalid metadata")

	// ErrZeroTxTime is returned by a store handed a zero transaction instant
	// it would otherwise have stamped. A zero instant is not a timestamp:
	// written as TxFrom it reads as "always believed" and as TxTo it reads as
	// "still current", so a store that adopts proposed instants ([MemStore])
	// refuses a zero one rather than corrupting the transaction axis silently.
	ErrZeroTxTime = errors.New("chronicle: zero transaction instant")

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

	// ErrConflict is returned by [Store.Apply] when the write was computed
	// against a pre-state that no longer holds, because another writer changed
	// the entity in between. Nothing is applied.
	//
	// It is a retryable condition rather than a failure: [Log] re-reads the
	// entity and recomputes the split, up to a bounded number of attempts, and
	// only surfaces the error if it keeps losing. Callers who drive a store
	// directly should do the same.
	ErrConflict = errors.New("chronicle: write conflict")

	// ErrMissingHoldID is returned when a legal hold is placed without an ID.
	// Errors wrapping it are always a [*HoldError].
	ErrMissingHoldID = errors.New("chronicle: hold ID required")

	// ErrHoldExists is returned when a hold is placed under an ID that already
	// exists. Holds are not upsertable; see [HoldStore.PlaceHold].
	ErrHoldExists = errors.New("chronicle: hold already exists")

	// ErrHoldReleased is returned when a hold that has already been released
	// is released again. A second release would rewrite or silently discard
	// the first release's attribution; see [HoldStore.ReleaseHold].
	ErrHoldReleased = errors.New("chronicle: hold already released")

	// ErrCurrentRecord is returned by [Deleter.Delete] when a deletion names a
	// record that is still current belief. Nothing is deleted. Retention trims
	// history; it must never change the present. Errors wrapping it are always
	// a [*DeleteError] naming the record.
	ErrCurrentRecord = errors.New("chronicle: record is current belief")

	// ErrNoChain is returned by [Log.Verify] and [Log.ChainHead] when the
	// entity has no chained records and no tombstones — there is nothing to
	// verify, which must not be mistaken for a verification that passed.
	ErrNoChain = errors.New("chronicle: no hash chain")

	// ErrReservedMeta is returned by every write path when caller-supplied
	// metadata uses a key in chronicle's reserved namespace — any key starting
	// with [MetaReservedPrefix]. chronicle stores its own bookkeeping (chain
	// hashes, encryption markers) in Meta under that prefix, and a caller who
	// could write those keys could forge exactly what they exist to attest.
	ErrReservedMeta = errors.New("chronicle: reserved metadata key")

	// ErrShredded is returned when a record's data was encrypted under a
	// per-subject key that is no longer available — usually because
	// [Keyring.DestroyKey] destroyed it. The record's structure survives; its
	// value does not, and chronicle fails rather than hand back ciphertext
	// where plaintext was asked for. Errors wrapping it are always a
	// [*ShredError].
	ErrShredded = errors.New("chronicle: data shredded")

	// ErrKeyDestroyed is returned by a [Keyring] for a subject whose key has
	// been destroyed. Destruction is terminal: the keyring will not mint a
	// replacement under the same subject, since a quietly re-minted key would
	// make new writes readable under an identifier the caller believes erased.
	ErrKeyDestroyed = errors.New("chronicle: subject key destroyed")

	// ErrNoKeyring is returned when an operation needs a subject key and the
	// log has no [Keyring] configured — writing with [WithSubject], or reading
	// a record whose data is encrypted.
	ErrNoKeyring = errors.New("chronicle: no keyring configured")
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

// IntentError reports an intent value chronicle does not define, carrying the
// offending value. It wraps [ErrUnknownIntent].
type IntentError struct {
	// Intent is the offending value.
	Intent Intent
	// Err is the wrapped sentinel.
	Err error
}

// Error implements the error interface.
func (e *IntentError) Error() string {
	return fmt.Sprintf("chronicle: unknown intent %d", uint8(e.Intent))
}

// Unwrap returns the wrapped sentinel so [errors.Is] matches.
func (e *IntentError) Unwrap() error { return e.Err }

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

// ConflictError reports that a write was computed against a pre-state that no
// longer holds. It wraps [ErrConflict].
type ConflictError struct {
	// Reason describes what changed underneath the write.
	Reason string
	// Attempts is the number of times the write was retried before giving up,
	// zero when the error comes straight from a store.
	Attempts int
	// Err is the underlying failure, when a store had one to report. Nil when
	// the conflict was detected rather than reported.
	Err error
}

// Error implements the error interface.
func (e *ConflictError) Error() string {
	msg := "chronicle: write conflict: " + e.Reason
	if e.Attempts > 0 {
		msg += " (after " + itoa(e.Attempts) + " attempts)"
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap returns the underlying failure, so that a store's own error remains
// reachable with [errors.As].
func (e *ConflictError) Unwrap() error { return e.Err }

// Is reports that a ConflictError matches [ErrConflict], in addition to
// whatever the wrapped error matches.
func (e *ConflictError) Is(target error) bool { return target == ErrConflict }

// conflictf builds a [*ConflictError] with a formatted reason.
func conflictf(format string, args ...any) error {
	return &ConflictError{Reason: fmt.Sprintf(format, args...)}
}

// HoldError reports a failed legal-hold operation, carrying the hold's ID.
type HoldError struct {
	// ID is the hold involved, empty when the caller supplied none.
	ID string
	// Err is the wrapped sentinel: [ErrMissingHoldID], [ErrMissingActor],
	// [ErrHoldExists], [ErrHoldReleased] or [ErrNotFound].
	Err error
}

// Error implements the error interface.
func (e *HoldError) Error() string {
	if e.ID == "" {
		return "chronicle: hold: " + e.Err.Error()
	}
	return fmt.Sprintf("chronicle: hold %q: %v", e.ID, e.Err)
}

// Unwrap returns the wrapped sentinel so [errors.Is] matches.
func (e *HoldError) Unwrap() error { return e.Err }

// DeleteError reports a refused deletion, naming the record that caused the
// refusal. It wraps [ErrCurrentRecord].
type DeleteError struct {
	// RecordID names the record the deletion was refused over.
	RecordID RecordID
	// Err is the wrapped sentinel.
	Err error
}

// Error implements the error interface.
func (e *DeleteError) Error() string {
	return fmt.Sprintf("chronicle: delete %s: %v", e.RecordID, e.Err)
}

// Unwrap returns the wrapped sentinel so [errors.Is] matches.
func (e *DeleteError) Unwrap() error { return e.Err }

// ChainError reports a chain operation that could not run, carrying the
// entity involved. It wraps [ErrNoChain], or a store failure.
type ChainError struct {
	// Kind and EntityID identify the entity.
	Kind, EntityID string
	// Err is the wrapped error.
	Err error
}

// Error implements the error interface.
func (e *ChainError) Error() string {
	return fmt.Sprintf("chronicle: chain %s/%s: %v", e.Kind, e.EntityID, e.Err)
}

// Unwrap returns the wrapped error so [errors.Is] matches.
func (e *ChainError) Unwrap() error { return e.Err }

// KeyError reports a keyring failure, carrying the subject involved.
type KeyError struct {
	// Subject is the subject whose key was involved.
	Subject string
	// Err is the wrapped error: [ErrKeyDestroyed], [ErrNoKeyring], or a
	// keyring's own failure.
	Err error
}

// Error implements the error interface.
func (e *KeyError) Error() string {
	return fmt.Sprintf("chronicle: key for subject %q: %v", e.Subject, e.Err)
}

// Unwrap returns the wrapped error so [errors.Is] matches.
func (e *KeyError) Unwrap() error { return e.Err }

// ShredError reports that a record's encrypted data could not be recovered.
// It wraps [ErrShredded] — always — and additionally whatever underlying
// error the keyring or cipher reported.
type ShredError struct {
	// Subject is the subject whose key the data was encrypted under.
	Subject string
	// RecordID identifies the unrecoverable record, when known.
	RecordID RecordID
	// Reason says why recovery failed, when the cause is more specific than
	// the wrapped error.
	Reason string
	// Err is the underlying failure, when there was one.
	Err error
}

// Error implements the error interface.
func (e *ShredError) Error() string {
	msg := fmt.Sprintf("chronicle: record %s: data for subject %q is unrecoverable", e.RecordID, e.Subject)
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap returns the underlying failure, when there was one.
func (e *ShredError) Unwrap() error { return e.Err }

// Is reports that a ShredError matches [ErrShredded], in addition to whatever
// the wrapped error matches.
func (e *ShredError) Is(target error) bool { return target == ErrShredded }

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
