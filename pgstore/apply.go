package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zkrebbekx/chronicle"
)

// insertBatch caps how many records go into one INSERT. Postgres allows 65535
// bind parameters and a record costs fourteen, so the real ceiling is about
// 4600; a thousand leaves room and keeps a single statement's memory bounded.
const insertBatch = 1000

// Apply implements [chronicle.Store]. It reads the entity's current
// overlapping records, runs the caller's plan against them, and applies the
// result — all in one transaction, under a lock held from before the read
// until after the commit.
//
// # Isolation, and why not SERIALIZABLE
//
// A write to one entity is a read-modify-write, and the read has to be inside
// the same lock and the same transaction as the write. That is why Apply takes
// a plan rather than a finished write.
//
// The alternative — the log reads through Query, computes a split, and hands
// Apply the result — cannot be rescued by an isolation level, and this was
// measured rather than assumed:
//
//   - SERIALIZABLE does nothing. SSI detects an anomaly by tracking the reads a
//     transaction made; the read happened in an earlier, already-committed
//     transaction, so there is no dependency inside the write's transaction to
//     track and both writers commit happily. It would add mandatory 40001 retry
//     handling and buy nothing.
//   - SELECT ... FOR UPDATE has the same hole. The rows were read before this
//     transaction began, so the lock is taken after the decision it guards.
//   - Detect-and-retry is correct but starves. Under two writers hammering one
//     entity, the writer that waits on the lock always finds its plan stale by
//     the time it gets in, while the writer that never waits never conflicts.
//     The loser loses every round: not a probabilistic tail, a stable
//     equilibrium. Measured at 100% starvation of one writer over eighty
//     writes before this was restructured.
//
// So the lock covers the whole operation:
//
//  1. A per-entity advisory lock, taken first and held to commit. Writers to
//     one entity queue in the order they arrive; readers are never blocked.
//  2. The entity's current overlapping records are read and row-locked inside
//     that transaction, then handed to the plan. Nothing can move between the
//     read and the write, so a plan cannot be stale.
//  3. The deferred exclusion constraint is the backstop, for a write that came
//     in as a [chronicle.StaticWrite] and so was not planned from this read at
//     all, and for anything writing to the table that is not chronicle. It
//     surfaces at COMMIT and is reported as [chronicle.ErrConflict].
//
// # Transaction time
//
// The instant is assigned here, from the database, and is the greater of
// clock_timestamp() and one microsecond past the newest transaction start
// among the records this write touches. clock_timestamp() alone would be
// enough almost always — every writer reads the same server clock — but two
// writes to one entity inside a single microsecond would tie, and a record
// superseded at its own TxFrom has an empty transaction interval that no
// as-of query can ever see.
//
// Whatever TxFrom an incoming record carries is overwritten, and
// ApplyRequest.TxAt is ignored. There is no path by which a caller can choose
// when the log appears to have learned something.
func (s *Store) Apply(ctx context.Context, req chronicle.ApplyRequest) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	if req.Plan == nil {
		return time.Time{}, errors.New("pgstore: ApplyRequest needs a Plan")
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return time.Time{}, fmt.Errorf("pgstore: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// The entity lock comes before anything is read. Taking it afterwards
	// would be taking it after the decision it exists to protect.
	planned := req.Entity.Kind != "" || req.Entity.EntityID != ""
	if planned {
		if err := s.lockEntity(ctx, tx, req.Entity); err != nil {
			return time.Time{}, err
		}
	}

	current, err := s.currentOverlapping(ctx, tx, req.Entity, req.Valid, planned)
	if err != nil {
		return time.Time{}, err
	}

	txAt, err := s.assignTxTime(ctx, tx, current)
	if err != nil {
		return time.Time{}, err
	}

	w, err := req.Plan(current, txAt)
	if err != nil {
		return time.Time{}, err
	}

	// A write that was not planned from this read — a StaticWrite, or one for
	// several entities — has had no lock taken on its behalf yet, so it gets
	// one now. It is late by construction, which is why such a write also has
	// to survive the exclusion constraint on its own merits.
	if !planned {
		for _, ref := range w.Entities() {
			if err := s.lockEntity(ctx, tx, ref); err != nil {
				return time.Time{}, err
			}
		}
	}

	targets, err := s.lockSupersedeTargets(ctx, tx, w.Supersede)
	if err != nil {
		return time.Time{}, err
	}
	if len(w.Insert) > 0 {
		if err := checkTargetsCurrent(w.Supersede, targets); err != nil {
			return time.Time{}, err
		}
	}

	if len(w.Supersede) > 0 {
		if err := s.closeTargets(ctx, tx, w.Supersede, txAt); err != nil {
			return time.Time{}, err
		}
	}
	if err := s.insert(ctx, tx, w.Insert, txAt); err != nil {
		return time.Time{}, err
	}

	// The exclusion constraint is deferred, so an overlap raised by a
	// concurrent writer surfaces here rather than at the statement that caused
	// it. That is the whole reason it is deferred, and the reason COMMIT is a
	// place errors have to be handled properly.
	if err := tx.Commit(); err != nil {
		if isExclusionViolation(err) {
			return time.Time{}, &chronicle.ConflictError{
				Reason: "another writer left an overlapping current record for this entity",
				Err:    err,
			}
		}
		return time.Time{}, fmt.Errorf("pgstore: commit: %w", err)
	}
	committed = true
	return txAt, nil
}

// lockEntity takes an advisory lock on one entity, held until the transaction
// ends.
//
// An advisory lock rather than a row lock, because there may be no rows to
// lock: the first write to an entity has nothing to take a lock on, and that
// is precisely the case where two writers can both create a first current
// record.
//
// Callers that lock several entities must do so in a deterministic order, or a
// write spanning two entities will deadlock against another spanning the same
// two the other way round. [chronicle.Write.Entities] sorts.
func (s *Store) lockEntity(ctx context.Context, tx *sql.Tx, ref chronicle.EntityRef) error {
	// hashtextextended rather than a Go-side hash so the key does not depend
	// on this process's hash seed, and \x1f as the separator so that
	// ("ab", "c") and ("a", "bc") cannot collide.
	key := ref.Kind + "\x1f" + ref.EntityID
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		return fmt.Errorf("pgstore: lock %s/%s: %w", ref.Kind, ref.EntityID, err)
	}
	return nil
}

