package chronicle

import (
	"maps"
	"slices"
	"time"
)

// RecordID uniquely identifies a record within a log.
//
// IDs are assigned by chronicle, never by the caller. The format is an
// implementation detail and callers must treat an ID as opaque, but two
// properties are guaranteed and relied upon internally: an ID is unique within
// a log, and IDs sort lexicographically in the order the records were written.
// The second property is what gives queries a deterministic total order even
// when two records share a transaction instant.
type RecordID string

// Actor identifies who caused a change. It is required on every write.
//
// chronicle has no ambient default actor and will never silently record
// "system" on your behalf: a write whose actor has an empty ID fails with
// [ErrMissingActor]. That restriction is deliberate and has a regulatory basis
// — 21 CFR 11.10(e) requires audit trails to record "operator entries and
// actions", 11.50(a)(1) requires a signer's printed name, and PCAOB AS 1215
// .16 requires "the name of the person who prepared" added documentation. An
// attribution that defaults is not an attribution.
type Actor struct {
	// ID identifies the actor within your system. It is the only required
	// field, and it is the field [Query.ActorID] matches on.
	ID string
	// Type describes what sort of actor this is — "user", "service", "job",
	// whatever your system distinguishes. Free-form and optional.
	Type string
	// Name is a human-readable display name, retained so that a rendering of
	// the log is legible without a join against your user table.
	Name string
}

// IsZero reports whether the actor carries no identity.
func (a Actor) IsZero() bool { return a.ID == "" && a.Type == "" && a.Name == "" }

// Intent records why a record was written. It distinguishes a routine
// assertion from an explicit correction of a prior belief, and marks the
// records chronicle writes itself when splitting an existing interval.
type Intent uint8

const (
	// IntentAssert is a normal [Log.Put]: a statement about what was true,
	// carrying no claim about whether it revises anything.
	IntentAssert Intent = iota
	// IntentCorrection is a [Log.Correct]: an explicit statement that a prior
	// belief was wrong. Storage-identical to an assertion; the difference is
	// that it is auditable as a correction, which is what makes "what did we
	// believe then, and when did we stop believing it" answerable.
	IntentCorrection
	// IntentRemainder marks a record chronicle wrote itself to preserve the
	// part of an existing record's valid interval that a new write did not
	// cover. A remainder carries the superseded record's data, actor, reason
	// and metadata unchanged — it re-asserts an existing fact rather than
	// making a new one — and it carries the transaction time of the write that
	// caused the split. See [Log.Put] for why the actor is attributed that way.
	IntentRemainder
)

// String implements [fmt.Stringer].
func (i Intent) String() string {
	switch i {
	case IntentAssert:
		return "assert"
	case IntentCorrection:
		return "correction"
	case IntentRemainder:
		return "remainder"
	default:
		return "intent(" + itoa(int(i)) + ")"
	}
}

func (i Intent) valid() bool { return i <= IntentRemainder }

// Record is one entity's state over a half-open valid-time interval, as
// believed over a half-open transaction-time interval.
//
// The two axes are independent and answer different questions. Valid time is
// when the fact was true in the world and is supplied by the caller.
// Transaction time is when chronicle learned it; it is assigned by the system,
// is never supplied by the caller, and is never rewritten. There is no
// exported way to set TxFrom or TxTo on a write, and that restriction is the
// only reason the log is worth trusting.
//
// Both axes are half-open, [from, to). An unbounded end is the zero
// [time.Time]. Use [Record.Valid] and [Record.Tx] to reason about the
// intervals rather than comparing the fields directly.
type Record struct {
	// ID is the record's unique, chronicle-assigned identifier.
	ID RecordID
	// EntityID is the caller's opaque identifier for the entity.
	EntityID string
	// Kind discriminates the type of entity.
	Kind string
	// Data is the serialized state, in whatever encoding the log's [Codec]
	// understands. Records carry their own shape, so a schema change does not
	// orphan history written before it.
	Data []byte

	// ValidFrom is when the fact became true in the world, inclusive. Zero
	// means it was always true.
	ValidFrom time.Time
	// ValidTo is when the fact stopped being true, exclusive. Zero means it
	// still holds.
	ValidTo time.Time

	// TxFrom is when chronicle learned the fact, inclusive. System-assigned.
	TxFrom time.Time
	// TxTo is when this belief was superseded, exclusive. Zero means it is the
	// current belief. System-assigned.
	TxTo time.Time

	// Actor is who caused the write. Always populated.
	Actor Actor
	// Reason is free-text business justification. Optional by design: see
	// docs/COMPLIANCE.md — a reason-for-change mandate has essentially one
	// textual home in the researched regulatory corpus, and it binds audit
	// firms rather than the systems chronicle records.
	Reason string
	// Intent records whether this was an assertion, a correction, or a
	// remainder chronicle wrote when splitting an interval.
	Intent Intent
	// Meta is caller-supplied metadata, copied on the way in and out.
	Meta map[string]string
}

// Valid returns the record's valid-time interval.
func (r Record) Valid() Interval { return Interval{From: r.ValidFrom, To: r.ValidTo} }

// Tx returns the record's transaction-time interval.
func (r Record) Tx() Interval { return Interval{From: r.TxFrom, To: r.TxTo} }

// IsCurrent reports whether this record is part of the current belief — that
// is, whether its transaction interval is still open.
func (r Record) IsCurrent() bool { return r.TxTo.IsZero() }

// Clone returns a deep copy. Data and Meta are copied, so the result shares no
// mutable state with the receiver.
//
// chronicle clones every record crossing the boundary into or out of a store,
// which is what lets callers hold on to returned records without being able to
// corrupt the log, and what keeps the in-memory store race-free.
func (r Record) Clone() Record {
	out := r
	out.Data = slices.Clone(r.Data)
	if r.Meta != nil {
		out.Meta = maps.Clone(r.Meta)
	}
	return out
}

func cloneRecords(recs []Record) []Record {
	if recs == nil {
		return nil
	}
	out := make([]Record, len(recs))
	for i, r := range recs {
		out[i] = r.Clone()
	}
	return out
}

// itoa is a tiny non-negative integer formatter, kept local so that record.go
// need not pull in strconv for one diagnostic string.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
