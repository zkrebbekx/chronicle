package chronicle

import (
	"context"
	"slices"
	"strings"
	"time"
)

// This file is MemStore's implementation of the optional compliance
// capabilities: [Deleter] and [HoldStore]. They live apart from the core
// store because they are apart from it in kind — everything in memstore.go
// preserves history, and everything here either destroys it on instruction or
// restrains that destruction.

// Delete implements [Deleter]. It destroys the named records, records a
// [Tombstone] for each one whose metadata carries [MetaChain], and refuses the
// whole batch if any named record is still current belief.
func (s *MemStore) Delete(ctx context.Context, ids []RecordID) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}

	// Refusal comes first and is all-or-nothing: a batch that would destroy
	// current belief destroys nothing at all.
	doomed := make(map[RecordID]struct{}, len(ids))
	for _, id := range ids {
		idx, ok := s.byID[id]
		if !ok {
			continue // already gone; deletion is idempotent
		}
		if s.recs[idx].IsCurrent() {
			return 0, &DeleteError{RecordID: id, Err: ErrCurrentRecord}
		}
		doomed[id] = struct{}{}
	}
	if len(doomed) == 0 {
		return 0, nil
	}

	now := time.Now().UTC()
	kept := make([]Record, 0, len(s.recs)-len(doomed))
	for _, r := range s.recs {
		if _, dead := doomed[r.ID]; !dead {
			kept = append(kept, r)
			continue
		}
		if hash, chained := r.Meta[MetaChain]; chained {
			key := entityKey{kind: r.Kind, entityID: r.EntityID}
			s.tombs[key] = append(s.tombs[key], Tombstone{
				Kind:      r.Kind,
				EntityID:  r.EntityID,
				RecordID:  r.ID,
				ValidFrom: r.ValidFrom,
				TxFrom:    r.TxFrom,
				ChainHash: hash,
				DeletedAt: now,
			})
		}
	}
	s.recs = kept
	s.reindexLocked()
	return len(doomed), nil
}

// reindexLocked rebuilds the ID and entity indexes after the record slice has
// been compacted. Linear in the store, which is the right trade for a
// reference implementation: deletion is a rare administrative sweep, not a
// hot path.
func (s *MemStore) reindexLocked() {
	s.byID = make(map[RecordID]int, len(s.recs))
	s.byEntry = make(map[entityKey][]int, len(s.byEntry))
	for i, r := range s.recs {
		s.byID[r.ID] = i
		key := entityKey{kind: r.Kind, entityID: r.EntityID}
		s.byEntry[key] = append(s.byEntry[key], i)
	}
}

// Tombstones implements [Deleter], returning one entity's tombstones in chain
// order.
func (s *MemStore) Tombstones(ctx context.Context, kind, entityID string) ([]Tombstone, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if kind == "" {
		return nil, &KindError{Err: ErrUnknownKind}
	}
	if entityID == "" {
		return nil, ErrMissingEntityID
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrClosed
	}
	out := slices.Clone(s.tombs[entityKey{kind: kind, entityID: entityID}])
	slices.SortFunc(out, compareTombstones)
	return out, nil
}

// compareTombstones orders tombstones by chronicle's total order — the
// position the destroyed record used to occupy.
func compareTombstones(a, b Tombstone) int {
	if c := a.TxFrom.Compare(b.TxFrom); c != 0 {
		return c
	}
	if c := compareStarts(a.ValidFrom, b.ValidFrom); c != 0 {
		return c
	}
	return strings.Compare(string(a.RecordID), string(b.RecordID))
}

// PlaceHold implements [HoldStore]. PlacedAt is assigned here, and the release
// fields are cleared whatever the caller put in them: placement writes the
// placement half of the hold and nothing else.
func (s *MemStore) PlaceHold(ctx context.Context, h Hold) (Hold, error) {
	if err := ctx.Err(); err != nil {
		return Hold{}, err
	}
	if err := h.Validate(); err != nil {
		return Hold{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Hold{}, ErrClosed
	}
	if _, exists := s.holdIdx[h.ID]; exists {
		return Hold{}, &HoldError{ID: h.ID, Err: ErrHoldExists}
	}

	h.EffectiveFrom = utcOrZero(h.EffectiveFrom)
	h.PlacedAt = time.Now().UTC()
	h.ReleasedAt = time.Time{}
	h.ReleasedBy = Actor{}
	h.ReleaseReason = ""

	s.holds = append(s.holds, h)
	s.holdIdx[h.ID] = len(s.holds) - 1
	return h, nil
}

// ReleaseHold implements [HoldStore]. The hold's row survives with its release
// fields set; releasing twice is an error rather than a quiet no-op.
func (s *MemStore) ReleaseHold(ctx context.Context, id string, by Actor, reason string) (Hold, error) {
	if err := ctx.Err(); err != nil {
		return Hold{}, err
	}
	if id == "" {
		return Hold{}, &HoldError{Err: ErrMissingHoldID}
	}
	if by.ID == "" {
		return Hold{}, &HoldError{ID: id, Err: ErrMissingActor}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Hold{}, ErrClosed
	}
	idx, ok := s.holdIdx[id]
	if !ok {
		return Hold{}, &HoldError{ID: id, Err: ErrNotFound}
	}
	if !s.holds[idx].ReleasedAt.IsZero() {
		return Hold{}, &HoldError{ID: id, Err: ErrHoldReleased}
	}
	s.holds[idx].ReleasedAt = time.Now().UTC()
	s.holds[idx].ReleasedBy = by
	s.holds[idx].ReleaseReason = reason
	return s.holds[idx], nil
}

// Holds implements [HoldStore], returning every hold ever placed — released
// ones included — in placement order.
func (s *MemStore) Holds(ctx context.Context) ([]Hold, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrClosed
	}
	return slices.Clone(s.holds), nil
}

// utcOrZero converts to UTC while preserving the zero time's meaning.
func utcOrZero(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}

// Compile-time assertions that MemStore carries the compliance capabilities.
var (
	_ Deleter   = (*MemStore)(nil)
	_ HoldStore = (*MemStore)(nil)
)
