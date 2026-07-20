// Package retain is chronicle's retention sweeper: scheduled, explicit,
// hold-aware destruction of superseded records.
//
// Everything else in chronicle preserves history; this package destroys it,
// and the contradiction is the point rather than an accident. Data kept past
// its retention period is a liability its owner did not choose, and the
// regulations that require audit trails to exist also leave their disposal to
// the record owner's schedule. Whether a kind should be swept at all, and
// after how long, is a regulatory and contractual decision chronicle cannot
// make — so it refuses to: there is no default retention period, no policy
// ships enabled, and [Execute] fails rather than guesses when given nothing
// explicit. The commonly cited periods do not transplant the way vendors
// imply — HIPAA's six years attaches to written policies and procedures, and
// the SOX-lineage seven years binds the external audit firm's workpapers —
// see docs/COMPLIANCE.md. Set the period your counsel advises.
//
// What a sweep can destroy is deliberately narrow: records that are already
// superseded — TxTo closed — and have been for at least the policy's KeepFor.
// A current record is never eligible, whatever its age, because destroying
// current belief does not trim history, it changes the present; the age that
// matters is how long a record has been *dead*, measured from TxTo, not how
// long ago it was written. Records matched by an active legal hold are
// withheld, always: hold beats retention, with no override parameter, and the
// [Report] names each withheld record and the hold that saved it.
//
// [Plan] is the dry run and [Execute] is the real thing; they share every
// line of decision logic, so what Plan reports is what Execute would do
// against the same store state.
package retain

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zkrebbekx/chronicle"
)

// Policy is one kind's retention schedule: destroy this kind's superseded
// records once they have been superseded for KeepFor.
//
// A duration rather than a fixed cutoff date, because that is the shape
// retention obligations take — "retain for seven years from the report
// release date" — and because a duration stays correct across sweeps where a
// stored cutoff would silently stop deleting the day it was written. The
// cutoff for any given sweep is now minus KeepFor, and the [Report] states
// it, so every sweep's arithmetic is on the record.
type Policy struct {
	// Kind is the entity kind the policy applies to. Required; kinds without
	// a policy are never swept.
	Kind string
	// KeepFor is how long a superseded record is kept, measured from the
	// instant it was superseded (TxTo). Must be positive: a zero or negative
	// KeepFor would make "sweep" mean "destroy all history", and if that is
	// truly wanted it deserves a more honest spelling than a zero value.
	KeepFor time.Duration
}

// Errors reported by the sweeper. Match with [errors.Is].
var (
	// ErrNoPolicy is returned when a sweep is asked to run with no policies.
	// There is no default retention period — see the package comment — so a
	// sweep with nothing explicit has nothing legitimate to do.
	ErrNoPolicy = errors.New("retain: no retention policy given")

	// ErrInvalidPolicy is returned for a policy with no kind, a non-positive
	// KeepFor, or a kind named by more than one policy.
	ErrInvalidPolicy = errors.New("retain: invalid policy")

	// ErrNoDeleter is returned by [Execute] when the store does not implement
	// [chronicle.Deleter]. Destruction is an explicit store capability, not
	// something a sweeper improvises over Query.
	ErrNoDeleter = errors.New("retain: store does not support deletion")
)

// Report says what a sweep did — or, for [Plan], exactly what it would do.
type Report struct {
	// Now is the instant the sweep's cutoffs were computed from.
	Now time.Time
	// Executed is false for a [Plan]: nothing was archived or destroyed.
	Executed bool
	// Kinds holds one entry per policy, in the order the policies were given.
	Kinds []KindReport
}

// KindReport is one policy's share of a sweep.
type KindReport struct {
	// Kind is the policy's kind.
	Kind string
	// Cutoff is the eligibility line this sweep used: Now minus the policy's
	// KeepFor. Records superseded at or before it were eligible.
	Cutoff time.Time
	// Examined is how many of the kind's records the sweep considered.
	Examined int
	// Deleted is how many records were destroyed — or, when the report's
	// Executed is false, would have been.
	Deleted int
	// Tombstones is how many of those records carried a chain hash and so
	// leave (or would leave) a [chronicle.Tombstone] behind.
	Tombstones int
	// Withheld names every eligible record an active hold saved, and the hold
	// that saved it. Withheld records are evidence of the control working,
	// which is why they are listed rather than counted.
	Withheld []Withholding
}

