package chronicle

import (
	"context"
	"slices"
	"strings"
	"time"
)

// Store is the persistence boundary. chronicle's temporal reasoning lives
// above it; a store only has to filter, sort and hand back rows.
//
// The interface is deliberately shaped so that a database/sql implementation
// is straightforward: every method takes a value and returns values, nothing
// takes a callback, nothing returns an iterator that holds a lock, and nothing
// requires that the whole log fit in memory. [Query] carries a limit and a
// cursor precisely so that a SQL implementation can push both down into a
// LIMIT and a keyset predicate rather than materialising a result set.
//
// # Atomicity
//
// A write supersedes some records and inserts others, and the two must land
// together — a Put that closes three records and writes four must never be
// observable half-applied, or a reader will see either a gap or an overlap in
// valid time, which is exactly the invariant the library exists to hold.
//
// [Store.Apply] therefore carries both halves of a write, and there is no way
// to express the halves separately. An earlier design had Put and Supersede as
// distinct methods with Apply as an optional extension; that shape was removed
// because the fallback path — supersede, then insert, with no shared
// transaction — is only correct when nobody else is looking, which is a
// property no library can check and every caller assumes.
//
// # Isolation
//
// chronicle reads an entity's overlapping records, computes a split, and then
// calls Apply. Those two steps are separate calls, so an implementation cannot
// protect the read with the write's transaction. It must instead detect that
// the pre-state changed underneath it and report [ErrConflict]; [Log] retries
// the whole read-modify-write when it sees one. Implementations backed by a
// database should take a per-entity lock for the duration of Apply and rely on
// a deferrable exclusion constraint to catch anything the lock missed.
type Store interface {
	// Apply performs the whole of a write: it closes the transaction interval
	// of every record named in Supersede, then inserts every record in Insert.
	// Either all of it is visible to a subsequent read, or none of it is.
	//
	// Apply owns the transaction axis. It picks one instant for the write,
	// stamps it on every inserted record's TxFrom and every superseded
	// record's TxTo, and returns it. Whatever TxFrom an inserted record
	// arrives carrying is overwritten, so there is no path by which a caller
	// can choose when the log appears to have learned something — which is the
	// only reason the transaction axis is worth trusting.
	//
	// Write.TxAt is a proposal. A single-writer store may adopt it; a store
	// with more than one writing process must not, since no one process's
	// clock is authoritative, and takes the instant from somewhere both
	// writers agree on instead. [Log] treats the returned instant as the
	// truth either way.
	//
	// Apply reports an error wrapping [ErrConflict] when the write was
	// computed against a pre-state that no longer holds — typically because a
	// record it meant to supersede has already been superseded by someone
	// else. Nothing is applied in that case.
	Apply(ctx context.Context, w Write) (time.Time, error)

	// Get returns the single record covering the given point on both axes, or
	// an error wrapping [ErrNotFound]. Where the log's invariant holds, at
	// most one record can match.
	Get(ctx context.Context, q GetQuery) (*Record, error)

	// Query returns records matching q in the order described by [Query],
	// along with a cursor for the next page. The cursor is empty when the
	// result set is exhausted.
	Query(ctx context.Context, q Query) ([]Record, Cursor, error)
}

// Write is one indivisible unit of change, as handed to [Store.Apply].
type Write struct {
	// Supersede names the records whose transaction interval is to be closed.
	// A record already closed keeps the timestamp it was closed with:
	// transaction time, once assigned, is never rewritten.
	//
	// Whether a target that is missing or already closed is an error depends
	// on what else the write does. On its own, a supersession is idempotent
	// and such a target is ignored, so that a retry cannot rewrite a
	// transaction timestamp. Alongside insertions it is one half of a split,
	// and a target that has moved means the split was planned against a
	// pre-state that no longer holds — applying the other half would leave the
	// entity's valid timeline overlapping — so the store reports [ErrConflict]
	// and applies nothing.
	Supersede []RecordID
	// TxAt is the transaction instant the log proposes for the write: the TxTo
	// of everything superseded and the TxFrom of everything inserted, so that
	// the belief transition happens at a single point on the transaction axis
	// with no gap and no overlap.
	//
	// It is a proposal. The store picks the instant it actually uses, applies
	// it to both halves of the write, and returns it — see [Store.Apply].
	TxAt time.Time
	// Insert holds the records to add: the caller's new record, plus any
	// remainders preserving the parts of superseded intervals the new record
	// did not cover.
	Insert []Record
}

// Entities returns the distinct (kind, entity) pairs a write touches, as
// implied by the records it inserts, in a deterministic order.
//
// Store implementations use it to decide what to lock. It cannot see entities
// named only by Supersede, since a record ID does not carry its entity; a
// store that needs those must look them up.
func (w Write) Entities() []EntityRef {
	seen := make(map[EntityRef]struct{}, len(w.Insert))
	out := make([]EntityRef, 0, len(w.Insert))
	for _, r := range w.Insert {
		ref := EntityRef{Kind: r.Kind, EntityID: r.EntityID}
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		out = append(out, ref)
	}
	slices.SortFunc(out, func(a, b EntityRef) int {
		if c := strings.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		return strings.Compare(a.EntityID, b.EntityID)
	})
	return out
}