// currentOverlapping reads and row-locks the entity's current records whose
// valid interval overlaps valid, in chronicle's total order.
//
// FOR UPDATE on top of the advisory lock is not redundant. The advisory lock
// only stops other writers going through this adapter; the row lock also stops
// anything else in the database updating these rows while the plan is being
// computed against them.
func (s *Store) currentOverlapping(ctx context.Context, tx *sql.Tx, ref chronicle.EntityRef, valid chronicle.Interval, planned bool) ([]chronicle.Record, error) {
	if !planned {
		return nil, nil
	}

	args := []any{ref.Kind, ref.EntityID}
	q := `SELECT ` + columns + ` FROM ` + s.qualified +
		` WHERE kind = $1 AND entity_id = $2 AND tx_to IS NULL`
	if !valid.IsAlways() {
		q += ` AND valid && tstzrange($3::timestamptz, $4::timestamptz, '[)')`
		args = append(args, nullTime(valid.From), nullTime(valid.To))
	}
	q += ` ORDER BY ` + orderKey + ` FOR UPDATE`

	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("pgstore: read current records for %s/%s: %w", ref.Kind, ref.EntityID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []chronicle.Record
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("pgstore: read current records: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgstore: read current records: %w", err)
	}
	return out, nil
}

// target is what the write needs to know about a record it means to supersede.
type target struct {
	txFrom  time.Time
	current bool
}

// lockSupersedeTargets reads and row-locks the records the write means to
// close. The row lock is redundant with the advisory lock in the ordinary case
// and cheap; it is not redundant when a write supersedes records belonging to
// an entity it does not insert into.
func (s *Store) lockSupersedeTargets(ctx context.Context, tx *sql.Tx, ids []chronicle.RecordID) (map[chronicle.RecordID]target, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders, args := idList(ids, 0)
	q := `SELECT id, tx_from, tx_to IS NULL FROM ` + s.qualified +
		` WHERE id IN (` + placeholders + `) FOR UPDATE`
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("pgstore: read supersession targets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[chronicle.RecordID]target, len(ids))
	for rows.Next() {
		var (
			id string
			t  target
		)
		if err := rows.Scan(&id, &t.txFrom, &t.current); err != nil {
			return nil, fmt.Errorf("pgstore: read supersession targets: %w", err)
		}
		t.txFrom = t.txFrom.UTC()
		out[chronicle.RecordID(id)] = t
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgstore: read supersession targets: %w", err)
	}
	return out, nil
}

// checkTargetsCurrent reports a conflict if any record the write means to
// supersede has gone or has already been closed.
//
// Only writes that also insert are held to this. A supersession on its own is
// idempotent by contract, so that a retry cannot rewrite a transaction
// timestamp that has already been assigned.
func checkTargetsCurrent(ids []chronicle.RecordID, targets map[chronicle.RecordID]target) error {
	for _, id := range ids {
		t, ok := targets[id]
		if !ok {
			return &chronicle.ConflictError{
				Reason: fmt.Sprintf("record %s no longer exists", id),
			}
		}
		if !t.current {
			return &chronicle.ConflictError{
				Reason: fmt.Sprintf("record %s was already superseded by another writer", id),
			}
		}
	}
	return nil
}

// assignTxTime picks the write's transaction instant, database-side.
//
// The floor is one microsecond past the newest transaction start among the
// records the write is about to close, which is the only pair the ordering has
// to hold for: a record superseded at its own TxFrom has an empty transaction
// interval and is invisible to every as-of query. Writers to one entity are
// serialized by the advisory lock, so the record the previous writer left
// behind is in this set and this instant is strictly past it.
func (s *Store) assignTxTime(ctx context.Context, tx *sql.Tx, current []chronicle.Record) (time.Time, error) {
	var floor any
	var newest time.Time
	for _, r := range current {
		if r.TxFrom.After(newest) {
			newest = r.TxFrom
		}
	}
	if !newest.IsZero() {
		floor = newest
	}

	// GREATEST ignores NULL arguments, so a write with nothing to supersede
	// simply takes the clock.
	var txAt time.Time
	err := tx.QueryRowContext(ctx,
		`SELECT GREATEST(clock_timestamp(), $1::timestamptz + interval '1 microsecond')`,
		floor,
	).Scan(&txAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("pgstore: assign transaction time: %w", err)
	}
	return txAt.UTC(), nil
}

// closeTargets stamps the write's instant on every named record that is still
// current. Records already closed keep the timestamp they were closed with:
// transaction time, once assigned, is never rewritten.
func (s *Store) closeTargets(ctx context.Context, tx *sql.Tx, ids []chronicle.RecordID, txAt time.Time) error {
	placeholders, args := idList(ids, 1)
	args = append([]any{txAt}, args...)
	q := `UPDATE ` + s.qualified + ` SET tx_to = $1 WHERE id IN (` + placeholders + `) AND tx_to IS NULL`
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("pgstore: supersede: %w", err)
	}
	return nil
}