// Withholding is one record a legal hold kept out of a sweep.
type Withholding struct {
	// RecordID is the record that was eligible and not destroyed.
	RecordID chronicle.RecordID
	// HoldID is the hold that withheld it — the first active matching hold in
	// placement order, when several match.
	HoldID string
}

// ArchiveFunc receives each batch of records immediately before they are
// destroyed. Returning an error aborts the sweep with that batch untouched.
//
// The hook is how the archive-before-delete strategy works without chronicle
// owning archival storage: copy the batch to your archive table, your object
// store, wherever — and only then does the sweeper destroy the originals.
//
// It must be idempotent, and this is a hard requirement rather than advice.
// The hook runs in the caller's failure domain and the deletion runs in the
// store's, and no transaction spans the two — the [chronicle.Store] interface
// has no way to run caller code inside a store transaction, deliberately. If
// the archive succeeds and the deletion then fails, or the process dies
// between them, the records are still in the store and the next sweep will
// archive them again. Key the archive on record ID and make the write an
// upsert; an archive that appends blindly will accumulate duplicates on
// exactly the runs that go wrong.
type ArchiveFunc func(ctx context.Context, doomed []chronicle.Record) error

// Option configures [Execute].
type Option func(*sweeper)

// WithArchive installs an archive hook. See [ArchiveFunc] for the idempotency
// requirement, which is load-bearing.
func WithArchive(fn ArchiveFunc) Option {
	return func(s *sweeper) { s.archive = fn }
}

// DefaultBatchSize is how many records a sweep examines, archives and deletes
// per store round trip when [WithBatchSize] is not given.
const DefaultBatchSize = 500

// WithBatchSize sets the page and deletion batch size. Values below one fall
// back to [DefaultBatchSize].
func WithBatchSize(n int) Option {
	return func(s *sweeper) {
		if n > 0 {
			s.batch = n
		}
	}
}

// Plan is the dry run: it walks the store exactly as [Execute] would and
// reports what Execute would destroy and withhold, deleting nothing,
// archiving nothing, and requiring no capabilities beyond reading. Run it
// first; a sweep whose plan surprises you is a sweep you were about to
// regret.
func Plan(ctx context.Context, store chronicle.Store, policies []Policy, now time.Time) (Report, error) {
	s := sweeper{store: store, batch: DefaultBatchSize}
	return s.sweep(ctx, policies, now, false)
}

// Execute destroys what [Plan] would report: every superseded record older
// than its kind's policy allows, except those an active legal hold matches.
// The store must implement [chronicle.Deleter] — [ErrNoDeleter] otherwise —
// and deletion is batched, so a sweep interrupted partway leaves some
// batches destroyed and the rest for the next run, which is safe because
// every decision is recomputed from store state each time.
//
// Holds are consulted through [chronicle.HoldStore] when the store implements
// it. A store without the capability cannot contain holds — placing one is
// the only way in — so its absence means there is genuinely nothing to
// honour, not that holds were skipped.
//
// now is compared against transaction instants the store assigned, so it
// should come from the same clock authority — for a database-backed store,
// the database's. In practice retention periods are months or years and dwarf
// any clock skew; it matters only when KeepFor approaches the skew between
// the sweeping process's clock and the store's.
func Execute(ctx context.Context, store chronicle.Store, policies []Policy, now time.Time, opts ...Option) (Report, error) {
	s := sweeper{store: store, batch: DefaultBatchSize}
	for _, opt := range opts {
		opt(&s)
	}
	var ok bool
	if s.deleter, ok = store.(chronicle.Deleter); !ok {
		return Report{}, fmt.Errorf("%w: %T", ErrNoDeleter, store)
	}
	return s.sweep(ctx, policies, now, true)
}

type sweeper struct {
	store   chronicle.Store
	deleter chronicle.Deleter
	archive ArchiveFunc
	batch   int
}

