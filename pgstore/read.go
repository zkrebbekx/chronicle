package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/zkrebbekx/chronicle"
)

// Get implements [chronicle.Store]. It returns the single record covering the
// given point on both axes, or an error wrapping [chronicle.ErrNotFound].
//
// The exclusion constraint makes it structurally impossible for two current
// records to match, but history is not so constrained — a query pinned to an
// instant on the transaction axis can in principle meet more than one row if
// the log was written to by something other than chronicle. LIMIT 1 over
// chronicle's total order makes the answer deterministic either way, matching
// the in-memory store's choice of the earliest.
func (s *Store) Get(ctx context.Context, q chronicle.GetQuery) (*chronicle.Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query := `SELECT ` + columns + ` FROM ` + s.qualified + `
		WHERE kind = $1 AND entity_id = $2
		  AND valid @> $3::timestamptz
		  AND tx @> $4::timestamptz
		ORDER BY ` + orderKey + ` LIMIT 1`

	row := s.db.QueryRowContext(ctx, query, q.Kind, q.EntityID, q.ValidAt.UTC(), q.TxAt.UTC())
	rec, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &chronicle.NotFoundError{
			Kind:     q.Kind,
			EntityID: q.EntityID,
			As:       chronicle.As{ValidAt: q.ValidAt, TxAt: q.TxAt},
		}
	}
	if err != nil {
		return nil, fmt.Errorf("pgstore: get %s/%s: %w", q.Kind, q.EntityID, err)
	}
	return &rec, nil
}

// Query implements [chronicle.Store]. Every filter, the ordering and the
// keyset resumption are pushed into SQL, so paging a large log never
// materialises it.
//
// One row beyond the limit is fetched and discarded, which is how the cursor
// comes back empty exactly when nothing was withheld — callers terminate
// without a trailing empty page, and the store never claims there is more when
// there is not.
func (s *Store) Query(ctx context.Context, q chronicle.Query) ([]chronicle.Record, chronicle.Cursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, chronicle.NoCursor, err
	}
	// Validation happens before any SQL is built, so a malformed query is the
	// same error here as it is in the in-memory store rather than whatever
	// Postgres would have said about it.
	if err := q.Validate(); err != nil {
		return nil, chronicle.NoCursor, err
	}

	b := &builder{}
	s.buildFilters(b, q)
	if !q.After.IsZero() {
		key, err := chronicle.DecodeCursor(q.After)
		if err != nil {
			return nil, chronicle.NoCursor, err
		}
		b.where(s.keysetPredicate(b, key, q.Descending))
	}

	query := `SELECT ` + columns + ` FROM ` + s.qualified + b.whereClause() +
		` ORDER BY ` + orderDirection(q.Descending)
	if q.Limit > 0 {
		query += ` LIMIT ` + strconv.Itoa(q.Limit+1)
	}

	rows, err := s.db.QueryContext(ctx, query, b.args...)
	if err != nil {
		return nil, chronicle.NoCursor, fmt.Errorf("pgstore: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []chronicle.Record
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, chronicle.NoCursor, fmt.Errorf("pgstore: query: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, chronicle.NoCursor, fmt.Errorf("pgstore: query: %w", err)
	}

	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
		return out, chronicle.EncodeCursor(out[len(out)-1]), nil
	}
	return out, chronicle.NoCursor, nil
}

// buildFilters translates a [chronicle.Query]'s predicates into SQL. Each one
// mirrors the in-memory store's matcher exactly; the conformance suite is what
// keeps them mirroring it.
func (s *Store) buildFilters(b *builder, q chronicle.Query) {
	if q.Kind != "" {
		b.where("kind = " + b.arg(q.Kind))
	}
	if q.EntityID != "" {
		b.where("entity_id = " + b.arg(q.EntityID))
	}
	if q.ActorID != "" {
		b.where("actor_id = " + b.arg(q.ActorID))
	}
	if q.HasIntent {
		b.where("intent = " + b.arg(int16(q.Intent)))
	}
	if q.CurrentOnly {
		b.where("tx_to IS NULL")
	}
	// The range operators are what the GiST indexes answer, and they carry the
	// half-open and unbounded-end conventions for free: a NULL bound is
	// infinite, and adjacent ranges do not overlap.
	if !q.Valid.IsAlways() {
		b.where("valid && tstzrange(" + b.arg(nullTime(q.Valid.From)) + "::timestamptz, " +
			b.arg(nullTime(q.Valid.To)) + "::timestamptz, '[)')")
	}
	if !q.Tx.IsAlways() {
		b.where("tx && tstzrange(" + b.arg(nullTime(q.Tx.From)) + "::timestamptz, " +
			b.arg(nullTime(q.Tx.To)) + "::timestamptz, '[)')")
	}
	if !q.ValidAt.IsZero() {
		b.where("valid @> " + b.arg(q.ValidAt.UTC()) + "::timestamptz")
	}
	if !q.TxAt.IsZero() {
		b.where("tx @> " + b.arg(q.TxAt.UTC()) + "::timestamptz")
	}
}

