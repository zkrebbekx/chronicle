// Package chronicle is a bitemporal entity change history for Go: an
// ORM-agnostic, queryable record of what your entities were, and of what you
// believed they were at any point in the past.
//
// A [Record] is one entity's state over a half-open valid-time interval, as
// believed over a half-open transaction-time interval. Writes never destroy
// anything: an overlapping write closes the transaction interval of the
// records it supersedes and writes replacements, so the log answers "what did
// we believe then" as readily as "what is true now".
//
//	log := chronicle.NewLog(chronicle.NewMemStore())
//	log.Put(ctx, "employee", "alice", []byte(`{"salary":50000}`), march, time.Time{}, actor)
//	rec, _ := log.Get(ctx, "employee", "alice", chronicle.Now())
//
// # The two time axes
//
// Valid time is when a fact was true in the world, and the caller supplies it.
// Transaction time is when chronicle learned the fact; the system assigns it,
// the caller cannot set it, and it is never rewritten. There is no exported
// field, option or argument anywhere in this package that writes TxFrom or
// TxTo, and that absence is the whole basis for trusting the log — a
// transaction axis its users can write records what someone wanted to have
// believed rather than what was believed.
//
// The pair makes three genuinely different questions separable:
//
//   - What is Alice's salary? — [Now]
//   - What was it in March? — [ValidAt]
//   - What did we believe in April that it had been in March? — [As] with
//     both fields set
//
// A uni-temporal log answers the first two and gives a confidently wrong
// answer to the third, because a retroactive correction rewrites what it
// appears to have known.
//
// # Half-open intervals and the zero time
//
// Both axes are half-open, [from, to): the lower bound is included and the
// upper bound is not. Closed intervals make adjacency ambiguous and coalescing
// wrong, and SQL:2011 follows the same convention.
//
// An unbounded end is the zero [time.Time], never a sentinel maximum
// timestamp. Zero is unambiguous in Go, survives marshalling, cannot be
// confused with a real instant, and maps to SQL NULL. A zero ValidTo means the
// fact still holds; a zero ValidFrom means it always did; a zero TxTo means
// the record is current belief.
//
// Reading a zero time correctly depends on which end of an interval it sits
// at — a zero lower bound is negative infinity and a zero upper bound is
// positive infinity — and getting that backwards is the classic bitemporal
// bug. All of it is therefore confined to [Interval]. Use its methods rather
// than comparing record timestamps by hand.
//
// # Writes
//
// [Log.Put] asserts that an entity had a state over a valid interval.
// [Log.Correct] does the same and marks the write as a correction of a prior
// belief. They are identical in storage and differ only in [Intent], which is
// what makes a retroactive fix distinguishable from an ordinary late-arriving
// fact.
//
// Both run the same algorithm. Every current record whose valid interval
// overlaps the new one is superseded, and wherever such a record extended
// beyond the new interval, the uncovered part is rewritten as a remainder
// record carrying the superseded record's data. The invariant this maintains
// is the point of the library: at any transaction instant, an entity's current
// valid intervals do not overlap, and no write introduces a gap where the
// timeline was previously covered.
//
// [Actor] is required on every write. There is no ambient default and no
// silent "system" attribution; a write with an empty actor ID is
// [ErrMissingActor]. Empty and inverted valid intervals are rejected with
// [ErrInvalidInterval] rather than stored.
//
// # Transaction time is ratcheted
//
// Two writes must never share a transaction instant, or a record superseded by
// the write that immediately follows it would be left with an empty
// transaction interval that no as-of query could observe. chronicle therefore
// ratchets: a write whose clock reading does not advance on the previous write
// is assigned the previous instant plus one nanosecond. Transaction timestamps
// within a [Log] are strictly increasing, whatever the clock does, so every
// superseded record has TxTo strictly after TxFrom.
//
// The alternative — letting timestamps tie and ordering on a separate sequence
// number — pushes the tiebreak into every reader and every downstream query.
// Ratcheting keeps it in one place. The cost is that transaction time can lead
// the wall clock by a nanosecond per write under sustained load, which
// self-corrects as soon as the write rate drops. The ratchet is per-Log; run
// one Log per store.
//
// Record IDs embed the transaction instant and a sequence number, so they sort
// in write order and give queries a total order even across the records of a
// single multi-record write.
//
// # Reads
//
// [Log.Get] returns the single record in force at a point on both axes.
// [Log.History] returns every version an entity has ever had, superseded ones
// included. [Log.Timeline] returns the valid-time sequence as believed at one
// transaction instant. [Log.Diff] reports field-level changes between two
// points. [Log.Query] filters across entities by kind, entity, actor, intent
// and time on either axis, and pages with an opaque keyset [Cursor] whose
// final tiebreaker is the unique record ID, so no row can be skipped or
// repeated at a page boundary however many records share a timestamp.
//
// # Diffing
//
// [Log.Diff] decodes record data through a [Codec] — [JSONCodec] by default —
// and compares the decoded structures, reporting each change as a
// [FieldChange] with an RFC 6901 JSON Pointer path.
//
// The comparison is fully structural: it descends into nested objects and
// arrays to any depth, and a change of shape at a node (an object becoming a
// scalar, say) is reported once at that node rather than as a burst of
// unrelated additions and removals. Objects are compared by key, so reordering
// keys is not a change. Numbers are decoded as [encoding/json.Number], so
// integers beyond float64's exact range compare correctly.
//
// Arrays are compared by position. This is the one documented limitation:
// inserting an element at the head of an array reports every later element as
// modified plus one addition at the end, rather than a single insertion.
// Reporting it as an insertion needs an alignment heuristic — a
// longest-common-subsequence over values, or a per-array identity field — and
// a heuristic that misfires on the cases it does not fit would be worse than a
// rule that is simple and stated.
//
// A codec failure is [ErrCodec], never an empty diff. A change log that
// reports "nothing changed" when it means "I could not tell" is worse than one
// that fails.
//
// # Storage
//
// [Store] is the persistence boundary and [MemStore] is the reference
// implementation. The interface is shaped so that a database/sql
// implementation is straightforward — values in, values out, no callbacks, no
// lock-holding iterators, and a limit and cursor that push down into LIMIT and
// a keyset predicate.
//
// Supersession must be atomic with the writes accompanying it. The four Store
// methods cannot express that, since Supersede and Put are separate calls, so
// stores should also implement [Atomic], whose single Apply carries both
// halves of a write; chronicle uses it whenever it is available and falls back
// to a non-atomic pair when it is not. A SQL implementation should run Apply
// in one transaction, and — because chronicle reads an entity's overlapping
// records before computing the write — should take row locks or run
// serializable, so two writers to one entity cannot both split the same
// pre-state.
//
// # Errors
//
// Match errors with [errors.Is] against [ErrNotFound], [ErrInvalidInterval],
// [ErrMissingActor], [ErrUnknownKind], [ErrCodec], [ErrInvalidCursor],
// [ErrMissingEntityID] and [ErrClosed]. Where extra context helps, the
// concrete error is also available via [errors.As]: [*IntervalError],
// [*KindError], [*CodecError], [*NotFoundError] and [*CursorError].
//
// # Concurrency
//
// A [Log] and a [MemStore] are safe for concurrent use. Records are deep-
// copied crossing the store boundary in both directions, so a caller can
// neither reach into the log through data it wrote nor corrupt it by mutating
// a record it read.
//
// # Scope
//
// This is the core temporal engine. It is not a database — Postgres does the
// hard storage work — nor a CDC/WAL tailer, which loses the actor identity and
// business intent a change record exists to capture, nor an event-sourcing
// framework, since chronicle records what an entity was rather than a stream
// of commands to fold.
//
// Retention policies, legal hold, tamper-evidence and the Postgres adapter are
// deliberately absent; see docs/DESIGN.md for the roadmap and
// docs/COMPLIANCE.md for what regulation actually requires, which is
// considerably less than most audit-log vendors claim.
package chronicle
