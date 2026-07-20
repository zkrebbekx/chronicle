package chronicle

import (
	"context"
	"slices"
	"time"
)

// FieldValue is the value at one JSON Pointer path in a decoded record, with
// the one distinction that makes a single field's history correct: a field
// that is not in the object is not the same as a field explicitly set to JSON
// null.
//
// Present reports whether the path resolved to anything at all. When it is
// false the field was absent — the object did not contain the path — and Value
// is nil and meaningless. When it is true the field was there, and Value holds
// the decoded value, which may itself be nil for an explicit JSON null. So an
// absent field is FieldValue{Present: false} and a null field is
// FieldValue{Present: true, Value: nil}, and [Log.FieldHistory] treats a move
// between them as a real change, because it is one.
//
// Value follows the same representation [Log.Diff] uses: scalars decoded by the
// [Codec] (numbers as [encoding/json.Number] so notation does not read as a
// change), []any for arrays and map[string]any for objects, nested to any
// depth. A path that lands on a whole subtree carries that subtree here.
type FieldValue struct {
	// Value is the decoded value at the path, meaningful only when Present.
	// A present nil is an explicit JSON null.
	Value any
	// Present reports whether the path existed in the object at all.
	Present bool
}

// IsNull reports whether the field was present and explicitly JSON null, as
// opposed to absent (Present false) or present with a value.
func (v FieldValue) IsNull() bool { return v.Present && v.Value == nil }

// equal reports whether two field values are the same, by the same rule
// [Log.Diff] compares by: presence must match, and where both are present the
// values must be structurally equal with numbers compared by value rather than
// by notation. It is the definition of "did this field change", and sharing it
// with Diff is what keeps FieldHistory from ever reporting a change Diff would
// not — a re-recorded 100.0 after 100 is not a change here either.
func (v FieldValue) equal(o FieldValue) bool {
	if v.Present != o.Present {
		return false
	}
	if !v.Present {
		return true
	}
	return valuesEqual(v.Value, o.Value)
}

// valuesEqual reports whether two decoded values are structurally equal,
// reusing Diff's own descent so the two features cannot disagree. Two values
// are equal exactly when diffing them yields no changes.
func valuesEqual(a, b any) bool {
	var changes []FieldChange
	diffValues("", a, b, &changes)
	return len(changes) == 0
}

// FieldRevision is one recorded change to a single field, on the transaction
// axis, at a fixed point in valid time. It is the element of a [Log.FieldHistory]
// result — the answer to "how did our belief about this field evolve, and who
// changed it, and was it an assertion or a correction".
//
// The name is [FieldRevision] rather than FieldChange because [FieldChange] is
// already the element of a [Delta] — a spatial diff between two states, carrying
// an add/remove/modify Op. A field's history is a different shape: it carries no
// Op (every element is a change, by construction), it distinguishes absent from
// null on both sides, and it carries the attribution and the two time
// coordinates of the belief that introduced the change. Overloading one name
// with both meanings would have been the more confusing choice.
type FieldRevision struct {
	// Path is the RFC 6901 JSON Pointer the history was taken for, echoed on
	// every revision.
	Path string
	// From is the field's value in the belief immediately before this one, on
	// the transaction axis, at the fixed valid point. For the first revision it
	// is the absent value (Present false): before anything was recorded the
	// field did not exist.
	From FieldValue
	// To is the field's value in the belief this revision records.
	To FieldValue
	// TxAt is the transaction instant this belief was recorded — the TxFrom of
	// the record that introduced the new value. It answers "when did we come to
	// believe this", which for a correction is "when did we discover we were
	// wrong".
	TxAt time.Time
	// ValidFrom and ValidTo are the valid interval of the record that introduced
	// this belief. The fixed valid point the history was taken at always falls
	// within [ValidFrom, ValidTo); the bounds show how much of the world around
	// it the same belief covered.
	ValidFrom time.Time
	ValidTo   time.Time
	// Actor is who recorded the introducing belief.
	Actor Actor
	// Reason is the free-text justification on the introducing record, if any.
	Reason string
	// Intent distinguishes an original assertion ([IntentAssert]) from a
	// correction of a prior belief ([IntentCorrection]) — the distinction that
	// makes "when did we discover the salary was wrong" answerable. A revision
	// can also be introduced by a remainder ([IntentRemainder]) chronicle wrote
	// when splitting an interval, though a remainder re-asserts existing data
	// and so rarely changes a field's value at the split point.
	Intent Intent
}

// FieldHistoryOption configures a [Log.FieldHistory] call.
type FieldHistoryOption func(*fieldHistoryOpts)

type fieldHistoryOpts struct {
	descending bool
}

// FieldHistoryDescending reverses the result of [Log.FieldHistory] so the most
// recently recorded revision comes first. The default is transaction-time
// ascending. Only the order of the slice changes; each revision still reads
// From then To in forward transaction time, so "100 corrected to 120" is
// From=100, To=120 whichever way the list runs.
func FieldHistoryDescending() FieldHistoryOption {
	return func(o *fieldHistoryOpts) { o.descending = true }
}

// fieldHistoryPage is how many records FieldHistory pulls from the store per
// query. It walks the covering records in order and keeps only the previous
// value between them, so memory is bounded by one page plus the revisions it
// emits, whatever the length of the entity's transaction history.
const fieldHistoryPage = 256

