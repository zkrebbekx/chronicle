package pgstore_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zkrebbekx/chronicle"
)

// TestExclusionConstraint covers the first of the adapter's three correctness
// requirements: overlapping current valid intervals for one entity must be
// structurally impossible, and the constraint that makes them impossible must
// be deferred or it rejects ordinary correct writes.
//
// Both halves need proving, and they pull against each other. A constraint
// that is not deferred passes the "genuine violation is rejected" half and
// fails every real write; a table with no constraint at all passes the "correct
// writes are accepted" half and holds a corrupt log. Neither test alone says
// anything.
func TestExclusionConstraint(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	t.Run("given an entity whose record a write is about to split", func(t *testing.T) {
		store := newStore(t, db)
		l := chronicle.NewLog(store)

		if _, err := l.Put(ctx, "employee", "e1", []byte(`{"grade":"L3"}`), march, time.Time{}, alice); err != nil {
			t.Fatalf("first Put failed: %v", err)
		}

		t.Run("when the write goes through the adapter", func(t *testing.T) {
			// The new record and two remainders cover the same stretch of
			// valid time as the record being replaced, so this is the shape of
			// write the constraint has the most to say about.
			_, err := l.Put(ctx, "employee", "e1", []byte(`{"grade":"L4"}`), may, july, alice)
			t.Run("then it is accepted", func(t *testing.T) {
				if err != nil {
					t.Fatalf("a correct split was rejected: %v", err)
				}
			})
			t.Run("then the committed result does not overlap", func(t *testing.T) {
				if n := countOverlaps(ctx, t, db, store.Table()); n != 0 {
					t.Fatalf("%d pairs of current records overlap", n)
				}
			})
		})

		t.Run("when many splits accumulate", func(t *testing.T) {
			for i := 0; i < 20; i++ {
				from := march.AddDate(0, i%7, 0)
				_, err := l.Put(ctx, "employee", "e1", []byte(fmt.Sprintf(`{"n":%d}`, i)), from, from.AddDate(0, 3, 0), alice)
				if err != nil {
					t.Fatalf("Put %d failed: %v", i, err)
				}
			}
			t.Run("then every one was accepted and the timeline still tiles", func(t *testing.T) {
				if n := countOverlaps(ctx, t, db, store.Table()); n != 0 {
					t.Fatalf("%d pairs of current records overlap", n)
				}
			})
		})
	})

	// The two tests below are the pair that gives DEFERRABLE INITIALLY DEFERRED
	// its meaning, and they go through raw SQL rather than the adapter on
	// purpose.
	//
	// DESIGN.md states the requirement in terms of a write that inserts its
	// replacement before closing the record it replaces. This adapter does not
	// write in that order — Apply closes first, and so never passes through an
	// overlapping state at all — which means driving these through Apply would
	// prove nothing about the deferral either way. Testing the ordering the
	// requirement is actually about is the only way to show that the deferral
	// does what it claims, and the only way to keep it from being tidied away
	// by someone who tries it and finds nothing breaks.
	t.Run("given a split written in the order DESIGN.md describes", func(t *testing.T) {
		t.Run("when the replacement is inserted before its predecessor is closed", func(t *testing.T) {
			store := newStore(t, db)
			seedCurrent(ctx, t, db, store.Table(), "old", march, july)

			err := splitInsertFirst(ctx, db, store.Table(), "old", "new", march, july)
			t.Run("then the deferred constraint accepts it", func(t *testing.T) {
				if err != nil {
					t.Fatalf("a correct split was rejected: %v\n"+
						"the intermediate state — replacement inserted, predecessor still "+
						"open — is exactly what the deferral exists to tolerate", err)
				}
			})
		})
	})

	t.Run("given the same constraint made non-deferrable", func(t *testing.T) {
		t.Run("when the same split is attempted", func(t *testing.T) {
			store := newStore(t, db)
			seedCurrent(ctx, t, db, store.Table(), "old", march, july)
			makeConstraintImmediate(ctx, t, db, store.Table())

			err := splitInsertFirst(ctx, db, store.Table(), "old", "new", march, july)
			t.Run("then an ordinary correct split is rejected mid-write", func(t *testing.T) {
				if err == nil {
					t.Fatal("the split succeeded under a per-statement check, so the deferral " +
						"is not doing the job the requirement says it does")
				}
				if !strings.Contains(err.Error(), "exclusion constraint") {
					t.Fatalf("failed with %v; want an exclusion constraint violation", err)
				}
			})
		})
	})

	t.Run("given an overlapping row injected behind the library's back", func(t *testing.T) {
		store := newStore(t, db)
		l := chronicle.NewLog(store)
		if _, err := l.Put(ctx, "employee", "e1", []byte(`{"grade":"L3"}`), march, july, alice); err != nil {
			t.Fatalf("Put failed: %v", err)
		}

		t.Run("when the transaction that inserts it commits", func(t *testing.T) {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback() }()

			// Straight SQL, bypassing the adapter entirely: nothing in Go can
			// be trusted to be the only writer, so the constraint has to hold
			// against a writer that never heard of chronicle.
			_, err = tx.ExecContext(ctx, `INSERT INTO `+store.Table()+
				` (id, kind, entity_id, data, valid_from, valid_to, tx_from, actor_id, intent)`+
				` VALUES ('injected', 'employee', 'e1', NULL, $1, $2, now(), 'intruder', 0)`,
				april, june)

			t.Run("then the statement itself succeeds, because the check is deferred", func(t *testing.T) {
				if err != nil {
					t.Fatalf("INSERT failed: %v\nwith DEFERRABLE INITIALLY DEFERRED the "+
						"violation must not surface until COMMIT", err)
				}
			})

			commitErr := tx.Commit()
			t.Run("then COMMIT is refused", func(t *testing.T) {
				if commitErr == nil {
					t.Fatal("COMMIT succeeded — two current records now cover the same valid " +
						"instant for one entity, which is the invariant the library exists to hold")
				}
			})
			t.Run("then the refusal names the exclusion constraint", func(t *testing.T) {
				if !strings.Contains(commitErr.Error(), "exclusion constraint") {
					t.Fatalf("COMMIT failed with %v; want an exclusion constraint violation", commitErr)
				}
			})
			t.Run("then nothing was written", func(t *testing.T) {
				recs, _, err := store.Query(ctx, chronicle.Query{Kind: "employee", EntityID: "e1"})
				if err != nil {
					t.Fatalf("Query failed: %v", err)
				}
				for _, r := range recs {
					if r.ID == "injected" {
						t.Fatal("the rejected row is in the table")
					}
				}
			})
		})
	})

	t.Run("given a superseded record overlapping a current one", func(t *testing.T) {
		store := newStore(t, db)
		l := chronicle.NewLog(store)
		if _, err := l.Put(ctx, "employee", "e1", []byte("v1"), march, july, alice); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
		if _, err := l.Put(ctx, "employee", "e1", []byte("v2"), march, july, bob); err != nil {
			t.Fatalf("Put failed: %v", err)
		}

		t.Run("when the history is inspected", func(t *testing.T) {
			recs, _, err := store.Query(ctx, chronicle.Query{Kind: "employee", EntityID: "e1"})
			if err != nil {
				t.Fatalf("Query failed: %v", err)
			}
			t.Run("then both versions are still there", func(t *testing.T) {
				if len(recs) != 2 {
					t.Fatalf("records = %d; want 2 — supersession must not destroy anything", len(recs))
				}
			})
			t.Run("then overlap is only forbidden among current records", func(t *testing.T) {
				// The two records cover exactly the same valid interval. The
				// constraint's WHERE (tx_to IS NULL) is what makes that legal:
				// a bitemporal log is precisely a stack of superseded beliefs
				// about the same stretch of valid time, and a constraint over
				// all rows would forbid the library's whole purpose.
				var current int
				for _, r := range recs {
					if r.IsCurrent() {
						current++
					}
				}
				if current != 1 {
					t.Fatalf("current records = %d; want exactly 1", current)
				}
			})
		})
	})

	t.Run("given a write asserting a zero-width valid interval", func(t *testing.T) {
		store := newStore(t, db)
		t.Run("when it is inserted directly", func(t *testing.T) {
			_, err := db.ExecContext(ctx, `INSERT INTO `+store.Table()+
				` (id, kind, entity_id, valid_from, valid_to, tx_from, actor_id, intent)`+
				` VALUES ('empty', 'employee', 'e1', $1, $1, now(), 'a', 0)`, march)
			t.Run("then the check constraint rejects it", func(t *testing.T) {
				// An empty range overlaps nothing, so the exclusion constraint
				// would let it through, and it would then sit in the log
				// asserting nothing at all.
				if err == nil {
					t.Fatal("an empty valid interval was accepted")
				}
				if !strings.Contains(err.Error(), "valid_nonempty") {
					t.Fatalf("rejected with %v; want the valid_nonempty check", err)
				}
			})
		})
	})
}

