package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zkrebbekx/chronicle"
)

// This file is the adapter's implementation of the optional compliance
// capabilities: [chronicle.Deleter] and [chronicle.HoldStore]. Deletion is
// the one operation in the adapter that destroys rows, and it is fenced the
// same way the in-memory store fences it — current records refuse the whole
// batch, and chained records leave tombstones in the same transaction.

// Delete implements [chronicle.Deleter]. The current-record check, the
// deletion and the tombstone insertion share one transaction, so no reader
// can observe a chained record gone and its tombstone missing, and a refusal
// leaves the store untouched.
func (s *Store) Delete(ctx context.Context, ids []chronicle.RecordID) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("pgstore: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Row-lock the targets and read what the tombstones will need. The lock
	// matters: a concurrent Apply superseding one of these records between
	// the check and the DELETE would otherwise race the current-record fence.
	placeholders, args := idList(ids, 1)
	args = append([]any{chronicle.MetaChain}, args...)
	rows, err := tx.QueryContext(ctx,
		`SELECT id, kind, entity_id, valid_from, tx_from, tx_to IS NULL, meta->>$1 FROM `+s.qualified+
			` WHERE id IN (`+placeholders+`) FOR UPDATE`, args...)
	if err != nil {
		return 0, fmt.Errorf("pgstore: read deletion targets: %w", err)
	}
	type doomed struct {
		id        string
		kind      string
		entityID  string
		validFrom sql.NullTime
		txFrom    time.Time
		chain     sql.NullString
	}
	var targets []doomed
	for rows.Next() {
		var (
			d       doomed
			current bool
		)
		if err := rows.Scan(&d.id, &d.kind, &d.entityID, &d.validFrom, &d.txFrom, &current, &d.chain); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("pgstore: read deletion targets: %w", err)
		}
		if current {
			_ = rows.Close()
			return 0, &chronicle.DeleteError{RecordID: chronicle.RecordID(d.id), Err: chronicle.ErrCurrentRecord}
		}
		targets = append(targets, d)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("pgstore: read deletion targets: %w", err)
	}
	_ = rows.Close()
	if len(targets) == 0 {
		// Nothing named still exists; a retried sweep is a no-op, not an error.
		return 0, tx.Commit()
	}

	delPlaceholders, delArgs := idList(ids, 0)
	res, err := tx.ExecContext(ctx, `DELETE FROM `+s.qualified+` WHERE id IN (`+delPlaceholders+`)`, delArgs...)
	if err != nil {
		return 0, fmt.Errorf("pgstore: delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("pgstore: delete: %w", err)
	}

	// Tombstones for the chained records, in the same transaction. ON
	// CONFLICT DO NOTHING keeps a retried deletion from touching a tombstone
	// already written — DeletedAt records the first destruction, and there is
	// only ever one.
	var (
		values strings.Builder
		targs  []any
	)
	for _, d := range targets {
		if !d.chain.Valid || d.chain.String == "" {
			continue
		}
		if values.Len() > 0 {
			values.WriteString(", ")
		}
		base := len(targs)
		fmt.Fprintf(&values, "($%d, $%d, $%d, $%d, $%d, $%d)", base+1, base+2, base+3, base+4, base+5, base+6)
		var validFrom any
		if d.validFrom.Valid {
			validFrom = d.validFrom.Time.UTC()
		}
		targs = append(targs, d.id, d.kind, d.entityID, validFrom, d.txFrom.UTC(), d.chain.String)
	}
	if values.Len() > 0 {
		q := `INSERT INTO ` + s.tombs + ` (record_id, kind, entity_id, valid_from, tx_from, chain_hash) VALUES ` +
			values.String() + ` ON CONFLICT (record_id) DO NOTHING`
		if _, err := tx.ExecContext(ctx, q, targs...); err != nil {
			return 0, fmt.Errorf("pgstore: write tombstones: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("pgstore: commit deletion: %w", err)
	}
	committed = true
	return int(n), nil
}

// Tombstones implements [chronicle.Deleter], returning one entity's
// tombstones in chain order.
func (s *Store) Tombstones(ctx context.Context, kind, entityID string) ([]chronicle.Tombstone, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if kind == "" {
		return nil, &chronicle.KindError{Err: chronicle.ErrUnknownKind}
	}
	if entityID == "" {
		return nil, chronicle.ErrMissingEntityID
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT record_id, kind, entity_id, valid_from, tx_from, chain_hash, deleted_at FROM `+s.tombs+
			` WHERE kind = $1 AND entity_id = $2`+
			` ORDER BY tx_from, COALESCE(valid_from, '-infinity'::timestamptz), record_id`,
		kind, entityID)
	if err != nil {
		return nil, fmt.Errorf("pgstore: tombstones for %s/%s: %w", kind, entityID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []chronicle.Tombstone
	for rows.Next() {
		var (
			t         chronicle.Tombstone
			recordID  string
			validFrom sql.NullTime
			txFrom    time.Time
			deletedAt time.Time
		)
		if err := rows.Scan(&recordID, &t.Kind, &t.EntityID, &validFrom, &txFrom, &t.ChainHash, &deletedAt); err != nil {
			return nil, fmt.Errorf("pgstore: tombstones: %w", err)
		}
		t.RecordID = chronicle.RecordID(recordID)
		t.ValidFrom = fromNullTime(validFrom)
		t.TxFrom = txFrom.UTC()
		t.DeletedAt = deletedAt.UTC()
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgstore: tombstones: %w", err)
	}
	return out, nil
}

// holdColumns is the hold projection, in the order scanHold expects.
const holdColumns = `id, kind, entity_id, effective_from, reason, ` +
	`placed_by_id, placed_by_type, placed_by_name, placed_at, ` +
	`released_at, released_by_id, released_by_type, released_by_name, release_reason`

// PlaceHold implements [chronicle.HoldStore]. placed_at comes from the
// database clock, and the caller's release fields are never written:
// placement writes the placement half of the row, nothing else.
func (s *Store) PlaceHold(ctx context.Context, h chronicle.Hold) (chronicle.Hold, error) {
	if err := ctx.Err(); err != nil {
		return chronicle.Hold{}, err
	}
	if err := h.Validate(); err != nil {
		return chronicle.Hold{}, err
	}

	row := s.db.QueryRowContext(ctx,
		`INSERT INTO `+s.holds+
			` (id, kind, entity_id, effective_from, reason, placed_by_id, placed_by_type, placed_by_name, placed_at)`+
			` VALUES ($1, $2, $3, $4, $5, $6, $7, $8, clock_timestamp())`+
			` RETURNING `+holdColumns,
		h.ID, h.Kind, h.EntityID, nullTime(h.EffectiveFrom), h.Reason,
		h.PlacedBy.ID, h.PlacedBy.Type, h.PlacedBy.Name)
	placed, err := scanHold(row)
	if err != nil {
		if isUniqueViolation(err) {
			return chronicle.Hold{}, &chronicle.HoldError{ID: h.ID, Err: chronicle.ErrHoldExists}
		}
		return chronicle.Hold{}, fmt.Errorf("pgstore: place hold %q: %w", h.ID, err)
	}
	return placed, nil
}

// ReleaseHold implements [chronicle.HoldStore]. The row survives with its
// release half filled in; the guard on released_at makes a second release an
// error rather than a rewrite of the first one's attribution.
func (s *Store) ReleaseHold(ctx context.Context, id string, by chronicle.Actor, reason string) (chronicle.Hold, error) {
	if err := ctx.Err(); err != nil {
		return chronicle.Hold{}, err
	}
	if id == "" {
		return chronicle.Hold{}, &chronicle.HoldError{Err: chronicle.ErrMissingHoldID}
	}
	if by.ID == "" {
		return chronicle.Hold{}, &chronicle.HoldError{ID: id, Err: chronicle.ErrMissingActor}
	}

	row := s.db.QueryRowContext(ctx,
		`UPDATE `+s.holds+
			` SET released_at = clock_timestamp(), released_by_id = $2, released_by_type = $3,`+
			` released_by_name = $4, release_reason = $5`+
			` WHERE id = $1 AND released_at IS NULL`+
			` RETURNING `+holdColumns,
		id, by.ID, by.Type, by.Name, reason)
	released, err := scanHold(row)
	if err == nil {
		return released, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return chronicle.Hold{}, fmt.Errorf("pgstore: release hold %q: %w", id, err)
	}

	// Nothing matched: either the hold does not exist, or it was already
	// released. The distinction matters to the caller, so look.
	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM `+s.holds+` WHERE id = $1)`, id).Scan(&exists); err != nil {
		return chronicle.Hold{}, fmt.Errorf("pgstore: release hold %q: %w", id, err)
	}
	if exists {
		return chronicle.Hold{}, &chronicle.HoldError{ID: id, Err: chronicle.ErrHoldReleased}
	}
	return chronicle.Hold{}, &chronicle.HoldError{ID: id, Err: chronicle.ErrNotFound}
}

// Holds implements [chronicle.HoldStore], returning every hold ever placed —
// released ones included — in placement order.
func (s *Store) Holds(ctx context.Context) ([]chronicle.Hold, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+holdColumns+` FROM `+s.holds+` ORDER BY placed_at, id`)
	if err != nil {
		return nil, fmt.Errorf("pgstore: holds: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []chronicle.Hold
	for rows.Next() {
		h, err := scanHold(rows)
		if err != nil {
			return nil, fmt.Errorf("pgstore: holds: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgstore: holds: %w", err)
	}
	return out, nil
}

// scanHold reads one row of the holdColumns projection.
func scanHold(sc scanner) (chronicle.Hold, error) {
	var (
		h             chronicle.Hold
		effectiveFrom sql.NullTime
		placedAt      time.Time
		releasedAt    sql.NullTime
	)
	err := sc.Scan(&h.ID, &h.Kind, &h.EntityID, &effectiveFrom, &h.Reason,
		&h.PlacedBy.ID, &h.PlacedBy.Type, &h.PlacedBy.Name, &placedAt,
		&releasedAt, &h.ReleasedBy.ID, &h.ReleasedBy.Type, &h.ReleasedBy.Name, &h.ReleaseReason)
	if err != nil {
		return chronicle.Hold{}, err
	}
	h.EffectiveFrom = fromNullTime(effectiveFrom)
	h.PlacedAt = placedAt.UTC()
	h.ReleasedAt = fromNullTime(releasedAt)
	return h, nil
}

// isUniqueViolation reports whether err is Postgres's 23505, raised when a
// primary key or unique constraint rejects a row.
func isUniqueViolation(err error) bool {
	var st sqlStater
	if errors.As(err, &st) {
		return st.SQLState() == "23505"
	}
	return strings.Contains(err.Error(), "23505") ||
		strings.Contains(err.Error(), "duplicate key value violates unique constraint")
}

// Compile-time assertions that the adapter carries the compliance
// capabilities.
var (
	_ chronicle.Deleter   = (*Store)(nil)
	_ chronicle.HoldStore = (*Store)(nil)
)
