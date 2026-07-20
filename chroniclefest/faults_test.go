package chroniclefest_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/zkrebbekx/chronicle"
	"github.com/zkrebbekx/chronicle/chroniclefest"
)

// This file holds deliberately broken stores, one fault each, and nothing else.
// [TestSuiteCatchesFaults] runs the conformance suite against every one of them
// and asserts that the suite fails on the check that names the fault.
//
// The motivation is that a conformance suite which has only ever passed is not
// evidence. Its failure branches are the entire product — an inverted
// comparison or a mistyped variable in one of them would let a broken
// third-party store through while the suite reported that it had checked. These
// stores are how those branches get executed.
//
// Each fault wraps [chronicle.MemStore] and overrides exactly one behaviour, so
// the difference between it and a conforming store is nameable in one sentence,
// and a failure elsewhere is a signal that the suite is imprecise rather than
// that the fault was caught.

// base is the conforming store every fault is a delta from.
type base struct{ *chronicle.MemStore }

func newBase(t chroniclefest.T) base {
	m := chronicle.NewMemStore()
	t.Cleanup(func() { _ = m.Close() })
	return base{m}
}

// inner is the conforming store underneath a fault. Every fault delegates
// through it rather than through the embedded field directly, so that a call
// site reading s.inner().Query is visibly the correct implementation and not a
// recursion into the override being written.
func (b base) inner() *chronicle.MemStore { return b.MemStore }

// planWith returns req with its planner wrapped so that fn may corrupt the
// write the planner produced, inside the store's transaction and lock.
func planWith(req chronicle.ApplyRequest, fn func(chronicle.Write) chronicle.Write) chronicle.ApplyRequest {
	inner := req.Plan
	req.Plan = func(current []chronicle.Record, txAt time.Time) (chronicle.Write, error) {
		w, err := inner(current, txAt)
		if err != nil {
			return w, err
		}
		return fn(w), nil
	}
	return req
}

// ---------------------------------------------------------------------------
// atomicity
// ---------------------------------------------------------------------------

// deferredSupersede lands the insertions at the instant it reports and closes
// the superseded records a second later, which is what an implementation that
// runs the two halves of a write in separate transactions looks like from the
// outside. Between the two instants the entity has two current records; after
// them the old record's transaction interval overlaps the new one's.
type deferredSupersede struct{ base }

func (s *deferredSupersede) Apply(ctx context.Context, req chronicle.ApplyRequest) (time.Time, error) {
	var held []chronicle.RecordID
	tx, err := s.inner().Apply(ctx, planWith(req, func(w chronicle.Write) chronicle.Write {
		if len(w.Insert) == 0 {
			return w
		}
		held, w.Supersede = w.Supersede, nil
		return w
	}))
	if err != nil || len(held) == 0 {
		return tx, err
	}
	_, err = s.inner().Apply(ctx, chronicle.ApplyRequest{
		TxAt: tx.Add(time.Second),
		Plan: chronicle.StaticWrite(chronicle.Write{Supersede: held}),
	})
	return tx, err
}

// droppedInserts performs the supersessions of a split and silently discards
// the insertions, so the entity's valid-time coverage acquires a hole exactly
// where the replacement should have gone. Writes that only insert are left
// alone, so the fault is confined to the non-atomic case.
type droppedInserts struct{ base }

func (s *droppedInserts) Apply(ctx context.Context, req chronicle.ApplyRequest) (time.Time, error) {
	return s.inner().Apply(ctx, planWith(req, func(w chronicle.Write) chronicle.Write {
		if len(w.Supersede) > 0 {
			w.Insert = nil
		}
		return w
	}))
}

// ignoredSupersessions inserts what it is given and never closes anything, so
// every generation of a record stays current belief simultaneously.
type ignoredSupersessions struct{ base }

func (s *ignoredSupersessions) Apply(ctx context.Context, req chronicle.ApplyRequest) (time.Time, error) {
	return s.inner().Apply(ctx, planWith(req, func(w chronicle.Write) chronicle.Write {
		w.Supersede = nil
		return w
	}))
}

