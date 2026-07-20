package chronicle

import (
	"context"
	"slices"
	"sync"
	"time"
)

// MemStore is an in-memory [Store], and the reference implementation of the
// storage contract. It is the store to test against and a reasonable choice
// for callers who want bitemporal semantics without a database, but it holds
// the entire log in memory and never evicts, so it is not a durability story.
//
// It is safe for concurrent use, and writes are indivisible: a reader either
// sees the whole of a write — every supersession and every insertion — or none
// of it.
//
// Records are deep-copied on the way in and on the way out. A caller cannot
// reach into the log by holding on to the Data slice or Meta map it passed to
// [Log.Put], and cannot corrupt it by mutating a record it read back.
type MemStore struct {
	mu      sync.RWMutex
	recs    []Record
	byID    map[RecordID]int
	byEntry map[entityKey][]int
	closed  bool
}

type entityKey struct {
	kind, entityID string
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{
		byID:    make(map[RecordID]int),
		byEntry: make(map[entityKey][]int),
	}
}

// Len returns the number of records held, including superseded ones. Intended
// for tests and diagnostics.
func (s *MemStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.recs)
}

// Close releases the store's contents. Subsequent operations report
// [ErrClosed]. Closing twice is not an error.
func (s *MemStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.recs = nil
	s.byID = nil
	s.byEntry = nil
	return nil
}

// Apply implements [Store]. The whole write happens under a single lock, so
// no reader can observe the moment between closing the old records and
// inserting the new ones — which is the moment at which an entity's valid-time
// coverage would appear to have a hole in it.
//
// MemStore accepts the log's proposed transaction instant and returns it
// unchanged. It can: a MemStore has exactly one process writing to it, so the
// log's ratchet is authoritative. A store shared between processes must assign
// transaction time itself.
func (s *MemStore) Apply(ctx context.Context, w Write) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	txAt := w.TxAt.UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.supersedeLocked(w.Supersede, txAt, len(w.Insert) > 0); err != nil {
		return time.Time{}, err
	}
	if err := s.insertLocked(w.Insert, txAt); err != nil {
		return time.Time{}, err
	}
	return txAt, nil
}

// insertLocked adds records, stamping each with the write's transaction
// instant. The stamp is not negotiable: transaction time is the store's to
// assign, and honouring an incoming TxFrom would give callers a way to say when
// the log appears to have learned something.
func (s *MemStore) insertLocked(recs []Record, txFrom time.Time) error {
	if s.closed {
		return ErrClosed
	}
	for _, r := range recs {
		if _, exists := s.byID[r.ID]; exists {
			// Re-inserting an existing ID would duplicate history, and
			// overwriting is not an option for an append-only log, so this
			// keeps the original and moves on.
			continue
		}
		clone := r.Clone()
		clone.TxFrom = txFrom
		s.recs = append(s.recs, clone)
		idx := len(s.recs) - 1
		s.byID[clone.ID] = idx
		key := entityKey{kind: clone.Kind, entityID: clone.EntityID}
		s.byEntry[key] = append(s.byEntry[key], idx)
	}
	return nil
}

// supersedeLocked closes the named records' transaction intervals. It never
// rewrites a timestamp already assigned, which is what makes a retried write
// safe.
//
// strict says the write also inserts records, and so is one half of a split.
// Finding a target already closed then means the split was planned against a
// pre-state that has since moved, and applying the other half would leave the
// entity's timeline overlapping. That is [ErrConflict], not a no-op. A
// supersession on its own stays idempotent.
func (s *MemStore) supersedeLocked(ids []RecordID, txTo time.Time, strict bool) error {
	if s.closed {
		return ErrClosed
	}
	for _, id := range ids {
		idx, ok := s.byID[id]
		if !ok {
			if strict {
				return conflictf("record %s no longer exists", id)
			}
			continue
		}
		if !s.recs[idx].TxTo.IsZero() {
			if strict {
				return conflictf("record %s was already superseded", id)
			}
			continue
		}
		s.recs[idx].TxTo = txTo.UTC()
	}
	return nil
}

// Get implements [Store]. Where the log's non-overlap invariant holds at most
// one record can match; if several somehow do, the earliest in chronicle's
// total order is returned, so the result is deterministic either way.
func (s *MemStore) Get(ctx context.Context, q GetQuery) (*Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrClosed
	}

	var best *Record
	for _, idx := range s.byEntry[entityKey{kind: q.Kind, entityID: q.EntityID}] {
		r := s.recs[idx]
		if !r.Valid().Contains(q.ValidAt) || !r.Tx().Contains(q.TxAt) {
			continue
		}
		if best == nil || compareRecords(r, *best) < 0 {
			clone := r.Clone()
			best = &clone
		}
	}
	if best == nil {
		return nil, &NotFoundError{Kind: q.Kind, EntityID: q.EntityID, As: As{ValidAt: q.ValidAt, TxAt: q.TxAt}}
	}
	return best, nil
}

// Query implements [Store].
//
// The scan is linear over the candidate set, narrowed by the entity index when
// the query names one. That is the right shape for a reference implementation
// and the wrong shape for a large log; a SQL store pushes the same predicates,
// the same ordering and the same keyset resumption into the database.
func (s *MemStore) Query(ctx context.Context, q Query) ([]Record, Cursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, NoCursor, err
	}
	if err := q.validate(); err != nil {
		return nil, NoCursor, err
	}

	var key cursorKey
	haveCursor := !q.After.IsZero()
	if haveCursor {
		k, err := decodeCursor(q.After)
		if err != nil {
			return nil, NoCursor, err
		}
		key = k
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, NoCursor, ErrClosed
	}

	// Matching and sorting work on shallow copies, which share Data and Meta
	// with the stored records and so cost nothing to carry around. Only the
	// records that survive the limit are deep-copied, so paging a large log
	// at a small page size does not clone the whole result set per page.
	var matched []Record
	for _, r := range s.candidatesLocked(q) {
		if !q.matches(r) {
			continue
		}
		if haveCursor && !key.after(r, q.Descending) {
			continue
		}
		matched = append(matched, r)
	}

	slices.SortFunc(matched, func(a, b Record) int {
		if q.Descending {
			return compareRecords(b, a)
		}
		return compareRecords(a, b)
	})

	// A cursor is returned only when records were actually withheld. Callers
	// therefore terminate on an empty cursor without needing a trailing empty
	// page.
	truncated := q.Limit > 0 && len(matched) > q.Limit
	if truncated {
		matched = matched[:q.Limit]
	}

	out := cloneRecords(matched)
	if truncated {
		return out, encodeCursor(out[len(out)-1]), nil
	}
	return out, NoCursor, nil
}

// candidatesLocked narrows the scan using the per-entity index when the query
// pins an entity, and otherwise returns everything.
func (s *MemStore) candidatesLocked(q Query) []Record {
	if q.Kind == "" || q.EntityID == "" {
		return s.recs
	}
	idxs := s.byEntry[entityKey{kind: q.Kind, entityID: q.EntityID}]
	out := make([]Record, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, s.recs[i])
	}
	return out
}

// Compile-time assertion that MemStore satisfies the storage contract.
var _ Store = (*MemStore)(nil)
