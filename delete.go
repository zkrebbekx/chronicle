package chronicle

import (
	"context"
	"time"
)

// Deleter is an optional [Store] extension for stores that can destroy
// records. It exists for exactly one caller: the retention sweeper in
// package retain.
//
// Deletion is real destruction, and it contradicts the posture of everything
// else in this package — chronicle's core promise is that nothing is ever
// destroyed, and this interface destroys. The contradiction is deliberate and
// belongs to the caller: retention schedules exist precisely to destroy on a
// schedule, keeping data past its period is a liability its owner did not
// choose, and whether a given kind should be swept at all is a regulatory
// decision chronicle cannot make. What chronicle can do is refuse to blur the
// line — deletion is a separate capability, invoked by an explicit sweep
// against an explicit policy, and never a side effect of anything.
//
// # What a store must guarantee
//
// Delete must refuse to destroy current belief. A record whose TxTo is zero is
// what the log currently asserts, and destroying it does not trim history — it
// changes the present. If any named record is current, the store must delete
// nothing and fail with an error wrapping [ErrCurrentRecord] naming the
// record. The sweeper never names a current record; the store enforcing it
// anyway is what makes a buggy or malicious caller a loud failure instead of a
// quiet loss.
//
// Delete must leave tombstones for chained records. A deleted record whose
// metadata carries [MetaChain] takes its place in a tamper-evidence chain, and
// destroying it would otherwise break the chain for every record after it. The
// store records a [Tombstone] — the record's coordinates and its chain hash,
// nothing else — atomically with the deletion, and [Log.Verify] passes over
// tombstones using the retained hash. The store computes tombstones itself,
// from the records it is destroying, so that no caller can destroy a chained
// record and skip the tombstone.
//
// Delete must be idempotent. IDs that no longer exist are skipped, not errors,
// so a sweep interrupted after a partial batch can be retried whole.
type Deleter interface {
	// Delete destroys the named records and returns how many were actually
	// destroyed. The whole call is atomic: all named records are removed and
	// their tombstones recorded, or — if any named record is current — nothing
	// is, and the error wraps [ErrCurrentRecord]. IDs that do not exist are
	// skipped.
	Delete(ctx context.Context, ids []RecordID) (int, error)

	// Tombstones returns the tombstones recorded for one entity, in chain
	// order: transaction start, then valid start, then record ID. Both kind
	// and entityID are required.
	Tombstones(ctx context.Context, kind, entityID string) ([]Tombstone, error)
}

// Tombstone is what remains of a chained record after retention destroyed it:
// its coordinates in the chain, and the chain hash it carried. The content is
// gone — that was the point of deleting it — but the hash keeps the chain
// verifiable across the gap.
//
// Be precise about what a tombstone proves, because it is less than it looks.
// [Log.Verify] passing over a tombstone establishes that the surviving records
// around the gap are the ones the chain head commits to, and that a record
// with this chain value stood in the gap. It establishes nothing about why the
// record was destroyed: a store's Delete writes tombstones for whatever it is
// asked to delete, so an administrator with database access can destroy a
// chained record through the same protocol and Verify will pass just as it
// does after a legitimate sweep. A tombstone is evidence of destruction, not
// of authorisation. If you need the two distinguished, anchor chain heads
// externally with [Log.ChainHead] and keep sweep Reports where the database
// administrator cannot edit them; chronicle ships neither, and says so rather
// than implying otherwise.
type Tombstone struct {
	// Kind and EntityID name the entity whose chain the destroyed record
	// belonged to.
	Kind, EntityID string
	// RecordID is the destroyed record's ID.
	RecordID RecordID
	// ValidFrom and TxFrom are the destroyed record's positions on the two
	// axes — the parts of its sort key, retained so the tombstone can be
	// ordered into the chain where the record used to be.
	ValidFrom time.Time
	TxFrom    time.Time
	// ChainHash is the chain value the destroyed record carried, verbatim from
	// its [MetaChain] metadata, format version prefix included.
	ChainHash string
	// DeletedAt is when the store destroyed the record. Store-assigned.
	DeletedAt time.Time
}