// noConflictDetection quietly drops the already-closed records from a
// supersession rather than reporting [chronicle.ErrConflict], so half of a
// split planned against a stale pre-state lands and the entity ends up with two
// current records over one valid instant.
type noConflictDetection struct{ base }

func (s *noConflictDetection) Apply(ctx context.Context, req chronicle.ApplyRequest) (time.Time, error) {
	// Snapshotted before the store takes its lock, since the planner runs
	// inside it and must not touch the store.
	closed := map[chronicle.RecordID]bool{}
	if recs, _, err := s.inner().Query(ctx, chronicle.Query{}); err == nil {
		for _, r := range recs {
			if !r.IsCurrent() {
				closed[r.ID] = true
			}
		}
	}
	return s.inner().Apply(ctx, planWith(req, func(w chronicle.Write) chronicle.Write {
		w.Supersede = slices.DeleteFunc(slices.Clone(w.Supersede), func(id chronicle.RecordID) bool {
			return closed[id]
		})
		return w
	}))
}

// ---------------------------------------------------------------------------
// the transaction axis
// ---------------------------------------------------------------------------

// callerChosenTx is the hole that [chronicle.Store.Apply] returning a
// time.Time exists to close: it stamps an instant of its own choosing and
// reports back the one the caller proposed, so every as-of query the caller
// then makes is aimed at a coordinate the log never used.
type callerChosenTx struct{ base }

func (s *callerChosenTx) Apply(ctx context.Context, req chronicle.ApplyRequest) (time.Time, error) {
	proposed := req.TxAt
	req.TxAt = proposed.Add(-time.Hour)
	if _, err := s.inner().Apply(ctx, req); err != nil {
		return time.Time{}, err
	}
	return proposed, nil
}

// zeroTx applies correctly but reports the zero time, leaving the caller with
// no coordinate to read its own write back at.
type zeroTx struct{ base }

func (s *zeroTx) Apply(ctx context.Context, req chronicle.ApplyRequest) (time.Time, error) {
	if _, err := s.inner().Apply(ctx, req); err != nil {
		return time.Time{}, err
	}
	return time.Time{}, nil
}

// frozenTx stamps every write with one fixed instant, so a record superseded by
// a later write gets a transaction interval of zero width and becomes invisible
// to every as-of query rather than merely historical.
type frozenTx struct{ base }

var frozen = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

func (s *frozenTx) Apply(ctx context.Context, req chronicle.ApplyRequest) (time.Time, error) {
	req.TxAt = frozen
	return s.inner().Apply(ctx, req)
}

// ---------------------------------------------------------------------------
// as-of resolution
// ---------------------------------------------------------------------------

// uniTemporal is the failure mode the library exists to avoid: Get ignores both
// axes and hands back the newest record it holds for the entity. It answers
// "what do we believe now" correctly and every other question wrongly.
type uniTemporal struct{ base }

func (s *uniTemporal) Get(ctx context.Context, q chronicle.GetQuery) (*chronicle.Record, error) {
	recs, _, err := s.inner().Query(ctx, chronicle.Query{Kind: q.Kind, EntityID: q.EntityID})
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, &chronicle.NotFoundError{
			Kind: q.Kind, EntityID: q.EntityID,
			As: chronicle.As{ValidAt: q.ValidAt, TxAt: q.TxAt},
		}
	}
	newest := recs[0]
	for _, r := range recs[1:] {
		if chronicle.CompareRecords(r, newest) > 0 {
			newest = r
		}
	}
	return &newest, nil
}

// hidesSuperseded returns only records that are still current belief, which is
// what a store built over an UPDATE-in-place table does. Prior belief is
// unreachable, so the log cannot be audited and no as-of query before the last
// write can be answered.
type hidesSuperseded struct{ base }

func (s *hidesSuperseded) Query(ctx context.Context, q chronicle.Query) ([]chronicle.Record, chronicle.Cursor, error) {
	recs, cursor, err := s.inner().Query(ctx, q)
	if err != nil {
		return recs, cursor, err
	}
	return slices.DeleteFunc(recs, func(r chronicle.Record) bool { return !r.IsCurrent() }), cursor, nil
}