// keysetPredicate renders "sorts strictly after the cursor position", which is
// the whole of keyset pagination.
//
// It is written as a row comparison rather than the expanded three-level OR
// because Postgres understands a row comparison as a single index condition
// over the matching index, so a resumed page starts with a seek rather than a
// scan-and-discard.
func (s *Store) keysetPredicate(b *builder, key chronicle.CursorKey, descending bool) string {
	op := ">"
	if descending {
		op = "<"
	}
	// The unbounded valid start is -infinity on both sides of the comparison,
	// so that a cursor sitting on an unbounded record resumes correctly rather
	// than comparing against NULL and yielding no rows at all.
	validFrom := "'-infinity'::timestamptz"
	if !key.ValidFrom.IsZero() {
		validFrom = b.arg(key.ValidFrom.UTC()) + "::timestamptz"
	}
	return "(tx_from, COALESCE(valid_from, '-infinity'::timestamptz), id) " + op +
		" (" + b.arg(key.TxFrom.UTC()) + "::timestamptz, " + validFrom + ", " +
		b.arg(string(key.ID)) + `::text COLLATE "C")`
}

// orderDirection renders chronicle's total order in the requested direction.
// Descending must reverse every term, or the order stops being the exact
// reverse and pagination stops lining up with the ascending scan.
func orderDirection(descending bool) string {
	if descending {
		return `tx_from DESC, COALESCE(valid_from, '-infinity'::timestamptz) DESC, id DESC`
	}
	return orderKey
}

// builder accumulates WHERE conditions and their bind parameters, numbering
// placeholders as it goes so that conditions can be added in any order.
type builder struct {
	conds []string
	args  []any
}

// arg records a bind parameter and returns its placeholder.
func (b *builder) arg(v any) string {
	b.args = append(b.args, v)
	return "$" + strconv.Itoa(len(b.args))
}

func (b *builder) where(cond string) { b.conds = append(b.conds, cond) }

func (b *builder) whereClause() string {
	if len(b.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(b.conds, " AND ")
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

// scanRecord reads one row of the [columns] projection.
func scanRecord(sc scanner) (chronicle.Record, error) {
	var (
		rec        chronicle.Record
		id         string
		data       []byte
		validFrom  sql.NullTime
		validTo    sql.NullTime
		txFrom     sql.NullTime
		txTo       sql.NullTime
		intent     int16
		metaRaw    []byte
		reason     string
		actorID    string
		actorType  string
		actorName  string
		kind       string
		entityID   string
		metaParsed map[string]string
	)
	err := sc.Scan(&id, &kind, &entityID, &data, &validFrom, &validTo, &txFrom, &txTo,
		&actorID, &actorType, &actorName, &reason, &intent, &metaRaw)
	if err != nil {
		return chronicle.Record{}, err
	}
	if len(metaRaw) > 0 {
		if err := json.Unmarshal(metaRaw, &metaParsed); err != nil {
			return chronicle.Record{}, fmt.Errorf("decode metadata of record %s: %w", id, err)
		}
	}
	// An empty jsonb object comes back as an empty map; chronicle's convention
	// is that a record written without metadata reads back with none, so that a
	// round trip is an identity rather than an invention.
	if len(metaParsed) == 0 {
		metaParsed = nil
	}

	rec = chronicle.Record{
		ID:        chronicle.RecordID(id),
		Kind:      kind,
		EntityID:  entityID,
		Data:      data,
		ValidFrom: fromNullTime(validFrom),
		ValidTo:   fromNullTime(validTo),
		TxFrom:    fromNullTime(txFrom),
		TxTo:      fromNullTime(txTo),
		Actor:     chronicle.Actor{ID: actorID, Type: actorType, Name: actorName},
		Reason:    reason,
		Intent:    chronicle.Intent(intent),
		Meta:      metaParsed,
	}
	return rec, nil
}
