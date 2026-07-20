package chronicle

import (
	"context"
	"time"
)

// Hold is a legal hold: an instruction that records within its scope must not
// be destroyed while the hold is in effect. The retention sweeper skips every
// record an active hold matches, and reports what it withheld and under which
// hold, so that a sweep's output is evidence of the control working rather
// than a bare count.
//
// A hold restrains destruction only. It does not freeze writes — new records,
// supersessions and corrections proceed as ever, and must, because a log that
// stopped recording under litigation would be destroying evidence of the
// present to preserve evidence of the past. Nothing chronicle does destroys
// history anyway; the only operation a hold restrains is the retention
// sweeper's, which is the only destructive operation chronicle has.
//
// # Backdating is deliberate
//
// EffectiveFrom may sit in the past, and that is a requirement rather than a
// loophole. FRCP 37(e) applies to information "that should have been preserved
// in the anticipation or conduct of litigation", and per the 2015 Advisory
// Committee Note the duty attaches on *anticipation* — an event judged after
// the fact by a court, not the filing of a complaint. An operator therefore
// has to be able to assert, honestly and after the fact, "our duty attached
// last month". A hold that can only take effect "now" is the wrong shape for
// the obligation it exists to satisfy. See docs/COMPLIANCE.md.
//
// Two things backdating is not. It is not retroactive protection: a record
// destroyed before the hold was placed is gone, and the backdated timestamp
// cannot resurrect it — what it does is make the log of controls state when
// the duty attached, so that destruction between EffectiveFrom and PlacedAt is
// identifiable as the violation it may have been. And it is not a filter on
// which records the hold protects: an active hold withholds every record in
// its scope regardless of the record's own timestamps, because the
// preservation duty covers relevant information however old it is. A hold
// that excluded records older than its effective date would destroy exactly
// the evidence it was placed to keep.
//
// # The hold is itself a record
//
// PlacedAt is assigned by the store, never by the caller — an audit control
// whose own timeline could be written by its operator would prove nothing.
// Releasing a hold sets the release fields and keeps the row: the fact that a
// hold existed, who placed it, who released it and when, survives the hold
// itself.
type Hold struct {
	// ID identifies the hold. Caller-supplied — typically a matter or case
	// reference — required, and unique within the store.
	ID string

	// Kind scopes the hold to one entity kind. Empty matches every kind.
	Kind string
	// EntityID scopes the hold to one entity ID. Empty matches every entity.
	// With Kind empty too, the hold matches an entity ID across all kinds,
	// which is the natural scope for litigation about one subject whose
	// records span kinds. Both fields empty holds everything in the store.
	EntityID string

	// EffectiveFrom is when the preservation duty attached, as asserted by the
	// operator placing the hold. It may be backdated — see above — and it may
	// sit in the future, in which case the hold does not bite until then. The
	// zero time means the duty has no asserted start: the hold is effective
	// over all of time until released.
	EffectiveFrom time.Time

	// Reason is a free-text description of why the hold exists. Optional, like
	// [Record.Reason], and for the same researched reason: no regulation in
	// the corpus behind docs/COMPLIANCE.md mandates a reason field here.
	// Record one because your counsel wants one.
	Reason string

	// PlacedBy is who placed the hold. Required, with the same rule as every
	// chronicle write: no ambient default, no silent "system".
	PlacedBy Actor
	// PlacedAt is when the store recorded the hold. Store-assigned; whatever a
	// caller supplies is overwritten. The distinction between PlacedAt and
	// EffectiveFrom is the whole design: the operator asserts when the duty
	// attached, the system records when they asserted it, and neither can
	// masquerade as the other.
	PlacedAt time.Time

	// ReleasedAt is when the hold was released, exclusive, store-assigned.
	// Zero means the hold has not been released. A hold is active over
	// [EffectiveFrom, ReleasedAt), half-open like every interval in chronicle.
	ReleasedAt time.Time
	// ReleasedBy is who released the hold. Required at release.
	ReleasedBy Actor
	// ReleaseReason is a free-text justification for the release. Optional.
	ReleaseReason string
}

// Validate checks the fields a caller supplies at placement: the ID and the
// placing actor are required. It is exported so that every [HoldStore]
// implementation rejects the same malformed holds with the same errors.
func (h Hold) Validate() error {
	if h.ID == "" {
		return &HoldError{Err: ErrMissingHoldID}
	}
	if h.PlacedBy.ID == "" {
		return &HoldError{ID: h.ID, Err: ErrMissingActor}
	}
	return nil
}

// Matches reports whether the hold's scope covers the record. Scope is
// deliberately independent of the hold's effective interval — see the type
// comment for why a hold never filters records by time.
func (h Hold) Matches(r Record) bool {
	if h.Kind != "" && h.Kind != r.Kind {
		return false
	}
	if h.EntityID != "" && h.EntityID != r.EntityID {
		return false
	}
	return true
}

// ActiveAt reports whether the hold is in effect at the instant t: at or after
// EffectiveFrom and before ReleasedAt, with the usual half-open reading of
// zero bounds — a zero EffectiveFrom is "always was", a zero ReleasedAt is
// "still is".
func (h Hold) ActiveAt(t time.Time) bool {
	return Interval{From: h.EffectiveFrom, To: h.ReleasedAt}.Contains(t)
}

// HoldStore is an optional [Store] extension for stores that can hold legal
// holds. Both shipped stores implement it; the retention sweeper consults it
// when present, and a store without it simply cannot contain holds for the
// sweeper to honour — placing one is the only way in.
//
// Implementations must retain released holds. The audit value of a hold is
// precisely that its whole lifecycle — placed by whom, effective from when,
// released by whom — outlives it.
type HoldStore interface {
	// PlaceHold records a hold and returns it as stored, with PlacedAt
	// assigned by the store. The caller's PlacedAt, ReleasedAt, ReleasedBy and
	// ReleaseReason are ignored: placement writes the placement half of the
	// row and nothing else. Placing a hold whose ID already exists fails with
	// an error wrapping [ErrHoldExists] — a hold is not upsertable, because
	// silently replacing one hold's scope with another's is exactly the edit
	// an audit control must not permit.
	PlaceHold(ctx context.Context, h Hold) (Hold, error)

	// ReleaseHold marks the hold released, attributed to the given actor with
	// an optional free-text reason, and returns it as stored. ReleasedAt is
	// store-assigned. The hold row survives; nothing deletes it. Releasing a
	// hold that does not exist wraps [ErrNotFound]; releasing one already
	// released wraps [ErrHoldReleased], because a second release would either
	// rewrite the first release's attribution or silently do nothing, and an
	// audit control should do neither quietly.
	ReleaseHold(ctx context.Context, id string, by Actor, reason string) (Hold, error)

	// Holds returns every hold the store has ever recorded, released ones
	// included, ordered by placement.
	Holds(ctx context.Context) ([]Hold, error)
}