// ---------------------------------------------------------------------------
// ordering and keyset pagination
// ---------------------------------------------------------------------------

// validFirstOrder sorts by valid start before transaction start rather than the
// other way round. The two orders agree on most fixtures, which is exactly why
// a store can ship with this bug: it only shows when generations interleave on
// the valid axis.
type validFirstOrder struct{ base }

func (s *validFirstOrder) Query(ctx context.Context, q chronicle.Query) ([]chronicle.Record, chronicle.Cursor, error) {
	recs, cursor, err := s.inner().Query(ctx, q)
	if err != nil || q.Limit > 0 {
		return recs, cursor, err
	}
	slices.SortStableFunc(recs, func(a, b chronicle.Record) int {
		if c := a.ValidFrom.Compare(b.ValidFrom); c != 0 {
			if q.Descending {
				return -c
			}
			return c
		}
		if q.Descending {
			return chronicle.CompareRecords(b, a)
		}
		return chronicle.CompareRecords(a, b)
	})
	return recs, cursor, nil
}

// pageDropsRow withholds the last row of every truncated page while still
// minting the cursor from it, so that row is returned by no page at all. A
// caller paging the log sees a result set with holes in it and no way to tell.
type pageDropsRow struct{ base }

func (s *pageDropsRow) Query(ctx context.Context, q chronicle.Query) ([]chronicle.Record, chronicle.Cursor, error) {
	recs, cursor, err := s.inner().Query(ctx, q)
	if err != nil || cursor.IsZero() || len(recs) == 0 {
		return recs, cursor, err
	}
	return recs[:len(recs)-1], cursor, nil
}

// pageRepeatsRow mints the cursor from the second-to-last row of a truncated
// page, the classic off-by-one in a keyset predicate, so the boundary row comes
// back on the next page as well.
type pageRepeatsRow struct{ base }

func (s *pageRepeatsRow) Query(ctx context.Context, q chronicle.Query) ([]chronicle.Record, chronicle.Cursor, error) {
	recs, cursor, err := s.inner().Query(ctx, q)
	if err != nil || cursor.IsZero() || len(recs) < 2 {
		return recs, cursor, err
	}
	return recs, chronicle.EncodeCursor(recs[len(recs)-2]), nil
}

// alwaysCursor offers a cursor even when it withheld nothing, forcing every
// caller into a trailing empty page and an extra round trip to discover that
// the scan was already over.
type alwaysCursor struct{ base }

func (s *alwaysCursor) Query(ctx context.Context, q chronicle.Query) ([]chronicle.Record, chronicle.Cursor, error) {
	recs, cursor, err := s.inner().Query(ctx, q)
	if err != nil || !cursor.IsZero() || len(recs) == 0 {
		return recs, cursor, err
	}
	return recs, chronicle.EncodeCursor(recs[len(recs)-1]), nil
}

// ---------------------------------------------------------------------------
// round-trip fidelity
// ---------------------------------------------------------------------------

// mapRecords returns a store whose reads run every record through fn.
type mapRecords struct {
	base
	fn func(chronicle.Record) chronicle.Record
}

func (s *mapRecords) Get(ctx context.Context, q chronicle.GetQuery) (*chronicle.Record, error) {
	rec, err := s.inner().Get(ctx, q)
	if err != nil {
		return nil, err
	}
	out := s.fn(*rec)
	return &out, nil
}

func (s *mapRecords) Query(ctx context.Context, q chronicle.Query) ([]chronicle.Record, chronicle.Cursor, error) {
	recs, cursor, err := s.inner().Query(ctx, q)
	if err != nil {
		return recs, cursor, err
	}
	for i := range recs {
		recs[i] = s.fn(recs[i])
	}
	return recs, cursor, nil
}

// sentinelValidBounds stores an unbounded valid bound as a magic timestamp and
// never maps it back, which is what happens when the columns are NOT NULL and
// somebody reaches for one. Most range predicates still agree, so it survives
// everything except a look at the round trip and a query far enough out.
type sentinelValidBounds struct{ base }

var (
	farPast   = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	farFuture = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
)