func (s *sweeper) sweep(ctx context.Context, policies []Policy, now time.Time, execute bool) (Report, error) {
	if err := validatePolicies(policies); err != nil {
		return Report{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	holds, err := s.activeHolds(ctx, now)
	if err != nil {
		return Report{}, err
	}

	report := Report{Now: now, Executed: execute}
	for _, p := range policies {
		kr, err := s.sweepKind(ctx, p, now, holds, execute)
		report.Kinds = append(report.Kinds, kr)
		if err != nil {
			return report, err
		}
	}
	return report, nil
}

func validatePolicies(policies []Policy) error {
	if len(policies) == 0 {
		return ErrNoPolicy
	}
	seen := make(map[string]struct{}, len(policies))
	for _, p := range policies {
		if p.Kind == "" {
			return fmt.Errorf("%w: policy has no kind", ErrInvalidPolicy)
		}
		if p.KeepFor <= 0 {
			return fmt.Errorf("%w: kind %q has a non-positive KeepFor", ErrInvalidPolicy, p.Kind)
		}
		if _, dup := seen[p.Kind]; dup {
			return fmt.Errorf("%w: kind %q is named by two policies", ErrInvalidPolicy, p.Kind)
		}
		seen[p.Kind] = struct{}{}
	}
	return nil
}

// activeHolds returns the holds in effect at now, in placement order, which
// is the order [Withholding] attribution follows.
func (s *sweeper) activeHolds(ctx context.Context, now time.Time) ([]chronicle.Hold, error) {
	hs, ok := s.store.(chronicle.HoldStore)
	if !ok {
		return nil, nil
	}
	all, err := hs.Holds(ctx)
	if err != nil {
		return nil, fmt.Errorf("retain: reading holds: %w", err)
	}
	active := all[:0:0]
	for _, h := range all {
		if h.ActiveAt(now) {
			active = append(active, h)
		}
	}
	return active, nil
}

// sweepKind walks one policy's kind in pages, deciding per record and
// destroying per batch.
func (s *sweeper) sweepKind(ctx context.Context, p Policy, now time.Time, holds []chronicle.Hold, execute bool) (KindReport, error) {
	kr := KindReport{Kind: p.Kind, Cutoff: now.Add(-p.KeepFor)}

	var (
		cursor chronicle.Cursor
		doomed []chronicle.Record
	)
	flush := func() error {
		if len(doomed) == 0 || !execute {
			doomed = doomed[:0]
			return nil
		}
		if s.archive != nil {
			if err := s.archive(ctx, doomed); err != nil {
				return fmt.Errorf("retain: archive hook for kind %q: %w", p.Kind, err)
			}
		}
		ids := make([]chronicle.RecordID, len(doomed))
		for i, r := range doomed {
			ids[i] = r.ID
		}
		if _, err := s.deleter.Delete(ctx, ids); err != nil {
			return fmt.Errorf("retain: deleting %d records of kind %q: %w", len(ids), p.Kind, err)
		}
		doomed = doomed[:0]
		return nil
	}

	for {
		// The transaction-axis filter over-selects — it matches on TxFrom
		// before the cutoff, current records included — and the loop narrows
		// to the actual rule below. Deleting between pages is safe under
		// keyset pagination: the cursor is a position, not an offset, and the
		// deleted rows all sort at or before it.
		page, next, err := s.store.Query(ctx, chronicle.Query{
			Kind:  p.Kind,
			Tx:    chronicle.Until(kr.Cutoff),
			Limit: s.batch,
			After: cursor,
		})
		if err != nil {
			return kr, fmt.Errorf("retain: scanning kind %q: %w", p.Kind, err)
		}

		for _, r := range page {
			kr.Examined++
			// Current belief is never eligible, whatever its age. The age
			// that matters is how long the record has been superseded.
			if r.IsCurrent() || r.TxTo.After(kr.Cutoff) {
				continue
			}
			if hold := firstMatch(holds, r); hold != "" {
				kr.Withheld = append(kr.Withheld, Withholding{RecordID: r.ID, HoldID: hold})
				continue
			}
			kr.Deleted++
			if _, chained := r.Meta[chronicle.MetaChain]; chained {
				kr.Tombstones++
			}
			doomed = append(doomed, r)
			if len(doomed) >= s.batch {
				if err := flush(); err != nil {
					return kr, err
				}
			}
		}

		if next.IsZero() {
			break
		}
		cursor = next
	}
	return kr, flush()
}

// firstMatch returns the ID of the first hold matching the record, or "".
func firstMatch(holds []chronicle.Hold, r chronicle.Record) string {
	for _, h := range holds {
		if h.Matches(r) {
			return h.ID
		}
	}
	return ""
}
