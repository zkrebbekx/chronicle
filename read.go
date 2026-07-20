package chronicle

import (
	"context"
	"errors"
	"slices"
	"time"
)

// As is a point in bitemporal space: an instant on each axis.
//
// A zero field means "now", resolved when the read is made. The zero As is
// therefore the ordinary current-state question — what is true, according to
// what we currently believe — and the two axes are set independently, because
// they answer genuinely different questions:
//
//	As{}                          // what is Alice's salary?
//	As{ValidAt: march}            // what was it in March?
//	As{ValidAt: march, TxAt: april} // what did we believe in April it had
//	                                // been in March?
//
// The third is the one uni-temporal systems get wrong. They answer it with
// today's belief about March, because a retroactive correction rewrote what
// they appear to have known, and the answer looks entirely reasonable.
type As struct {
	// ValidAt is the instant on the valid axis — when in the world.
	ValidAt time.Time
	// TxAt is the instant on the transaction axis — as known when.
	TxAt time.Time
}

// Now is the zero [As]: current state, current belief.
func Now() As { return As{} }

// ValidAt returns an [As] at the given valid instant, at current belief.
func ValidAt(t time.Time) As { return As{ValidAt: t} }

// AsOf returns an [As] at the given transaction instant, asking about the
// state valid at that same instant — "what did the world look like to us
// then".
func AsOf(tx time.Time) As { return As{ValidAt: tx, TxAt: tx} }

// IsZero reports whether both axes are unset.
func (a As) IsZero() bool { return a.ValidAt.IsZero() && a.TxAt.IsZero() }

// resolve substitutes now for any unset axis and normalises to UTC.
func (a As) resolve(now time.Time) As {
	if a.ValidAt.IsZero() {
		a.ValidAt = now
	} else {
		a.ValidAt = a.ValidAt.UTC()
	}
	if a.TxAt.IsZero() {
		a.TxAt = now
	} else {
		a.TxAt = a.TxAt.UTC()
	}
	return a
}

// Get returns the single record in force for the entity at the given point on
// both axes, or an error wrapping [ErrNotFound] if the entity had no state
// there.
//
// Because current records never overlap in valid time, at most one record can
// satisfy any (ValidAt, TxAt) pair, so this is a lookup rather than a search
// that happens to return one row.
func (l *Log) Get(ctx context.Context, kind, entityID string, as As) (*Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := l.checkKind(kind); err != nil {
		return nil, err
	}
	if entityID == "" {
		return nil, ErrMissingEntityID
	}
	as = as.resolve(l.now())
	rec, err := l.store.Get(ctx, GetQuery{
		Kind:     kind,
		EntityID: entityID,
		ValidAt:  as.ValidAt,
		TxAt:     as.TxAt,
	})
	if err != nil {
		return nil, err
	}
	// Get answers "what was the state", so an encrypted record is decrypted —
	// and a shredded one is a loud [*ShredError], never returned ciphertext.
	// History, Timeline and Query return records as stored; see [Log.Decrypt].
	return l.decryptRecord(ctx, rec)
}

// HistoryOption filters [Log.History].
type HistoryOption func(*Query)

// InValidRange restricts history to records whose valid interval overlaps iv.
func InValidRange(iv Interval) HistoryOption {
	return func(q *Query) { q.Valid = iv.UTC() }
}

// InTxRange restricts history to records whose transaction interval overlaps
// iv — that is, to beliefs held at some point during that window.
func InTxRange(iv Interval) HistoryOption {
	return func(q *Query) { q.Tx = iv.UTC() }
}

// ByActor restricts history to writes attributed to one actor.
func ByActor(actorID string) HistoryOption {
	return func(q *Query) { q.ActorID = actorID }
}

// WithIntent restricts history to one intent — [IntentCorrection] to see only
// the retroactive fixes, for instance.
func WithIntent(i Intent) HistoryOption {
	return func(q *Query) {
		q.Intent = i
		q.HasIntent = true
	}
}

// CurrentOnly restricts history to records that are still current belief.
func CurrentOnly() HistoryOption {
	return func(q *Query) { q.CurrentOnly = true }
}

// Limit caps the number of records returned. Use [Log.Query] when you need to
// page rather than truncate.
func Limit(n int) HistoryOption {
	return func(q *Query) { q.Limit = n }
}

// Descending reverses the result order, newest belief first.
func Descending() HistoryOption {
	return func(q *Query) { q.Descending = true }
}

// History returns every version of an entity ever recorded, superseded ones
// included, ordered by transaction start then valid start.
//
// This is the raw log for one entity. Unlike [Log.Get] it reports no error
// when the entity is unknown — an entity with no history has an empty history,
// which is a fact rather than a failure.
func (l *Log) History(ctx context.Context, kind, entityID string, opts ...HistoryOption) ([]Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := l.checkKind(kind); err != nil {
		return nil, err
	}
	if entityID == "" {
		return nil, ErrMissingEntityID
	}

	q := Query{Kind: kind, EntityID: entityID}
	for _, opt := range opts {
		opt(&q)
	}
	// The entity is not negotiable, whatever the options did.
	q.Kind, q.EntityID = kind, entityID

	recs, _, err := l.store.Query(ctx, q)
	return recs, err
}