// EntityRef names one entity: a kind and an ID within that kind.
type EntityRef struct {
	// Kind discriminates the type of entity.
	Kind string
	// EntityID is the caller's opaque identifier, scoped by Kind.
	EntityID string
}

// GetQuery locates a single record by its coordinates on both time axes.
type GetQuery struct {
	// Kind and EntityID identify the entity.
	Kind, EntityID string
	// ValidAt is the instant on the valid axis. A zero value is a real
	// instant here, not a wildcard: [Log.Get] resolves "now" before calling
	// the store, so that stores need no notion of the current time.
	ValidAt time.Time
	// TxAt is the instant on the transaction axis, resolved the same way.
	TxAt time.Time
}

// Query selects records across entities, filtered on either time axis and
// paginated with an opaque cursor.
//
// Every field is optional and the zero Query matches every record in the log.
// Time filters follow chronicle's usual convention: a zero instant means "no
// restriction", and a zero [Interval] covers all of time. Unlike [Log.Get],
// a store never substitutes the current time for a zero one — the log resolves
// "now" before it calls down, so that a store is a pure function of its
// contents and the query.
//
// # Order
//
// Results are ordered by transaction start, then valid start, then record ID,
// ascending unless Descending is set. The record ID breaks every remaining
// tie, and record IDs are unique, so the order is total: no two records ever
// compare equal, and pagination cannot skip or repeat a row when many records
// share a transaction instant.
type Query struct {
	// Kind restricts results to one entity kind. Empty matches all kinds.
	Kind string
	// EntityID restricts results to one entity. Empty matches all entities.
	// It is only meaningful alongside Kind, since entity IDs are scoped by
	// kind.
	EntityID string
	// ActorID restricts results to writes attributed to one actor.
	ActorID string
	// Intent restricts results to one intent. Use HasIntent to enable it,
	// since the zero Intent is a meaningful value ([IntentAssert]).
	Intent Intent
	// HasIntent enables the Intent filter.
	HasIntent bool

	// Valid selects records whose valid interval overlaps this one. The zero
	// interval covers all of time and so filters nothing.
	Valid Interval
	// Tx selects records whose transaction interval overlaps this one.
	Tx Interval
	// ValidAt selects records whose valid interval contains this instant.
	// Zero disables the filter.
	ValidAt time.Time
	// TxAt selects records whose transaction interval contains this instant.
	// Zero disables the filter.
	TxAt time.Time
	// CurrentOnly restricts results to records that are still current belief,
	// that is, whose transaction interval is open.
	CurrentOnly bool

	// Limit caps the number of records returned. Zero or negative means no
	// limit, and a store may then return the whole matching set.
	Limit int
	// After resumes from a cursor returned by a previous call. It must have
	// been produced by a query with the same Descending setting; resuming a
	// descending scan from an ascending cursor walks the other way from that
	// point rather than failing.
	After Cursor
	// Descending reverses the result order.
	Descending bool
}

// matches reports whether a record satisfies the query's filters, ignoring
// ordering, cursor and limit. It is exported to store implementations only in
// the sense that they live in this package; a SQL store would translate the
// same predicates into its WHERE clause.
func (q Query) matches(r Record) bool {
	if q.Kind != "" && r.Kind != q.Kind {
		return false
	}
	if q.EntityID != "" && r.EntityID != q.EntityID {
		return false
	}
	if q.ActorID != "" && r.Actor.ID != q.ActorID {
		return false
	}
	if q.HasIntent && r.Intent != q.Intent {
		return false
	}
	if q.CurrentOnly && !r.IsCurrent() {
		return false
	}
	if !q.Valid.IsAlways() && !r.Valid().Overlaps(q.Valid) {
		return false
	}
	if !q.Tx.IsAlways() && !r.Tx().Overlaps(q.Tx) {
		return false
	}
	if !q.ValidAt.IsZero() && !r.Valid().Contains(q.ValidAt) {
		return false
	}
	if !q.TxAt.IsZero() && !r.Tx().Contains(q.TxAt) {
		return false
	}
	return true
}

// validate checks the query's intervals and cursor shape.
func (q Query) validate() error {
	if err := q.Valid.Validate(); err != nil {
		return &IntervalError{Field: "valid", Interval: q.Valid, Err: ErrInvalidInterval}
	}
	if err := q.Tx.Validate(); err != nil {
		return &IntervalError{Field: "transaction", Interval: q.Tx, Err: ErrInvalidInterval}
	}
	if !q.Intent.valid() {
		return &KindError{Err: ErrUnknownKind}
	}
	return nil
}

// compareRecords imposes chronicle's total order: transaction start, then
// valid start, then record ID. Because IDs are unique this never returns zero
// for two distinct records, which is what makes keyset pagination exact.
func compareRecords(a, b Record) int {
	if c := a.TxFrom.Compare(b.TxFrom); c != 0 {
		return c
	}
	if c := compareStarts(a.ValidFrom, b.ValidFrom); c != 0 {
		return c
	}
	return strings.Compare(string(a.ID), string(b.ID))
}