// insert adds the write's records, stamping each with the write's transaction
// instant.
//
// ON CONFLICT DO NOTHING keeps the original when an ID is inserted twice,
// matching the in-memory store: re-inserting an existing ID would duplicate
// history, and overwriting is not an option for an append-only log. Record IDs
// carry a per-log random token, so a genuine collision between two different
// records cannot happen and this only ever absorbs a repeat of the same write.
func (s *Store) insert(ctx context.Context, tx *sql.Tx, recs []chronicle.Record, txAt time.Time) error {
	for start := 0; start < len(recs); start += insertBatch {
		end := min(start+insertBatch, len(recs))
		if err := s.insertChunk(ctx, tx, recs[start:end], txAt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) insertChunk(ctx context.Context, tx *sql.Tx, recs []chronicle.Record, txAt time.Time) error {
	var (
		values strings.Builder
		args   = make([]any, 0, len(recs)*14)
	)
	for i, r := range recs {
		meta, err := encodeMeta(r.Meta)
		if err != nil {
			return fmt.Errorf("pgstore: record %s: %w", r.ID, err)
		}
		if i > 0 {
			values.WriteString(", ")
		}
		values.WriteByte('(')
		for c := 0; c < 14; c++ {
			if c > 0 {
				values.WriteString(", ")
			}
			values.WriteByte('$')
			values.WriteString(strconv.Itoa(len(args) + c + 1))
		}
		values.WriteString("::jsonb)")

		var data any
		if r.Data != nil {
			data = r.Data
		}
		args = append(args,
			string(r.ID), r.Kind, r.EntityID, data,
			nullTime(r.ValidFrom), nullTime(r.ValidTo),
			txAt, nullTime(r.TxTo),
			r.Actor.ID, r.Actor.Type, r.Actor.Name, r.Reason, int16(r.Intent), meta,
		)
	}

	q := `INSERT INTO ` + s.qualified + ` (` + columns + `) VALUES ` + values.String() +
		` ON CONFLICT (id) DO NOTHING`
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		if isExclusionViolation(err) {
			return &chronicle.ConflictError{
				Reason: "the write overlaps a current record it did not supersede",
				Err:    err,
			}
		}
		return fmt.Errorf("pgstore: insert: %w", err)
	}
	return nil
}

// encodeMeta renders a record's metadata as jsonb. Nil and empty both become
// an empty object, so that a record round-trips with nil metadata still nil.
func encodeMeta(meta map[string]string) (string, error) {
	if len(meta) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return "", fmt.Errorf("encode metadata: %w", err)
	}
	return string(b), nil
}

// idList renders a bind-parameter list for a set of record IDs, numbered from
// offset+1, and the matching arguments.
//
// Written out rather than passed as an array because array encoding is a
// driver-specific concern — lib/pq wants pq.Array, pgx takes a []string
// directly — and this package must work under any driver.
func idList(ids []chronicle.RecordID, offset int) (string, []any) {
	var b strings.Builder
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('$')
		b.WriteString(strconv.Itoa(offset + i + 1))
		args = append(args, string(id))
	}
	return b.String(), args
}

// sqlStater is the shape both pgx and lib/pq errors have: a five-character
// SQLSTATE. Matching on the interface rather than on a driver's concrete type
// is what lets this package stay driver-agnostic.
type sqlStater interface{ SQLState() string }

// isExclusionViolation reports whether err is Postgres's 23P01, raised when
// the non-overlap exclusion constraint rejects a write.
//
// The string fallback exists for drivers that expose neither SQLState nor a
// usable error type. It is a last resort and deliberately narrow: matching the
// constraint name would need the store's name threaded through here, and
// matching the SQLSTATE text is the most specific thing available.
func isExclusionViolation(err error) bool {
	var st sqlStater
	if errors.As(err, &st) {
		return st.SQLState() == "23P01"
	}
	return strings.Contains(err.Error(), "23P01") ||
		strings.Contains(err.Error(), "conflicting key value violates exclusion constraint")
}