// Timeline returns the entity's valid-time sequence as believed at a single
// transaction instant: the run of records that tiled its timeline at that
// moment, ordered by valid start.
//
// Only the TxAt axis of as is used. A timeline is a slice through valid time
// at one belief instant, so pinning a valid instant as well would reduce it to
// [Log.Get]; ValidAt is ignored rather than quietly narrowing the result.
//
// Taken at two different TxAt values, this is the clearest view of what a
// correction actually did — the same stretch of valid time, tiled differently.
func (l *Log) Timeline(ctx context.Context, kind, entityID string, as As) ([]Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := l.checkKind(kind); err != nil {
		return nil, err
	}
	if entityID == "" {
		return nil, ErrMissingEntityID
	}

	txAt := as.TxAt
	if txAt.IsZero() {
		txAt = l.now()
	} else {
		txAt = txAt.UTC()
	}

	recs, _, err := l.store.Query(ctx, Query{Kind: kind, EntityID: entityID, TxAt: txAt})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(recs, func(a, b Record) int {
		if c := compareStarts(a.ValidFrom, b.ValidFrom); c != 0 {
			return c
		}
		return CompareRecords(a, b)
	})
	return recs, nil
}

// Query returns records across entities, filtered on either axis and paged
// with an opaque cursor.
//
// A zero instant in q means "no restriction", not "now" — a cross-entity
// query is a scan over the log rather than a question about the present, and
// defaulting its time filters to the current instant would silently hide
// everything superseded. Use [Query.CurrentOnly], or set TxAt explicitly, to
// ask about a particular belief instant.
//
// The returned cursor is empty when the result set is exhausted, so the
// idiomatic loop terminates without a trailing empty page:
//
//	var cursor chronicle.Cursor
//	for {
//	    page, next, err := log.Query(ctx, chronicle.Query{Kind: "employee", Limit: 100, After: cursor})
//	    if err != nil {
//	        return err
//	    }
//	    // ... use page ...
//	    if next.IsZero() {
//	        break
//	    }
//	    cursor = next
//	}
func (l *Log) Query(ctx context.Context, q Query) ([]Record, Cursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, NoCursor, err
	}
	if q.Kind != "" {
		if err := l.checkKind(q.Kind); err != nil {
			return nil, NoCursor, err
		}
	}
	q.Valid = q.Valid.UTC()
	q.Tx = q.Tx.UTC()
	if !q.ValidAt.IsZero() {
		q.ValidAt = q.ValidAt.UTC()
	}
	if !q.TxAt.IsZero() {
		q.TxAt = q.TxAt.UTC()
	}
	return l.store.Query(ctx, q)
}

// Diff reports the field-level changes in an entity's state between two points
// in bitemporal space.
//
// Both points are resolved independently, so this compares across either axis
// or both at once. Holding ValidAt fixed and moving TxAt shows what a
// correction changed about a single moment in the world; holding TxAt fixed
// and moving ValidAt shows what actually happened to the entity over time,
// according to one consistent belief.
//
// If the entity has no state at one of the points, every field is reported as
// added or removed accordingly. If it has none at either, the result is an
// error wrapping [ErrNotFound]. If the codec cannot decode a record that does
// exist, that is a [*CodecError] wrapping [ErrCodec] — never an empty diff,
// because a change log that reports "nothing changed" when it means "I could
// not tell" is worse than one that fails.
func (l *Log) Diff(ctx context.Context, kind, entityID string, from, to As) (Delta, error) {
	if err := ctx.Err(); err != nil {
		return Delta{}, err
	}
	if err := l.checkKind(kind); err != nil {
		return Delta{}, err
	}
	if entityID == "" {
		return Delta{}, ErrMissingEntityID
	}

	now := l.now()
	from, to = from.resolve(now), to.resolve(now)

	fromRec, err := l.getOrNil(ctx, kind, entityID, from)
	if err != nil {
		return Delta{}, err
	}
	toRec, err := l.getOrNil(ctx, kind, entityID, to)
	if err != nil {
		return Delta{}, err
	}
	if fromRec == nil && toRec == nil {
		return Delta{}, &NotFoundError{Kind: kind, EntityID: entityID, As: from}
	}

	fromVals, err := l.decode(fromRec)
	if err != nil {
		return Delta{}, err
	}
	toVals, err := l.decode(toRec)
	if err != nil {
		return Delta{}, err
	}

	var changes []FieldChange
	diffMaps("", fromVals, toVals, &changes)

	return Delta{
		Kind:       kind,
		EntityID:   entityID,
		From:       from,
		To:         to,
		FromRecord: fromRec,
		ToRecord:   toRec,
		Changes:    changes,
	}, nil
}

// getOrNil resolves a point to a record, treating absence as nil rather than
// as an error. Absence is a legitimate diff operand: it is what an entity
// looks like before it existed.
func (l *Log) getOrNil(ctx context.Context, kind, entityID string, as As) (*Record, error) {
	rec, err := l.store.Get(ctx, GetQuery{
		Kind:     kind,
		EntityID: entityID,
		ValidAt:  as.ValidAt,
		TxAt:     as.TxAt,
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	// Diff compares states, so its operands are decrypted like [Log.Get]'s
	// result. A shredded operand fails the whole diff: reporting "no changes"
	// against ciphertext would be under-reporting, the one failure mode a
	// change log must not have.
	return l.decryptRecord(ctx, rec)
}

// decode turns a record into comparable values. A nil record decodes to an
// empty map, which is what makes "the entity did not exist yet" diff as a set
// of additions.
func (l *Log) decode(r *Record) (map[string]any, error) {
	if r == nil {
		return map[string]any{}, nil
	}
	if l.codec == nil {
		return nil, &CodecError{Codec: "none", RecordID: r.ID, Err: ErrCodec}
	}
	vals, err := l.codec.Decode(r.Data)
	if err != nil {
		return nil, &CodecError{Codec: l.codec.Name(), RecordID: r.ID, Err: err}
	}
	if vals == nil {
		vals = map[string]any{}
	}
	return vals, nil
}
