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
// A write to one entity is a read-modify-write: chronicle reads the records
// whose valid intervals overlap the new one, computes how to split them, and
// applies the result. All three parts have to be one indivisible step, or two
// writers can observe the same pre-state and each split it.
//
// Apply therefore takes a *plan* rather than a finished write. The store reads
// the current overlapping records itself, inside its own transaction and under
// whatever lock it uses, hands them to the plan, and applies what comes back
// without ever releasing either. chronicle's temporal reasoning stays above the
// store — the store never learns what a remainder is — but the reasoning runs
// where the store can protect it.
//
// An earlier shape had the log read through Query and pass a finished Write to
// Apply. It was correct and it starved: with the read outside the lock, the
// writer that waits for the lock always finds its plan stale, so under
// sustained contention one writer wins every race and the other never lands a
// write at all. That is not a tuning problem, and no isolation level fixes it,
// because no isolation level spans two separate calls.
type Store interface {
	// Apply computes and applies one indivisible change.
	//
	// It reads the current records for req.Entity whose valid intervals
	// overlap req.Valid, passes them to req.Plan along with the transaction
	// instant it has assigned, and applies the returned [Write]: every record
	// named in Supersede has its transaction interval closed, and every record
	// in Insert is added. Either all of it is visible to a subsequent read, or
	// none of it is. The read and the write share one transaction and one
	// lock, so the plan cannot go stale between them.
	//
	// Apply owns the transaction axis. It picks the instant, stamps it on
	// every inserted record's TxFrom and every superseded record's TxTo, and
	// returns it. Whatever TxFrom an inserted record arrives carrying is
	// overwritten, so there is no path by which a caller can choose when the
	// log appears to have learned something — which is the only reason the
	// transaction axis is worth trusting.
	//
	// An error from the plan is returned unchanged and nothing is applied.
	Apply(ctx context.Context, req ApplyRequest) (time.Time, error)

	// Get returns the single record covering the given point on both axes, or
	// an error wrapping [ErrNotFound]. Where the log's invariant holds, at
	// most one record can match.
	Get(ctx context.Context, q GetQuery) (*Record, error)

	// Query returns records matching q in the order described by [Query],
	// along with a cursor for the next page. The cursor is empty when the
	// result set is exhausted.
	Query(ctx context.Context, q Query) ([]Record, Cursor, error)
}

// ApplyRequest describes one indivisible change as a plan for the store to
// compute, rather than as a finished write for it to carry out.
type ApplyRequest struct {
	// Entity is the entity being written, and the granularity at which the
	// store locks. A zero Entity means the write is not planned from existing
	// state: Plan is called with no current records, and the store locks only
	// what the resulting write turns out to touch. Use it for seeding and
	// migration, never for an ordinary write, since it gives up exactly the
	// protection the planning form exists to provide.
	Entity EntityRef

	// Valid narrows the current records handed to Plan to those whose valid
	// interval overlaps it. The zero interval covers all of time.
	Valid Interval

	// TxAt is the transaction instant the log proposes. It is a proposal: a
	// single-writer store may adopt it, and a store with more than one writing
	// process must not, since no one process's clock is authoritative. The
	// instant the store settles on is what Plan is given and what Apply
	// returns.
	TxAt time.Time

	// Plan computes the write. It must not block, must not touch the store,
	// and must confine itself to Entity — the store locked that and nothing
	// else, so records written for another entity are unprotected.
	Plan Planner
}

// Planner computes a write from an entity's current records and the
// transaction instant the store has assigned.
//
// current holds the entity's records that are still current belief and whose
// valid interval overlaps the request's, read inside the store's transaction
// under its lock. It is ordered by chronicle's total order and the planner may
// retain it; stores hand over copies.
//
// txAt is the instant the store has settled on. A planner that mints record
// IDs from the transaction time should use this one rather than any it chose
// beforehand.
type Planner func(current []Record, txAt time.Time) (Write, error)

// StaticWrite returns a [Planner] that ignores the current state and applies w
// exactly as given.
//
// It is for seeding fixtures, for migrations, and for tests — anywhere the
// write is already decided. It is not for ordinary writes: planning from state
// the store read under its own lock is the entire mechanism by which two
// writers to one entity cannot both split the same pre-state, and a static
// write opts out of it.
func StaticWrite(w Write) Planner {
	return func([]Record, time.Time) (Write, error) { return w, nil }
}

// Write is one indivisible unit of change, as returned by a [Planner].
type Write struct {
	// Supersede names the records whose transaction interval is to be closed.
	// A record already closed keeps the timestamp it was closed with:
	// transaction time, once assigned, is never rewritten, and naming one that
	// no longer exists is not an error.
	//
	// A planner given the store's own reading of the current records has no
	// way to name a record that has since moved, which is the point. A
	// [StaticWrite] does, and a store may then report [ErrConflict] rather
	// than apply half of a split.
	Supersede []RecordID
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

// Matches reports whether a record satisfies the query's filters, ignoring
// ordering, cursor and limit.
//
// A [Store] backed by a database translates the same predicates into its WHERE
// clause instead. This is the definition they must agree with, and the reason
// it is exported: the conformance suite checks the agreement, and a store
// author needs something to check against.
func (q Query) Matches(r Record) bool {
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

// Validate checks the query's filters for coherence: neither time range may be
// empty or inverted, and the intent filter must name an intent chronicle
// defines.
//
// A [Store] should call it before touching its backing store, so that a
// malformed query is the same error from every implementation rather than
// whatever the database happened to say about it.
func (q Query) Validate() error {
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

// CompareRecords imposes chronicle's total order: transaction start, then
// valid start, then record ID. Because IDs are unique this never returns zero
// for two distinct records, which is what makes keyset pagination exact.
func CompareRecords(a, b Record) int {
	if c := a.TxFrom.Compare(b.TxFrom); c != 0 {
		return c
	}
	if c := compareStarts(a.ValidFrom, b.ValidFrom); c != 0 {
		return c
	}
	return strings.Compare(string(a.ID), string(b.ID))
}