func (s *sentinelValidBounds) Apply(ctx context.Context, req chronicle.ApplyRequest) (time.Time, error) {
	return s.inner().Apply(ctx, planWith(req, func(w chronicle.Write) chronicle.Write {
		w.Insert = slices.Clone(w.Insert)
		for i := range w.Insert {
			if w.Insert[i].ValidFrom.IsZero() {
				w.Insert[i].ValidFrom = farPast
			}
			if w.Insert[i].ValidTo.IsZero() {
				w.Insert[i].ValidTo = farFuture
			}
		}
		return w
	}))
}

// aliasesRecords hands the same *Record back to every caller that asks for it,
// so one caller mutating the Data slice it was given corrupts what the next
// caller reads.
type aliasesRecords struct {
	base
	mu   sync.Mutex
	seen map[chronicle.RecordID]*chronicle.Record
}

func (s *aliasesRecords) Get(ctx context.Context, q chronicle.GetQuery) (*chronicle.Record, error) {
	rec, err := s.inner().Get(ctx, q)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.seen[rec.ID]; ok {
		return prev, nil
	}
	if s.seen == nil {
		s.seen = map[chronicle.RecordID]*chronicle.Record{}
	}
	s.seen[rec.ID] = rec
	return rec, nil
}

// ---------------------------------------------------------------------------
// error reporting
// ---------------------------------------------------------------------------

// skipsValidation answers a malformed query with an empty result instead of an
// error, so a caller who inverted an interval, or named an intent that does not
// exist, is told there is no such data rather than that the question was wrong.
type skipsValidation struct{ base }

func (s *skipsValidation) Query(ctx context.Context, q chronicle.Query) ([]chronicle.Record, chronicle.Cursor, error) {
	if err := q.Validate(); err != nil {
		return nil, chronicle.NoCursor, nil
	}
	return s.inner().Query(ctx, q)
}

// bareIntervalError reports the sentinel without the typed error that carries
// which of the two intervals was wrong, so a caller sees "invalid interval" and
// cannot tell whether it got valid time or transaction time backwards.
type bareIntervalError struct{ base }

func (s *bareIntervalError) Query(ctx context.Context, q chronicle.Query) ([]chronicle.Record, chronicle.Cursor, error) {
	if err := q.Validate(); err != nil {
		var ie *chronicle.IntervalError
		if errors.As(err, &ie) {
			return nil, chronicle.NoCursor, chronicle.ErrInvalidInterval
		}
		return nil, chronicle.NoCursor, err
	}
	return s.inner().Query(ctx, q)
}

// resumeFails answers the first page and then errors on every resumption, so a
// scan that starts fine cannot be finished.
type resumeFails struct{ base }

func (s *resumeFails) Query(ctx context.Context, q chronicle.Query) ([]chronicle.Record, chronicle.Cursor, error) {
	if !q.After.IsZero() {
		return nil, chronicle.NoCursor, errBackend
	}
	return s.inner().Query(ctx, q)
}

// acceptsBadCursor drops a cursor it cannot parse and answers from the start of
// the result set, so a caller resuming with a corrupted or truncated cursor
// silently rescans instead of being told.
type acceptsBadCursor struct{ base }

func (s *acceptsBadCursor) Query(ctx context.Context, q chronicle.Query) ([]chronicle.Record, chronicle.Cursor, error) {
	if !q.After.IsZero() {
		if _, err := chronicle.DecodeCursor(q.After); err != nil {
			q.After = chronicle.NoCursor
		}
	}
	return s.inner().Query(ctx, q)
}

// ignoresLimit returns the whole matching set however small a page was asked
// for, which is how a store that forgot to push the limit down behaves: correct
// until the log is big enough to matter.
type ignoresLimit struct{ base }

func (s *ignoresLimit) Query(ctx context.Context, q chronicle.Query) ([]chronicle.Record, chronicle.Cursor, error) {
	limit := q.Limit
	q.Limit = 0
	recs, _, err := s.inner().Query(ctx, q)
	if err != nil || limit <= 0 || len(recs) <= limit {
		return recs, chronicle.NoCursor, err
	}
	return recs, chronicle.EncodeCursor(recs[limit-1]), nil
}