// countOverlaps asks the database directly how many pairs of current records
// cover the same valid instant for one entity.
//
// Asking SQL rather than the library is deliberate: a bug in the library's own
// reading of its records would otherwise be able to hide the very corruption
// this is looking for.
func countOverlaps(ctx context.Context, t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var overlaps int
	err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM `+table+` a JOIN `+table+` b
		  ON a.kind = b.kind AND a.entity_id = b.entity_id AND a.id < b.id
		 AND a.tx_to IS NULL AND b.tx_to IS NULL AND a.valid && b.valid`).Scan(&overlaps)
	if err != nil {
		t.Fatalf("overlap check failed: %v", err)
	}
	return overlaps
}

// seedCurrent inserts one current record straight into the table.
func seedCurrent(ctx context.Context, t *testing.T, db *sql.DB, table, id string, from, to time.Time) {
	t.Helper()
	_, err := db.ExecContext(ctx, `INSERT INTO `+table+
		` (id, kind, entity_id, valid_from, valid_to, tx_from, actor_id, intent)`+
		` VALUES ($1, 'employee', 'e1', $2, $3, clock_timestamp(), 'seed', 0)`, id, from, to)
	if err != nil {
		t.Fatalf("seeding %s: %v", id, err)
	}
}

// splitInsertFirst performs a supersede-and-replace in the order DESIGN.md
// describes: the replacement lands while its predecessor is still open, so the
// entity momentarily has two current records over the same interval.
func splitInsertFirst(ctx context.Context, db *sql.DB, table, oldID, newID string, from, to time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `INSERT INTO `+table+
		` (id, kind, entity_id, valid_from, valid_to, tx_from, actor_id, intent)`+
		` VALUES ($1, 'employee', 'e1', $2, $3, clock_timestamp(), 'writer', 0)`,
		newID, from, to); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET tx_to = clock_timestamp() WHERE id = $1`, oldID); err != nil {
		return err
	}
	return tx.Commit()
}

// makeConstraintImmediate replaces the exclusion constraint with a
// per-statement one, so that a test can show what the deferral was preventing.
func makeConstraintImmediate(ctx context.Context, t *testing.T, db *sql.DB, table string) {
	t.Helper()
	name := constraintName(table)
	for _, stmt := range []string{
		`ALTER TABLE ` + table + ` DROP CONSTRAINT ` + name,
		`ALTER TABLE ` + table + ` ADD CONSTRAINT ` + name +
			` EXCLUDE USING gist (kind WITH =, entity_id WITH =, valid WITH &&) WHERE (tx_to IS NULL)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("reshaping the constraint: %v", err)
		}
	}
}

// constraintName renders the exclusion constraint's name for a quoted,
// schema-qualified table. The schema qualifies the constraint's table, not the
// constraint, so only the table part carries into the name.
func constraintName(qualifiedTable string) string {
	parts := strings.Split(qualifiedTable, ".")
	bare := strings.Trim(parts[len(parts)-1], `"`)
	return `"` + bare + `_no_overlap"`
}
