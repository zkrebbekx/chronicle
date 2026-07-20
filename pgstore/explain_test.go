package pgstore_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestQueryPlans checks that the query surface actually uses the indexes the
// schema ships, rather than assuming it does.
//
// An index that is never chosen is worse than no index: it costs every write
// and buys nothing, and nobody notices until the table is large enough that
// noticing is expensive. So each of these asks Postgres what it would do and
// fails if the answer is a sequential scan.
//
// The planner needs enough rows to prefer an index, and it needs statistics,
// so the fixture is large-ish and ANALYZEd. It is still small enough that the
// planner could reasonably choose a scan for the unselective cases, which is
// why only the selective ones are asserted on.
func TestQueryPlans(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	store := newStore(t, db)
	table := store.Table()

	seedForPlanning(ctx, t, db, table)
	if _, err := db.ExecContext(ctx, `ANALYZE `+table); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	cases := []struct {
		name  string
		query string
		args  []any
		// want names an index the plan must mention.
		want string
	}{
		{
			name: "current records for one entity overlapping an interval",
			// The hot path: what every write plans against.
			query: `SELECT id FROM ` + table + ` WHERE kind = $1 AND entity_id = $2
			        AND tx_to IS NULL AND valid && tstzrange($3::timestamptz, $4::timestamptz, '[)')`,
			args: []any{"employee", "e0042", march, july},
			want: "_no_overlap",
		},
		{
			name: "one entity's history in chronicle's total order",
			query: `SELECT id FROM ` + table + ` WHERE kind = $1 AND entity_id = $2
			        ORDER BY tx_from, COALESCE(valid_from, '-infinity'::timestamptz), id`,
			args: []any{"employee", "e0042"},
			want: "_entity_order",
		},
		{
			name: "everything one actor wrote",
			query: `SELECT id FROM ` + table + ` WHERE actor_id = $1
			        ORDER BY tx_from, COALESCE(valid_from, '-infinity'::timestamptz), id`,
			args: []any{"u-rare"},
			want: "_actor_order",
		},
		{
			name: "a page resumed from a cursor",
			// Keyset pagination. A sequential scan here would mean every page
			// past the first costs the whole table.
			query: `SELECT id FROM ` + table + `
			        WHERE (tx_from, COALESCE(valid_from, '-infinity'::timestamptz), id) > ($1::timestamptz, $2::timestamptz, $3::text COLLATE "C")
			        ORDER BY tx_from, COALESCE(valid_from, '-infinity'::timestamptz), id LIMIT 50`,
			args: []any{time.Now().UTC(), march, "zzz"},
			want: "_order",
		},
		{
			name: "the first page of an unfiltered scan",
			query: `SELECT id FROM ` + table + `
			        ORDER BY tx_from, COALESCE(valid_from, '-infinity'::timestamptz), id LIMIT 50`,
			want: "_order",
		},
		{
			name: "a point lookup on the valid axis for one entity",
			query: `SELECT id FROM ` + table + ` WHERE kind = $1 AND entity_id = $2
			        AND valid @> $3::timestamptz AND tx @> $4::timestamptz`,
			args: []any{"employee", "e0042", may, time.Now().UTC()},
			want: "_valid_gist",
		},
	}

	for _, tc := range cases {
		t.Run("given "+tc.name, func(t *testing.T) {
			plan := explain(ctx, t, db, tc.query, tc.args...)
			t.Run("when the planner is asked how it would run it", func(t *testing.T) {
				t.Run("then it uses "+tc.want, func(t *testing.T) {
					if !strings.Contains(plan, tc.want) {
						t.Fatalf("plan does not use an index matching %q:\n%s", tc.want, plan)
					}
				})
				t.Run("then it does not fall back to a sequential scan", func(t *testing.T) {
					if strings.Contains(plan, "Seq Scan") {
						t.Fatalf("plan contains a sequential scan:\n%s", plan)
					}
				})
			})
		})
	}
}

// seedForPlanning fills the table with enough rows, spread over enough
// entities and actors, for the planner to have a real choice to make.
func seedForPlanning(ctx context.Context, t *testing.T, db *sql.DB, table string) {
	t.Helper()
	// One current record and several superseded ones per entity, so both the
	// partial index and the full ones have something to be selective about.
	_, err := db.ExecContext(ctx, `
		INSERT INTO `+table+` (id, kind, entity_id, data, valid_from, valid_to, tx_from, tx_to, actor_id, intent)
		SELECT
			'seed-' || lpad(e::text, 5, '0') || '-' || lpad(g::text, 3, '0'),
			'employee',
			'e' || lpad(e::text, 4, '0'),
			NULL,
			$1::timestamptz + (g || ' days')::interval,
			$1::timestamptz + ((g + 30) || ' days')::interval,
			$2::timestamptz + (e * 10 + g || ' seconds')::interval,
			CASE WHEN g < 4 THEN $2::timestamptz + (e * 10 + g + 1 || ' seconds')::interval END,
			CASE WHEN e = 42 AND g = 0 THEN 'u-rare' ELSE 'u-common' END,
			0
		FROM generate_series(1, 2000) e, generate_series(0, 4) g`,
		march, time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("seeding the planning fixture: %v", err)
	}
}

// explain returns the plan Postgres would use, as text.
func explain(ctx context.Context, t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.QueryContext(ctx, "EXPLAIN (COSTS OFF) "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN failed: %v\nquery: %s", err, query)
	}
	defer func() { _ = rows.Close() }()

	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("reading the plan: %v", err)
		}
		fmt.Fprintln(&b, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the plan: %v", err)
	}
	return b.String()
}