// emptyAfterCursor answers any resumed query with nothing, the shape of a
// keyset predicate that excludes every row rather than the rows already seen.
type emptyAfterCursor struct{ base }

func (s *emptyAfterCursor) Query(ctx context.Context, q chronicle.Query) ([]chronicle.Record, chronicle.Cursor, error) {
	if !q.After.IsZero() {
		if _, err := chronicle.DecodeCursor(q.After); err != nil {
			return nil, chronicle.NoCursor, err
		}
		return nil, chronicle.NoCursor, nil
	}
	return s.inner().Query(ctx, q)
}

// duplicatesRows returns every record twice, the shape of a join that fans out.
// It breaks the total order as well as the counts, since a record cannot sort
// strictly before itself.
type duplicatesRows struct{ base }

func (s *duplicatesRows) Query(ctx context.Context, q chronicle.Query) ([]chronicle.Record, chronicle.Cursor, error) {
	recs, cursor, err := s.inner().Query(ctx, q)
	if err != nil || q.Limit > 0 {
		return recs, cursor, err
	}
	out := make([]chronicle.Record, 0, 2*len(recs))
	for _, r := range recs {
		out = append(out, r, r)
	}
	return out, cursor, nil
}

// strictSupersede reports a conflict for a supersession naming a record that is
// already closed or was never there, even when the write inserts nothing. The
// contract makes a bare supersession idempotent precisely so a retry after an
// ambiguous failure is safe; this store makes the retry fail.
type strictSupersede struct{ base }

func (s *strictSupersede) Apply(ctx context.Context, req chronicle.ApplyRequest) (time.Time, error) {
	live := map[chronicle.RecordID]bool{}
	if recs, _, err := s.inner().Query(ctx, chronicle.Query{CurrentOnly: true}); err == nil {
		for _, r := range recs {
			live[r.ID] = true
		}
	}
	inner := req.Plan
	req.Plan = func(current []chronicle.Record, txAt time.Time) (chronicle.Write, error) {
		w, err := inner(current, txAt)
		if err != nil {
			return w, err
		}
		if len(w.Insert) == 0 {
			for _, id := range w.Supersede {
				if !live[id] {
					return w, fmt.Errorf("chroniclefest: %s is not open for supersession", id)
				}
			}
		}
		return w, nil
	}
	return s.inner().Apply(ctx, req)
}

// rejectsDuplicateIDs surfaces the primary-key violation instead of keeping the
// record it already holds, so a retried write fails rather than being absorbed.
type rejectsDuplicateIDs struct{ base }

func (s *rejectsDuplicateIDs) Apply(ctx context.Context, req chronicle.ApplyRequest) (time.Time, error) {
	seen := map[chronicle.RecordID]bool{}
	if recs, _, err := s.inner().Query(ctx, chronicle.Query{}); err == nil {
		for _, r := range recs {
			seen[r.ID] = true
		}
	}
	inner := req.Plan
	req.Plan = func(current []chronicle.Record, txAt time.Time) (chronicle.Write, error) {
		w, err := inner(current, txAt)
		if err != nil {
			return w, err
		}
		for _, r := range w.Insert {
			if seen[r.ID] {
				return w, fmt.Errorf("chroniclefest: duplicate key %s", r.ID)
			}
		}
		return w, nil
	}
	return s.inner().Apply(ctx, req)
}

// errBackend stands in for whatever a store says when the thing underneath it
// is not there.
var errBackend = errors.New("chroniclefest: simulated backend failure")

// applyFails cannot write at all. The suite must fail loudly rather than
// mistake "nothing was asserted" for "everything held".
type applyFails struct{ base }

func (s *applyFails) Apply(context.Context, chronicle.ApplyRequest) (time.Time, error) {
	return time.Time{}, errBackend
}

// readsFail can write but cannot read, which is the other half of the same
// concern: a suite that skips its assertions when a read errors would report
// success for a store nobody can query.
type readsFail struct{ base }

func (s *readsFail) Get(context.Context, chronicle.GetQuery) (*chronicle.Record, error) {
	return nil, errBackend
}