// FieldHistory walks how our belief about a single field of one entity evolved
// over transaction time, holding the valid-time point fixed.
//
// This is the bitemporal read the whole library exists to make possible, in its
// most focused form. as.ValidAt pins a point in the world — a day, an instant —
// and the result answers: for the entity's state as it was (or is) valid at that
// point, how did the recorded value of path change as we learned more, and who
// changed it. It is a walk along the *transaction* axis at a fixed valid point,
// not along valid time. Only as.ValidAt is used; as.TxAt is ignored, the mirror
// image of [Log.Timeline], which uses only as.TxAt. A zero as.ValidAt means now.
//
// path is an RFC 6901 JSON Pointer in the same grammar [Log.Diff] emits:
// "/salary", "/address/city", "/tags/0", with "~0" for a literal "~" and "~1"
// for a literal "/". The empty path is the whole document. A path that is not a
// well-formed pointer is [ErrInvalidPath]; a well-formed path that no record
// happens to contain is not an error, it is an empty result.
//
// A [FieldRevision] is emitted every time the value at path differs from the
// value in the previous belief, including the first appearance (absent to
// present) and a later belief that still covers the valid point but no longer
// contains the field (present to absent). Equality is [Log.Diff]'s: numbers
// compare by value, not notation, so re-recording 100 as 100.0 is not a change.
// Absent and null are distinguished — see [FieldValue].
//
// A record that exists but cannot be decoded is a [*CodecError] wrapping
// [ErrCodec], never a silently skipped step; a record encrypted for a subject
// whose key has been destroyed is a [*ShredError]. Under-reporting a change is
// the one failure mode a change log must not have, so both fail loudly, exactly
// as [Log.Diff] does.
//
// # Cost
//
// FieldHistory reads every record that ever covered as.ValidAt — one store
// query, paged internally — and decodes each once, so it is linear in the number
// of beliefs recorded about that valid point and independent of the rest of the
// log. It holds one page of records and the previous field value at a time, not
// the whole history.
//
// # What a present-to-absent revision means, and does not
//
// A field goes absent when a later belief still covers the valid point but its
// object no longer contains the path — a correction that drops the field, or a
// tombstoning write whose body is JSON null. Ordinary [Log.Put] and [Log.Correct]
// cannot instead make the *coverage* lapse: a write whose valid interval stops
// before as.ValidAt still leaves a remainder carrying the old belief across the
// point, so the point stays covered. Only destruction (retention, erasure) can
// remove a belief outright, and FieldHistory reflects the surviving history — it
// does not resurrect a belief that was deleted.
func (l *Log) FieldHistory(ctx context.Context, kind, entityID, path string, as As, opts ...FieldHistoryOption) ([]FieldRevision, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := l.checkKind(kind); err != nil {
		return nil, err
	}
	if entityID == "" {
		return nil, ErrMissingEntityID
	}
	// The path is parsed before the store is touched, so a malformed pointer is
	// the same ErrInvalidPath whether or not the entity has any history.
	tokens, err := parsePointer(path)
	if err != nil {
		return nil, err
	}

	var o fieldHistoryOpts
	for _, opt := range opts {
		opt(&o)
	}

	valid := as.ValidAt
	if valid.IsZero() {
		valid = l.now()
	} else {
		valid = valid.UTC()
	}

	// Every record whose valid interval contains the fixed point, across all of
	// transaction time, in transaction order. These are exactly the beliefs that
	// ever covered the point; at any single transaction instant at most one does,
	// so walking them in order is the walk along the transaction axis. The query
	// is paged so a deep history does not have to be held in memory at once.
	q := Query{Kind: kind, EntityID: entityID, ValidAt: valid, Limit: fieldHistoryPage}

	var (
		revisions []FieldRevision
		prev      FieldValue // absent: before any belief was recorded, the field did not exist
	)
	for {
		page, cursor, err := l.store.Query(ctx, q)
		if err != nil {
			return nil, err
		}
		for i := range page {
			rec := &page[i]
			vals, err := l.fieldValues(ctx, rec)
			if err != nil {
				return nil, err
			}
			value, present := valueAtPointer(vals, tokens)
			cur := FieldValue{Value: value, Present: present}
			if !prev.equal(cur) {
				revisions = append(revisions, FieldRevision{
					Path:      path,
					From:      prev,
					To:        cur,
					TxAt:      rec.TxFrom,
					ValidFrom: rec.ValidFrom,
					ValidTo:   rec.ValidTo,
					Actor:     rec.Actor,
					Reason:    rec.Reason,
					Intent:    rec.Intent,
				})
			}
			prev = cur
		}
		if cursor.IsZero() {
			break
		}
		q.After = cursor
	}

	if o.descending {
		slices.Reverse(revisions)
	}
	return revisions, nil
}

// fieldValues decrypts a record if it is sealed for a subject, then decodes it
// to comparable values. It is the same path [Log.Diff] takes for each of its
// operands: a shredded record is a loud [*ShredError] and an undecodable one a
// [*CodecError], never returned as an empty or skipped value.
func (l *Log) fieldValues(ctx context.Context, r *Record) (map[string]any, error) {
	dr, err := l.decryptRecord(ctx, r)
	if err != nil {
		return nil, err
	}
	return l.decode(dr)
}