func (s *readsFail) Query(context.Context, chronicle.Query) ([]chronicle.Record, chronicle.Cursor, error) {
	return nil, chronicle.NoCursor, errBackend
}

// exclusiveLowerBound treats a valid interval as open at the bottom as well as
// the top, so the one instant a record most obviously covers is the one it
// reports nothing for.
type exclusiveLowerBound struct{ base }

func (s *exclusiveLowerBound) Get(ctx context.Context, q chronicle.GetQuery) (*chronicle.Record, error) {
	rec, err := s.inner().Get(ctx, q)
	if err != nil {
		return nil, err
	}
	if rec.ValidFrom.Equal(q.ValidAt) {
		return nil, &chronicle.NotFoundError{
			Kind: q.Kind, EntityID: q.EntityID,
			As: chronicle.As{ValidAt: q.ValidAt, TxAt: q.TxAt},
		}
	}
	return rec, nil
}

// getIgnoresIdentity resolves a lookup against the whole log rather than the
// entity that was named, so one entity's data is served under another's
// coordinates and under another kind entirely.
type getIgnoresIdentity struct{ base }

func (s *getIgnoresIdentity) Get(ctx context.Context, q chronicle.GetQuery) (*chronicle.Record, error) {
	recs, _, err := s.inner().Query(ctx, chronicle.Query{ValidAt: q.ValidAt, TxAt: q.TxAt})
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, &chronicle.NotFoundError{
			Kind: q.Kind, EntityID: q.EntityID,
			As: chronicle.As{ValidAt: q.ValidAt, TxAt: q.TxAt},
		}
	}
	return &recs[0], nil
}

// ignoresCancellation substitutes a live context for a cancelled one, so work
// the caller has abandoned still lands.
type ignoresCancellation struct{ base }

func (s *ignoresCancellation) Apply(_ context.Context, req chronicle.ApplyRequest) (time.Time, error) {
	return s.inner().Apply(context.Background(), req)
}

func (s *ignoresCancellation) Get(_ context.Context, q chronicle.GetQuery) (*chronicle.Record, error) {
	return s.inner().Get(context.Background(), q)
}

func (s *ignoresCancellation) Query(_ context.Context, q chronicle.Query) ([]chronicle.Record, chronicle.Cursor, error) {
	return s.inner().Query(context.Background(), q)
}

// ---------------------------------------------------------------------------
// invariants the suite did not check until fault injection found the gap
// ---------------------------------------------------------------------------

// nullsLastOrder sorts records whose valid start is unbounded after the rest,
// which is what ORDER BY valid_from does in Postgres when unbounded is NULL and
// nobody wrote NULLS FIRST.
type nullsLastOrder struct{ base }

func (s *nullsLastOrder) Query(ctx context.Context, q chronicle.Query) ([]chronicle.Record, chronicle.Cursor, error) {
	recs, cursor, err := s.inner().Query(ctx, q)
	if err != nil {
		return recs, cursor, err
	}
	slices.SortStableFunc(recs, func(a, b chronicle.Record) int {
		c := 0
		switch {
		case a.ValidFrom.IsZero() && !b.ValidFrom.IsZero():
			c = 1
		case !a.ValidFrom.IsZero() && b.ValidFrom.IsZero():
			c = -1
		}
		if q.Descending {
			return -c
		}
		return c
	})
	return recs, cursor, nil
}

// honoursIncomingTxFrom behaves as a store that stamps an inserted record with
// whatever TxFrom it arrived carrying instead of the instant the store
// assigned. Since MemStore always overwrites, the fault is reproduced on the
// way out, which is indistinguishable from the caller's side.
type honoursIncomingTxFrom struct {
	base
	mu     sync.Mutex
	claims map[chronicle.RecordID]time.Time
}

func (s *honoursIncomingTxFrom) Apply(ctx context.Context, req chronicle.ApplyRequest) (time.Time, error) {
	return s.inner().Apply(ctx, planWith(req, func(w chronicle.Write) chronicle.Write {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.claims == nil {
			s.claims = map[chronicle.RecordID]time.Time{}
		}
		for _, r := range w.Insert {
			if !r.TxFrom.IsZero() {
				s.claims[r.ID] = r.TxFrom
			}
		}
		return w
	}))
}

func (s *honoursIncomingTxFrom) rewrite(r chronicle.Record) chronicle.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	if claimed, ok := s.claims[r.ID]; ok {
		r.TxFrom = claimed
	}
	return r
}

func (s *honoursIncomingTxFrom) Get(ctx context.Context, q chronicle.GetQuery) (*chronicle.Record, error) {
	rec, err := s.inner().Get(ctx, q)
	if err != nil {
		return nil, err
	}
	out := s.rewrite(*rec)
	return &out, nil
}

func (s *honoursIncomingTxFrom) Query(ctx context.Context, q chronicle.Query) ([]chronicle.Record, chronicle.Cursor, error) {
	recs, cursor, err := s.inner().Query(ctx, q)
	if err != nil {
		return recs, cursor, err
	}
	for i := range recs {
		recs[i] = s.rewrite(recs[i])
	}
	return recs, cursor, nil
}

// truncatesValidToTheDay keeps valid times only to the day, which the suite's
// own documentation calls out of contract.
type truncatesValidToTheDay struct{ base }

func (s *truncatesValidToTheDay) Apply(ctx context.Context, req chronicle.ApplyRequest) (time.Time, error) {
	day := func(t time.Time) time.Time {
		if t.IsZero() {
			return t
		}
		return t.UTC().Truncate(24 * time.Hour)
	}
	return s.inner().Apply(ctx, planWith(req, func(w chronicle.Write) chronicle.Write {
		w.Insert = slices.Clone(w.Insert)
		for i := range w.Insert {
			w.Insert[i].ValidFrom = day(w.Insert[i].ValidFrom)
			w.Insert[i].ValidTo = day(w.Insert[i].ValidTo)
		}
		return w
	}))
}

// wrongNotFoundCoordinates reports a not-found error naming an entity nobody
// asked about, so the error is useless for telling two concurrent lookups apart
// in a log.
type wrongNotFoundCoordinates struct{ base }

func (s *wrongNotFoundCoordinates) Get(ctx context.Context, q chronicle.GetQuery) (*chronicle.Record, error) {
	rec, err := s.inner().Get(ctx, q)
	if errors.Is(err, chronicle.ErrNotFound) {
		return nil, &chronicle.NotFoundError{
			Kind: "some-other-kind", EntityID: "some-other-entity",
			As: chronicle.As{ValidAt: q.ValidAt, TxAt: q.TxAt},
		}
	}
	return rec, err
}

// phantomRow invents a record whenever a query matches nothing. Answering an
// empty question with something is worse than answering it with an error: the
// caller has no way to tell.
type phantomRow struct{ base }

func (s *phantomRow) Query(ctx context.Context, q chronicle.Query) ([]chronicle.Record, chronicle.Cursor, error) {
	recs, cursor, err := s.inner().Query(ctx, q)
	if err != nil || len(recs) > 0 {
		return recs, cursor, err
	}
	ghost := chronicle.Record{
		ID: "phantom", Kind: q.Kind, EntityID: q.EntityID,
		Data: []byte("{}"), TxFrom: frozen, Actor: chronicle.Actor{ID: "u-nobody"},
	}
	return []chronicle.Record{ghost}, chronicle.EncodeCursor(ghost), nil
}

// splitApplyFails cannot apply a write that both supersedes and inserts, which
// is a store that never implemented the indivisible case — the only case the
// interface exists for.
type splitApplyFails struct{ base }

func (s *splitApplyFails) Apply(ctx context.Context, req chronicle.ApplyRequest) (time.Time, error) {
	inner := req.Plan
	req.Plan = func(current []chronicle.Record, txAt time.Time) (chronicle.Write, error) {
		w, err := inner(current, txAt)
		if err != nil {
			return w, err
		}
		if len(w.Supersede) > 0 && len(w.Insert) > 0 {
			return w, errBackend
		}
		return w, nil
	}
	return s.inner().Apply(ctx, req)
}
